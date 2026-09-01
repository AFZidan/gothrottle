// FILENAME: redis_lease.go
package gothrottle

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// Redis lease model
//
// Three keys per limiter, all named from the limiter ID:
//
//	gothrottle:lease:<id>   HASH  token -> weight
//	gothrottle:exp:<id>     ZSET  token -> expiry (microseconds)
//	gothrottle:start:<id>   ZSET  token -> start   (microseconds)
//
// Every script reads the clock with Redis TIME, so no participant's local clock
// affects the outcome, and purges expired tokens from all three keys before
// making a decision. Running weight is the sum of the surviving leases rather
// than a counter, which is why a late release cannot corrupt the total: it
// removes one specific token or nothing at all.
//
// The keys carry a TTL of twice the lease TTL purely as garbage collection for
// an idle limiter. Correctness comes from the per-token expiry scores, not from
// key expiry, so unlike the counter model a job outliving a TTL cannot cause
// another job to start over the limit.

const (
	leaseKeyPrefix = "gothrottle:lease:"
	expKeyPrefix   = "gothrottle:exp:"
	startKeyPrefix = "gothrottle:start:"
)

// redisAcquireScript purges expired leases, then admits or refuses the request.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 start zset
// ARGV:  1 max_concurrent, 2 min_time_us, 3 weight, 4 token, 5 ttl_us, 6 key_ttl_ms
// Reply: {1, expiry_us, 0}          admitted
//
//	{0, 0, -1}                 refused, capacity held (no deadline)
//	{0, 0, wait_us}            refused, retry after wait_us
const redisAcquireScript = `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local start_key = KEYS[3]

local max_concurrent = tonumber(ARGV[1])
local min_time_us = tonumber(ARGV[2])
local weight = tonumber(ARGV[3])
local token = ARGV[4]
local ttl_us = tonumber(ARGV[5])
local key_ttl_ms = tonumber(ARGV[6])

local time = redis.call("TIME")
local now_us = (tonumber(time[1]) * 1000000) + tonumber(time[2])

-- Reclaim leases whose holders are gone.
local expired = redis.call("ZRANGEBYSCORE", exp_key, "-inf", now_us)
if #expired > 0 then
    redis.call("ZREM", exp_key, unpack(expired))
    redis.call("ZREM", start_key, unpack(expired))
    redis.call("HDEL", lease_key, unpack(expired))
end

-- Running weight is the sum of live leases, so it cannot drift out of step with
-- the reservations it represents.
local running = 0
if max_concurrent > 0 then
    local live = redis.call("ZRANGE", exp_key, 0, -1)
    for i = 1, #live do
        running = running + tonumber(redis.call("HGET", lease_key, live[i]) or "0")
    end
    if running + weight > max_concurrent then
        return {0, 0, -1}
    end
end

if min_time_us > 0 then
    local last = redis.call("ZREVRANGE", start_key, 0, 0, "WITHSCORES")
    if #last >= 2 then
        local elapsed = now_us - tonumber(last[2])
        if elapsed < min_time_us then
            return {0, 0, min_time_us - elapsed}
        end
    end
end

local expires_us = now_us + ttl_us
redis.call("HSET", lease_key, token, weight)
redis.call("ZADD", exp_key, expires_us, token)
redis.call("ZADD", start_key, now_us, token)

redis.call("PEXPIRE", lease_key, key_ttl_ms)
redis.call("PEXPIRE", exp_key, key_ttl_ms)
redis.call("PEXPIRE", start_key, key_ttl_ms)

return {1, expires_us, 0}
`

