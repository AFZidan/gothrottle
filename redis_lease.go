// FILENAME: redis_lease.go
package gothrottle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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

// redisEnsureTTLHelper is the TTL discipline every script in this package
// shares: extend a key's expiry, never shorten it, and never resurrect one that
// is already gone. Both the lease scripts and the legacy counter scripts include
// it, because both had the same defect — an operation that knows only its own
// short window must not cut short a key protecting a longer one.
const redisEnsureTTLHelper = `
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

// redisReconcileHelper computes a limiter's running weight and, on the way,
// repairs reservation records that the normal expiry path can never reclaim.
//
// The expiry ZSET is authoritative for liveness, so the sum walks it rather than
// the lease hash. That inverts the older failure: a weight in the hash with no
// expiry entry is simply never counted, instead of consuming capacity forever.
// Removing the expiry entries that have no weight then leaves the ZSET a subset
// of the hash, which is what makes the cardinality comparison exact — comparing
// counts alone was the original defect, because a hash holding token A and a ZSET
// holding token B both have one member and nothing was reconciled.
//
// Running weight is computed deterministically using read-only ZRANGE rank
// pagination (at most reconcile_batch = 256 member names per ZRANGE call). This
// avoids relying on ZSCAN, whose documented SCAN contract permits duplicate
// elements under dictionary rehash (which Redis 6.2+ and 7.x can trigger even
// during read lookups).
//
// The deletion repair passes (prune_weightless, prune_untracked_fields) may
// continue using ZSCAN/HSCAN because duplicate returned elements are harmless for
// idempotent ZREM and HDEL operations. Redis guarantees that an element present
// throughout a full iteration is returned at least once.
//
// Note on SCAN memory and COUNT hints: COUNT 256 in ZSCAN/HSCAN is only a hint to
// Redis, not a hard response bound. Compactly encoded hashes or sorted sets ignore
// COUNT and return all elements in a single reply. Member lists passed to HMGET,
// ZREM, and HDEL are explicitly re-chunked to at most reconcile_batch (256)
// arguments so unpack() never exceeds Lua's stack limits regardless of collection
// size or encoding.
const redisReconcileHelper = `
local reconcile_batch = 256

-- deleter accumulates members and flushes in bounded batches, so unpack() never
-- receives more than reconcile_batch arguments however much state is being
-- repaired.
local function new_deleter(cmd, key)
    return {cmd = cmd, key = key, buf = {}}
end

local function deleter_flush(d)
    if #d.buf == 0 then
        return
    end
    redis.call(d.cmd, d.key, unpack(d.buf))
    d.buf = {}
end

