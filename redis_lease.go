// FILENAME: redis_lease.go
package gothrottle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// Redis lease model
//
// Four keys per limiter, all sharing one Redis Cluster hash tag derived from the
// limiter ID (see limiterKeyTag):
//
//	gothrottle:{<tag>}:leases        HASH    token -> weight
//	gothrottle:{<tag>}:expirations   ZSET    token -> expiry (microseconds)
//	gothrottle:{<tag>}:last-start    STRING  microseconds of the last admission
//	gothrottle:{<tag>}:config        HASH    id, max_concurrent, min_time_us, lease_ttl_us
//
// Every script reads the clock with Redis TIME, so no participant's local clock
// affects the outcome. Running weight is summed from the surviving leases rather
// than kept in a counter, which is why a late release cannot corrupt the total:
// it removes one specific token or nothing at all.
//
// Spacing history is deliberately a separate key from the reservations. MinTime
// is measured from when a job *started*, so it has to outlive the lease that
// started it — including when that lease expires because its holder crashed.
// Reclaiming an expired lease therefore purges the reservation keys only, and
// last-start is written on admission and never touched by renewal, release or
// reclamation. Its own lifetime is derived from MinTime alone.
//
// Key expiry is garbage collection for an idle limiter, never correctness:
// admission is decided by the per-token expiry scores and the last-start value.
// TTLs are only ever extended (see the ensure_ttl helper in each script), so a
// short-lived operation cannot shorten a window that protects a longer one.

const (
	// redisKeyPrefix namespaces every key this package creates.
	redisKeyPrefix = "gothrottle:"
)

// leaseTTLHelper is the TTL discipline shared by every lease script: extend a
// key's expiry, never shorten it, and never resurrect one that is already gone.
const leaseTTLHelper = `
local function ensure_ttl(key, ttl_ms)
    if ttl_ms <= 0 then
        return
    end
    local current = redis.call("PTTL", key)
    if current == -2 then
        return
    end
    if current == -1 or current < ttl_ms then
        redis.call("PEXPIRE", key, ttl_ms)
    end
end
`