// redisRenewScript extends one lease, and only if it is still held.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 start zset
// ARGV:  1 token, 2 ttl_us, 3 key_ttl_ms
// Reply: new expiry in microseconds, or 0 when the lease is gone.
const redisRenewScript = `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local start_key = KEYS[3]

local token = ARGV[1]
local ttl_us = tonumber(ARGV[2])
local key_ttl_ms = tonumber(ARGV[3])

if redis.call("HEXISTS", lease_key, token) == 0 then
    return 0
end

local time = redis.call("TIME")
local now_us = (tonumber(time[1]) * 1000000) + tonumber(time[2])
local expires_us = now_us + ttl_us

redis.call("ZADD", exp_key, expires_us, token)
redis.call("PEXPIRE", lease_key, key_ttl_ms)
redis.call("PEXPIRE", exp_key, key_ttl_ms)
redis.call("PEXPIRE", start_key, key_ttl_ms)

return expires_us
`

// redisReleaseScript removes one lease by token.
//
// The start score is deliberately kept: MinTime spacing is measured from when a
// job started, so forgetting it when the job finishes would let the next job
// start immediately.
//
// KEYS:  1 lease hash, 2 expiry zset, 3 start zset
// ARGV:  1 token, 2 key_ttl_ms
// Reply: 1 if the lease was held, 0 if it had already been reclaimed.
const redisReleaseScript = `
local lease_key = KEYS[1]
local exp_key = KEYS[2]
local start_key = KEYS[3]

local token = ARGV[1]
local key_ttl_ms = tonumber(ARGV[2])

local removed = redis.call("HDEL", lease_key, token)
redis.call("ZREM", exp_key, token)

redis.call("PEXPIRE", lease_key, key_ttl_ms)
redis.call("PEXPIRE", exp_key, key_ttl_ms)
redis.call("PEXPIRE", start_key, key_ttl_ms)

return removed
`

// leaseKeys returns the three keys backing a limiter's leases.
func leaseKeys(limiterID string) []string {
	return []string{
		leaseKeyPrefix + limiterID,
		expKeyPrefix + limiterID,
		startKeyPrefix + limiterID,
	}
}

// Acquire reserves capacity and returns a renewable lease. It implements
// LeaseDatastore.
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
	reply, err := rs.evalLeaseScript(ctx, redisAcquireScript, leaseKeys(limiterID), []interface{}{
		opts.MaxConcurrent,
		opts.MinTime.Microseconds(),
		weight,
		token,
		ttl.Microseconds(),
		keyTTL(ttl, opts.MinTime).Milliseconds(),
	})
	if err != nil {
		return nil, 0, err
	}

	values, ok := reply.([]interface{})
	if !ok || len(values) != 3 {
		return nil, 0, fmt.Errorf("unexpected redis acquire result format")
	}

	granted, err := toInt64(values[0])
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected redis acquire result: %w", err)
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

	reply, err := rs.evalLeaseScript(ctx, redisRenewScript, leaseKeys(lease.LimiterID), []interface{}{
		lease.Token,
		ttl.Microseconds(),
		keyTTL(ttl, 0).Milliseconds(),
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
	_, err := rs.evalLeaseScript(ctx, redisReleaseScript, leaseKeys(lease.LimiterID), []interface{}{
		lease.Token,
		keyTTL(lease.ttlOrDefault(), 0).Milliseconds(),
	})
	return err
}

// keyTTL is the garbage-collection window for an idle limiter's keys. It has to
// outlast both the lease TTL and the spacing window, since the start scores are
// what enforce MinTime.
func keyTTL(leaseTTL, minTime time.Duration) time.Duration {
	ttl := 2 * leaseTTL
	if spacing := 2 * minTime; spacing > ttl {
		ttl = spacing
	}
	return ttl
}

// evalLeaseScript runs a lease script, reloading it once if Redis has forgotten
// it. Lease scripts are cached by SHA on first use like the admission script.
func (rs *RedisStore) evalLeaseScript(ctx context.Context, script string, keys []string, args []interface{}) (interface{}, error) {
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

	reply, err := client.EvalSha(ctx, scriptSHA(script), keys, args...).Result()
	if isRedisNoScript(err) {
		if _, loadErr := client.ScriptLoad(ctx, script).Result(); loadErr != nil {
			return nil, fmt.Errorf("redis script reload error: %w", loadErr)
		}
		reply, err = client.EvalSha(ctx, scriptSHA(script), keys, args...).Result()
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