local function deleter_add(d, member)
    d.buf[#d.buf + 1] = member
    if #d.buf >= reconcile_batch then
        deleter_flush(d)
    end
end

-- sum_live_weight totals the weight of every token that has one, and counts the
-- tokens with and without. It writes nothing: exactness depends on deterministic,
-- read-only ZRANGE rank pagination over the expiry ZSET (at most reconcile_batch
-- members per rank page), fetching weights via one HMGET per page.
local function sum_live_weight(lease_key, exp_key)
    local running, live, weightless = 0, 0, 0
    local start = 0

    repeat
        local members = redis.call("ZRANGE", exp_key, start, start + reconcile_batch - 1)
        if #members == 0 then
            break
        end

        local weights = redis.call("HMGET", lease_key, unpack(members))
        for j = 1, #members do
            local weight = tonumber(weights[j])
            if weight and weight > 0 then
                running = running + weight
                live = live + 1
            else
                weightless = weightless + 1
            end
        end

        start = start + #members
    until #members < reconcile_batch

    return running, live, weightless
end

-- prune_weightless drops expiry entries with no lease weight using ZSCAN.
-- Duplicate returned elements are harmless because ZREM and HDEL are idempotent.
-- COUNT is a hint to Redis; compactly encoded collections may return all elements
-- in one scan reply.
local function prune_weightless(lease_key, exp_key)
    local zrem = new_deleter("ZREM", exp_key)
    local hdel = new_deleter("HDEL", lease_key)
    local cursor = "0"

    repeat
        local reply = redis.call("ZSCAN", exp_key, cursor, "COUNT", reconcile_batch)
        cursor = reply[1]
        local flat = reply[2]
        for i = 1, #flat, 2 do
            local weight = tonumber(redis.call("HGET", lease_key, flat[i]))
            if not weight or weight <= 0 then
                deleter_add(zrem, flat[i])
                deleter_add(hdel, flat[i])
            end
        end
    until cursor == "0"

    deleter_flush(zrem)
    deleter_flush(hdel)
end

-- prune_untracked_fields removes lease-hash fields with no expiry entry using HSCAN.
-- Duplicate returned fields are harmless because HDEL is idempotent. COUNT is a
-- hint; compactly encoded hashes may return all fields in a single reply.
local function prune_untracked_fields(lease_key, exp_key)
    local hdel = new_deleter("HDEL", lease_key)
    local cursor = "0"

    repeat
        local reply = redis.call("HSCAN", lease_key, cursor, "COUNT", reconcile_batch)
        cursor = reply[1]
        local flat = reply[2]
        for i = 1, #flat, 2 do
            if not redis.call("ZSCORE", exp_key, flat[i]) then
                deleter_add(hdel, flat[i])
            end
        end
    until cursor == "0"

    deleter_flush(hdel)
end

-- running_weight is the limiter's committed weight, repairing orphaned records on
-- the way. The sum is taken before any repair, and repairs only remove records
-- that contributed nothing to it, so the value stays exact.
local function running_weight(lease_key, exp_key)
    local running, live, weightless = sum_live_weight(lease_key, exp_key)

    if weightless > 0 then
        prune_weightless(lease_key, exp_key)
    end

    -- Every surviving expiry entry has a weight, so the ZSET is a subset of the
    -- hash and a count mismatch means the hash holds fields the ZSET does not.
    -- Comparing against the counted live entries is exact, which HLEN vs ZCARD
    -- was not: there, a hash holding token A and a ZSET holding token B both
    -- counted one and nothing was repaired.
    if redis.call("HLEN", lease_key) ~= live then
        prune_untracked_fields(lease_key, exp_key)
    end

    return running
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
const redisAcquireScript = redisEnsureTTLHelper + redisReconcileHelper + `
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
-- with the reservations it represents. See redisReconcileHelper for how orphaned
-- records are reconciled while summing.
--
-- The comparison is subtraction rather than "running + weight > max_concurrent",
-- matching the Go side: both operands are individually within Lua's exact range,
-- but their sum need not be, and a sum that rounds turns a refusal into an
-- admission. max_concurrent - running is bounded by max_concurrent, so it stays
-- exact.
if max_concurrent > 0 then
    local running = running_weight(lease_key, exp_key)
    if running >= max_concurrent or weight > max_concurrent - running then
        return {0, 0, -1}
    end
elseif redis.call("HLEN", lease_key) ~= redis.call("ZCARD", exp_key) then
    -- With no concurrency limit there is no capacity for an orphan to hold, so
    -- one costs memory rather than correctness. Reconcile only when the counts
    -- disagree: an unlimited limiter must not pay for a membership walk on every
    -- admission.
    running_weight(lease_key, exp_key)
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
const redisRenewScript = redisEnsureTTLHelper + leaseConfigTTLHelper + `
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
const redisReleaseScript = redisEnsureTTLHelper + leaseConfigTTLHelper + `
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
	if err := validateAdmission(limiterID, weight, opts); err != nil {
		return nil, 0, err
	}

	token, err := newLeaseToken()
	if err != nil {
		return nil, 0, err
	}

	ttl := opts.leaseTTL()
	config := opts.leaseConfig()

	// The three key TTLs are derived here rather than supplied, so validation has
	// not seen them. Each is bounded in its own right before it reaches Lua.
	leaseTTLMS, err := luaMillis("lease key TTL in milliseconds", leaseStateWindow(ttl))
	if err != nil {
		return nil, 0, err
	}
	startTTLMS, err := luaMillis("spacing key TTL in milliseconds", spacingStateWindow(opts.MinTime))
	if err != nil {
		return nil, 0, err
	}
	configTTLMS, err := luaMillis("config key TTL in milliseconds", configLifetime(ttl, opts.MinTime))
	if err != nil {
		return nil, 0, err
	}

	reply, err := rs.evalLeaseScript(ctx, redisAcquireScript, acquireScriptSHA, newLimiterKeys(limiterID).admission(), []interface{}{
		config.maxConcurrent,
		config.minTimeUS,
		weight,
		token,
		config.leaseTTLUS,
		leaseTTLMS,
		startTTLMS,
		configTTLMS,
		limiterID,
	})
	if err != nil {
		return nil, 0, err
	}

	return parseAcquireReply(reply, limiterID, weight, token, ttl, config)
}

// parseAcquireReply decodes the acquire script's reply: an admission with the
// new lease, a refusal with an optional bounded wait, or a configuration
// mismatch naming the registered configuration.
func parseAcquireReply(reply interface{}, limiterID string, weight int, token string, ttl time.Duration, requested leaseConfig) (*Lease, time.Duration, error) {
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
		return nil, 0, configMismatchError(limiterID, requested, stored, storedID)
	}

	if granted != 1 {
		waitUS, err := toInt64(values[2])
		if err != nil {
			return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
		}
		// Clamped on conversion: a nonsense reply must not wrap into a negative
		// Duration, which the scheduler reads as "no deadline" and stops waiting on.
		return nil, microsToDuration(waitUS), nil
	}

	expiresUS, err := toInt64(values[1])
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
	}

	return &Lease{
		Token:     token,
		LimiterID: limiterID,
		Weight:    weight,
		TTL:       ttl,
		ExpiresAt: microsToTime(expiresUS),
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
		maxConcurrent: maxConcurrent,
		minTimeUS:     minTimeUS,
		leaseTTLUS:    leaseTTLUS,
	}, storedID, nil
}

// Renew extends a lease. It implements LeaseDatastore.
//
// The lease is caller-supplied and no validator has seen it — a Lease can be
// constructed literally, or restored from storage — so the TTL derived from it is
// bounded here before it reaches Lua.
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
	ttlUS, err := luaMicros("Lease.TTL in microseconds", ttl)
	if err != nil {
		return err
	}
	stateTTLMS, err := luaMillis("lease key TTL in milliseconds", leaseStateWindow(ttl))
	if err != nil {
		return err
	}

	reply, err := rs.evalLeaseScript(ctx, redisRenewScript, renewScriptSHA, newLimiterKeys(lease.LimiterID).reservation(), []interface{}{
		lease.Token,
		ttlUS,
		stateTTLMS,
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

	lease.ExpiresAt = microsToTime(expiresUS)
	return nil
}

// Release returns a lease's capacity. It implements LeaseDatastore.
//
// Like Renew, the lease is caller-supplied, so its derived TTL is bounded before
// it reaches Lua.
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
	stateTTLMS, err := luaMillis("lease key TTL in milliseconds", leaseStateWindow(lease.ttlOrDefault()))
	if err != nil {
		return err
	}

	_, err = rs.evalLeaseScript(ctx, redisReleaseScript, releaseScriptSHA, newLimiterKeys(lease.LimiterID).reservation(), []interface{}{
		lease.Token,
		stateTTLMS,
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
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("value %q is not an integer", v)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("value of type %T is not an integer", value)
	}
}