// redisAcquireScript checks configuration agreement, reclaims expired leases,
// then admits or refuses the request.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 last-start string, 4 config hash
// ARGV:  1 max_concurrent, 2 min_time_us, 3 weight, 4 token, 5 lease_ttl_us,
//
//	6 lease_key_ttl_ms, 7 start_key_ttl_ms, 8 config_key_ttl_ms, 9 limiter_id
//
// Reply: {1, expiry_us, 0}                             admitted
//
//	{0, 0, -1}                                    refused, capacity held
//	{0, 0, wait_us}                               refused, retry after wait_us
//	{-1, max, min_time_us, lease_ttl_us, id}      configuration mismatch
const redisAcquireScript = leaseTTLHelper + `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local start_key = KEYS[3]
local config_key = KEYS[4]

local max_concurrent = tonumber(ARGV[1])
local min_time_us = tonumber(ARGV[2])
local weight = tonumber(ARGV[3])
local token = ARGV[4]
local lease_ttl_us = tonumber(ARGV[5])
local lease_key_ttl_ms = tonumber(ARGV[6])
local start_key_ttl_ms = tonumber(ARGV[7])
local config_key_ttl_ms = tonumber(ARGV[8])
local limiter_id = ARGV[9]

-- Configuration agreement is settled before anything is written, so a client
-- that disagrees cannot disturb the leases, TTLs or spacing state it was
-- rejected over. The raw ARGV strings are stored and compared numerically:
-- exact decimal integers in, exact decimal integers out.
local stored = redis.call("HMGET", config_key, "max_concurrent", "min_time_us", "lease_ttl_us", "id")
if stored[1] then
    if tonumber(stored[1]) ~= max_concurrent
        or tonumber(stored[2]) ~= min_time_us
        or tonumber(stored[3]) ~= lease_ttl_us
        or stored[4] ~= limiter_id then
        return {-1, stored[1], stored[2] or "0", stored[3] or "0", stored[4] or ""}
    end
else
    redis.call("HSET", config_key,
        "max_concurrent", ARGV[1],
        "min_time_us", ARGV[2],
        "lease_ttl_us", ARGV[5],
        "id", limiter_id)
end
ensure_ttl(config_key, config_key_ttl_ms)

local time = redis.call("TIME")
local now_us = (tonumber(time[1]) * 1000000) + tonumber(time[2])

-- Reclaim reservations whose holders are gone. Only reservation state is
-- purged: spacing history lives in its own key precisely so that a crashed
-- holder's expiry cannot hand the next job a free start.
--
-- Reclaiming in bounded batches keeps unpack() well inside Lua's argument
-- limits however much lease state has accumulated.
while true do
    local expired = redis.call("ZRANGEBYSCORE", exp_key, "-inf", now_us, "LIMIT", 0, 256)
    if #expired == 0 then
        break
    end
    redis.call("ZREM", exp_key, unpack(expired))
    redis.call("HDEL", lease_key, unpack(expired))
    if #expired < 256 then
        break
    end
end

-- Running weight is summed from the live leases, so it cannot drift out of step
-- with the reservations it represents. The reclaim loop above has just removed
-- everything lapsed, and the hash and the expiry set are always written
-- together, so the hash holds exactly the live leases — bounded by
-- max_concurrent, since each weighs at least 1.
--
-- The length comparison reconciles state this package did not write: after a
-- partial restore or manual surgery, a hash field could outlive its expiry entry
-- and hold capacity forever. It costs one comparison when nothing is wrong.
if max_concurrent > 0 then
    if redis.call("HLEN", lease_key) ~= redis.call("ZCARD", exp_key) then
        local tracked = {}
        local live = redis.call("ZRANGE", exp_key, 0, -1)
        for i = 1, #live do
            tracked[live[i]] = true
        end
        local fields = redis.call("HKEYS", lease_key)
        for i = 1, #fields do
            if not tracked[fields[i]] then
                redis.call("HDEL", lease_key, fields[i])
            end
        end
    end

    local running = 0
    local held = redis.call("HGETALL", lease_key)
    for i = 2, #held, 2 do
        running = running + tonumber(held[i])
    end
    if running + weight > max_concurrent then
        return {0, 0, -1}
    end
end

if min_time_us > 0 then
    local last_start_us = tonumber(redis.call("GET", start_key) or "0")
    if last_start_us > 0 then
        local elapsed = now_us - last_start_us
        if elapsed < min_time_us then
            local wait_us = min_time_us - elapsed
            -- A clock that moved backwards must not inflate the wait beyond the
            -- window itself.
            if wait_us > min_time_us then
                wait_us = min_time_us
            end
            return {0, 0, wait_us}
        end
    end
end

local expires_us = now_us + lease_ttl_us
redis.call("HSET", lease_key, token, weight)
redis.call("ZADD", exp_key, expires_us, token)
ensure_ttl(lease_key, lease_key_ttl_ms)
ensure_ttl(exp_key, lease_key_ttl_ms)

-- Only a successful admission moves the spacing window, and only MinTime
-- decides how long the record survives.
if min_time_us > 0 and start_key_ttl_ms > 0 then
    redis.call("SET", start_key, now_us, "PX", start_key_ttl_ms)
end

return {1, expires_us, 0}
`

// leaseConfigTTLHelper derives how long the configuration record must outlive
// the operation at hand. Renewal and release only know the lease's own TTL, so
// the spacing component is read back from the stored configuration rather than
// guessed — the record has to outlast both live leases and the active MinTime
// window, otherwise a different configuration could be registered while work is
// still running.
const leaseConfigTTLHelper = `
local function config_ttl_ms(config_key, lease_key_ttl_ms)
    local min_time_us = tonumber(redis.call("HGET", config_key, "min_time_us") or "0")
    local spacing_ttl_ms = math.ceil((min_time_us * 2) / 1000)
    if spacing_ttl_ms > lease_key_ttl_ms then
        return spacing_ttl_ms
    end
    return lease_key_ttl_ms
end
`

// redisRenewScript extends one lease, and only while it is still held.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 config hash
// ARGV:  1 token, 2 lease_ttl_us, 3 lease_key_ttl_ms
// Reply: new expiry in microseconds, or 0 when the lease is gone.
const redisRenewScript = leaseTTLHelper + leaseConfigTTLHelper + `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local config_key = KEYS[3]

local token = ARGV[1]
local lease_ttl_us = tonumber(ARGV[2])
local lease_key_ttl_ms = tonumber(ARGV[3])

local time = redis.call("TIME")
local now_us = (tonumber(time[1]) * 1000000) + tonumber(time[2])

-- A lapsed reservation is not renewable: the store may already have handed the
-- capacity to another holder, and reviving it would put both over the limit.
local expiry = redis.call("ZSCORE", exp_key, token)
if not expiry or tonumber(expiry) <= now_us then
    redis.call("ZREM", exp_key, token)
    redis.call("HDEL", lease_key, token)
    return 0
end

local expires_us = now_us + lease_ttl_us
redis.call("ZADD", exp_key, expires_us, token)
ensure_ttl(lease_key, lease_key_ttl_ms)
ensure_ttl(exp_key, lease_key_ttl_ms)
ensure_ttl(config_key, config_ttl_ms(config_key, lease_key_ttl_ms))

return expires_us
`

// redisReleaseScript removes one lease by token.
//
// The last-start record is deliberately untouched: MinTime is measured from
// when a job started, so forgetting it when the job finishes would let the next
// job start immediately.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 config hash
// ARGV:  1 token, 2 lease_key_ttl_ms
// Reply: 1 if the lease was held, 0 if it had already been reclaimed.
const redisReleaseScript = leaseTTLHelper + leaseConfigTTLHelper + `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local config_key = KEYS[3]

local token = ARGV[1]
local lease_key_ttl_ms = tonumber(ARGV[2])

local removed = redis.call("HDEL", lease_key, token)
redis.call("ZREM", exp_key, token)

ensure_ttl(lease_key, lease_key_ttl_ms)
ensure_ttl(exp_key, lease_key_ttl_ms)
ensure_ttl(config_key, config_ttl_ms(config_key, lease_key_ttl_ms))

return removed
`

// Lease script SHAs, computed once. EvalSha keeps the script off the wire on
// every call after the first, and hashing a few kilobytes per acquisition would
// be pointless work on the hot path.
var (
	acquireScriptSHA = scriptSHA(redisAcquireScript)
	renewScriptSHA   = scriptSHA(redisRenewScript)
	releaseScriptSHA = scriptSHA(redisReleaseScript)
)

// limiterKeys are the Redis keys backing one limiter's leases. Key naming is
// centralized here so every script call for a limiter addresses the same keys.
type limiterKeys struct {
	leases      string
	expirations string
	lastStart   string
	config      string
}

// newLimiterKeys derives a limiter's keys from its ID.
func newLimiterKeys(limiterID string) limiterKeys {
	base := redisKeyPrefix + "{" + limiterKeyTag(limiterID) + "}:"
	return limiterKeys{
		leases:      base + "leases",
		expirations: base + "expirations",
		lastStart:   base + "last-start",
		config:      base + "config",
	}
}

// admission returns the key list for the acquire script.
func (k limiterKeys) admission() []string {
	return []string{k.leases, k.expirations, k.lastStart, k.config}
}

// reservation returns the key list for the renew and release scripts, which
// must not touch the spacing record.
func (k limiterKeys) reservation() []string {
	return []string{k.leases, k.expirations, k.config}
}

// RedisKeyLayout names the Redis keys one limiter ID's lease state occupies. It
// is exported for operational use — inspecting live state, or clearing a limiter
// that will never run again — not because the layout is part of the throttling
// contract.
type RedisKeyLayout struct {
	// Leases is a hash of lease token to reserved weight.
	Leases string
	// Expirations is a sorted set of lease token to expiry, in microseconds of
	// Redis server time.
	Expirations string
	// LastStart holds the microsecond timestamp of the most recent admission,
	// which is what MinTime spacing is measured from. It is written only on a
	// successful acquisition and is absent when MinTime is zero.
	LastStart string
	// Config records the MaxConcurrent, MinTime and LeaseTTL every instance
	// sharing this ID must agree on.
	Config string
}

// RedisKeys returns the keys RedisStore uses for a limiter ID.
//
// All four share one Redis Cluster hash tag so the multi-key Lua scripts stay
// within a single hash slot. The tag is a hash of the limiter ID rather than the
// ID itself, because an ID containing braces would otherwise choose its own tag.
func RedisKeys(limiterID string) RedisKeyLayout {
	keys := newLimiterKeys(limiterID)
	return RedisKeyLayout{
		Leases:      keys.leases,
		Expirations: keys.expirations,
		LastStart:   keys.lastStart,
		Config:      keys.config,
	}
}

// limiterKeyTag derives the Redis Cluster hash tag for a limiter ID.
//
// Multi-key Lua requires every key to live in one hash slot, and Redis picks
// the slot from the substring inside the first {...}. The raw ID cannot go
// there: an ID containing braces would choose its own tag, so two IDs could
// collide or one could be steered onto an arbitrary slot. Hashing yields a
// fixed-length tag with no braces, the same for a given ID on every instance
// and different for different IDs in any practical population. Tag collisions
// are still caught rather than silently tolerated — the limiter ID is stored in
// the config hash and compared on every acquisition.
func limiterKeyTag(limiterID string) string {
	sum := sha256.Sum256([]byte(limiterID))
	return hex.EncodeToString(sum[:16])
}

// Acquire reserves capacity and returns a renewable lease. It implements
// LeaseDatastore.
//
// Every instance sharing a limiter ID must supply the same MaxConcurrent,
// MinTime and LeaseTTL. The first acquisition records them; a later one that
// disagrees is refused with an error matching ErrLimiterConfigMismatch rather
// than silently applying whichever policy arrived last.
func (rs *RedisStore) Acquire(ctx context.Context, limiterID string, weight int, opts Options) (*Lease, time.Duration, error) {
	if limiterID == "" {
		return nil, 0, ErrMissingID
	}
	if err := validateLimiterID(limiterID); err != nil {
		return nil, 0, err
	}
	if weight <= 0 {
		return nil, 0, ErrInvalidWeight
	}
	if opts.MaxConcurrent > 0 && weight > opts.MaxConcurrent {
		return nil, 0, ErrWeightExceedsMax
	}

	token, err := newLeaseToken()
	if err != nil {
		return nil, 0, err
	}

	ttl := opts.leaseTTL()
	config := opts.leaseConfig()
	reply, err := rs.evalLeaseScript(ctx, redisAcquireScript, acquireScriptSHA, newLimiterKeys(limiterID).admission(), []interface{}{
		config.maxConcurrent,
		config.minTimeUS,
		weight,
		token,
		config.leaseTTLUS,
		leaseStateWindow(ttl).Milliseconds(),
		spacingStateWindow(opts.MinTime).Milliseconds(),
		configLifetime(ttl, opts.MinTime).Milliseconds(),
		limiterID,
	})
	if err != nil {
		return nil, 0, err
	}

	values, ok := reply.([]interface{})
	if !ok || len(values) < 3 {
		return nil, 0, fmt.Errorf("unexpected redis acquire result format")
	}

	granted, err := toInt64(values[0])
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
	}
	if granted < 0 {
		stored, storedID, err := storedLeaseConfig(values)
		if err != nil {
			return nil, 0, err
		}
		return nil, 0, configMismatchError(limiterID, config, stored, storedID)
	}

	expiresUS, err := toInt64(values[1])
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
	}
	waitUS, err := toInt64(values[2])
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
	}

	if granted != 1 {
		var retryAfter time.Duration
		if waitUS > 0 {
			retryAfter = time.Duration(waitUS) * time.Microsecond
		}
		return nil, retryAfter, nil
	}

	return &Lease{
		Token:     token,
		LimiterID: limiterID,
		Weight:    weight,
		TTL:       ttl,
		ExpiresAt: time.Unix(0, expiresUS*int64(time.Microsecond)),
	}, 0, nil
}

// storedLeaseConfig reads the configuration a mismatch reply carries, along with
// the limiter ID it was registered under.
func storedLeaseConfig(values []interface{}) (leaseConfig, string, error) {
	if len(values) < 5 {
		return leaseConfig{}, "", fmt.Errorf("unexpected redis acquire mismatch result format")
	}

	maxConcurrent, err := toInt64(values[1])
	if err != nil {
		return leaseConfig{}, "", fmt.Errorf("unexpected redis acquire mismatch result: %w", err)
	}
	minTimeUS, err := toInt64(values[2])
	if err != nil {
		return leaseConfig{}, "", fmt.Errorf("unexpected redis acquire mismatch result: %w", err)
	}
	leaseTTLUS, err := toInt64(values[3])
	if err != nil {
		return leaseConfig{}, "", fmt.Errorf("unexpected redis acquire mismatch result: %w", err)
	}
	storedID, _ := values[4].(string)

	return leaseConfig{
		maxConcurrent: int(maxConcurrent),
		minTimeUS:     minTimeUS,
		leaseTTLUS:    leaseTTLUS,
	}, storedID, nil
}

// Renew extends a lease. It implements LeaseDatastore.
func (rs *RedisStore) Renew(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return ErrNilLease
	}
	if err := validateLimiterID(lease.LimiterID); err != nil {
		return err
	}

	// Reuse the TTL the lease was created with: renewal extends the window, it
	// does not redefine it.
	ttl := lease.ttlOrDefault()

	reply, err := rs.evalLeaseScript(ctx, redisRenewScript, renewScriptSHA, newLimiterKeys(lease.LimiterID).reservation(), []interface{}{
		lease.Token,
		ttl.Microseconds(),
		leaseStateWindow(ttl).Milliseconds(),
	})
	if err != nil {
		return err
	}

	expiresUS, err := toInt64(reply)
	if err != nil {
		return fmt.Errorf("unexpected redis renew result: %w", err)
	}
	if expiresUS == 0 {
		return ErrLeaseLost
	}

	lease.ExpiresAt = time.Unix(0, expiresUS*int64(time.Microsecond))
	return nil
}

// Release returns a lease's capacity. It implements LeaseDatastore.
func (rs *RedisStore) Release(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return ErrNilLease
	}
	if err := validateLimiterID(lease.LimiterID); err != nil {
		return err
	}

	// A release of an already-reclaimed lease is not an error: the store has
	// moved on, and only this token is touched, so a newer holder's reservation
	// is untouched either way.
	ttl := lease.ttlOrDefault()
	_, err := rs.evalLeaseScript(ctx, redisReleaseScript, releaseScriptSHA, newLimiterKeys(lease.LimiterID).reservation(), []interface{}{
		lease.Token,
		leaseStateWindow(ttl).Milliseconds(),
	})
	return err
}

// evalLeaseScript runs a lease script, falling back to EVAL if Redis has
// forgotten it. EVAL both executes and caches the script on the node that
// serves it, which is what makes the fallback work on a Cluster or Ring client
// where SCRIPT LOAD would have reached an arbitrary node.
func (rs *RedisStore) evalLeaseScript(ctx context.Context, script, sha string, keys []string, args []interface{}) (interface{}, error) {
	rs.mu.RLock()
	client := rs.client
	storeCtx := rs.ctx
	rs.mu.RUnlock()
	if client == nil {
		return nil, ErrStoreClosed
	}

	if ctx == nil {
		ctx = storeCtx
	}

	reply, err := client.EvalSha(ctx, sha, keys, args...).Result()
	if isRedisNoScript(err) {
		reply, err = client.Eval(ctx, script, keys, args...).Result()
	}
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("redis eval error: %w", err)
	}

	return reply, nil
}

// toInt64 normalizes the integer replies Redis returns from Lua.
func toInt64(value interface{}) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil {
			return 0, fmt.Errorf("value %q is not an integer", v)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("value of type %T is not an integer", value)
	}
}
