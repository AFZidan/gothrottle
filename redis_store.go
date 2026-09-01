// FILENAME: redis_store.go
package gothrottle

import (
	"context"
	"crypto/sha1" // #nosec G505 - SHA1 is used for Redis script hashing, not cryptographic security
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// defaultStateTTL bounds how long shared counter state survives without
// updates, so a process that dies mid-job does not leak its reservation
// forever.
const defaultStateTTL = 30 * time.Second

// RedisStore is a Redis-based implementation of Datastore.
type RedisStore struct {
	client     *redis.Client
	scriptSHA  string
	ctx        context.Context
	cancelFunc context.CancelFunc
	mu         sync.RWMutex
}

// NewRedisStore creates a new RedisStore instance.
func NewRedisStore(client *redis.Client) (*RedisStore, error) {
	if client == nil {
		return nil, ErrNilClient
	}

	ctx, cancel := context.WithCancel(context.Background())

	rs := &RedisStore{
		client:     client,
		ctx:        ctx,
		cancelFunc: cancel,
	}

	// Load the Lua script
	if err := rs.loadScript(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to load Lua script: %w", err)
	}

	return rs, nil
}

// redisScript admits or refuses a job atomically.
//
// Time comes from Redis TIME rather than the calling process's clock, so a
// machine with a skewed clock cannot admit work early or impose an inflated
// wait on everyone else. Durations are microseconds, because MinTime is a
// time.Duration and truncating to whole milliseconds silently dropped
// sub-millisecond spacing that LocalStore honored.
const redisScript = `
local key = KEYS[1]
local max_concurrent = tonumber(ARGV[1])
local min_time_us = tonumber(ARGV[2])
local weight = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])

local time = redis.call("TIME")
local now_us = (tonumber(time[1]) * 1000000) + tonumber(time[2])

local state = redis.call("HGETALL", key)
local running = 0
local last_start = 0

for i = 1, #state, 2 do
    if state[i] == "running" then
        running = tonumber(state[i+1])
    elseif state[i] == "last_start_us" then
        last_start = tonumber(state[i+1])
    end
end

if max_concurrent > 0 and running + weight > max_concurrent then
    return {0, -1}
end

if min_time_us > 0 and last_start > 0 then
    local elapsed = now_us - last_start
    if elapsed < min_time_us then
        return {0, min_time_us - elapsed}
    end
end

redis.call("HINCRBY", key, "running", weight)
redis.call("HSET", key, "last_start_us", now_us)
redis.call("PEXPIRE", key, ttl_ms)

return {1, 0}
`

// loadScript loads the Lua script into Redis and stores its SHA.
func (rs *RedisStore) loadScript() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	return rs.loadScriptLocked()
}

func (rs *RedisStore) loadScriptLocked() error {
	if rs.client == nil {
		return ErrStoreClosed
	}

	sha := fmt.Sprintf("%x", sha1.Sum([]byte(redisScript))) // #nosec G401 - SHA1 is used for Redis script hashing, not cryptographic security

	// Check if script already exists
	exists, err := rs.client.ScriptExists(rs.ctx, sha).Result()
	if err != nil {
		return err
	}

	if len(exists) > 0 && exists[0] {
		rs.scriptSHA = sha
		return nil
	}

	// Load the script
	loadedSHA, err := rs.client.ScriptLoad(rs.ctx, redisScript).Result()
	if err != nil {
		return err
	}

	rs.scriptSHA = loadedSHA
	return nil
}

// Request checks if a job can run according to the limiter's rules.
func (rs *RedisStore) Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error) {
	if limiterID == "" {
		return false, 0, ErrMissingID
	}
	if err := validateLimiterID(limiterID); err != nil {
		return false, 0, err
	}
	if weight <= 0 {
		return false, 0, ErrInvalidWeight
	}
	if opts.MaxConcurrent > 0 && weight > opts.MaxConcurrent {
		return false, 0, ErrWeightExceedsMax
	}

	args := []interface{}{
		opts.MaxConcurrent,
		opts.MinTime.Microseconds(),
		weight,
		rs.stateTTL(opts).Milliseconds(),
	}

	result, err := rs.evalScript(limiterID, args)
	if err != nil {
		return false, 0, err
	}

	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 2 {
		return false, 0, fmt.Errorf("unexpected redis script result format")
	}

	canRunInt, ok := resultSlice[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("unexpected redis script result format for canRun")
	}

	waitTimeInt, ok := resultSlice[1].(int64)
	if !ok {
		return false, 0, fmt.Errorf("unexpected redis script result format for waitTime")
	}

	canRun = canRunInt == 1
	waitTime = 0 // Default to no wait
	if waitTimeInt > 0 {
		waitTime = time.Duration(waitTimeInt) * time.Microsecond
	}

	return canRun, waitTime, nil
}

// evalScript runs the admission script, reloading it once if Redis has
// forgotten it (NOSCRIPT), for example after a SCRIPT FLUSH or a failover.
func (rs *RedisStore) evalScript(limiterID string, args []interface{}) (interface{}, error) {
	key := stateKey(limiterID)

	rs.mu.RLock()
	client := rs.client
	ctx := rs.ctx
	sha := rs.scriptSHA
	rs.mu.RUnlock()
	if client == nil {
		return nil, ErrStoreClosed
	}

	result, err := client.EvalSha(ctx, sha, []string{key}, args...).Result()
	if isRedisNoScript(err) {
		if loadErr := rs.loadScript(); loadErr != nil {
			return nil, fmt.Errorf("redis script reload error: %w", loadErr)
		}

		rs.mu.RLock()
		client = rs.client
		ctx = rs.ctx
		sha = rs.scriptSHA
		rs.mu.RUnlock()
		if client == nil {
			return nil, ErrStoreClosed
		}

		result, err = client.EvalSha(ctx, sha, []string{key}, args...).Result()
	}
	if err != nil {
		return nil, fmt.Errorf("redis eval error: %w", err)
	}

	return result, nil
}

// stateTTL is how long the shared counter survives without updates. It is a
// safety net against a process dying mid-job and leaking its reservation, so it
// must outlast the spacing window: a MinTime longer than the TTL would
// otherwise be bypassed entirely once the key expired.
//
// This does not fix the underlying flaw — a job that runs longer than the TTL
// still has its state expire while it is running, which is what tokenized
// leases address.
func (rs *RedisStore) stateTTL(opts Options) time.Duration {
	ttl := defaultStateTTL
	if minimum := 2 * opts.MinTime; minimum > ttl {
		ttl = minimum
	}
	return ttl
}

func stateKey(limiterID string) string {
	return fmt.Sprintf("gothrottle:%s", limiterID)
}

// RegisterDone informs the store that a job has finished.
func (rs *RedisStore) RegisterDone(limiterID string, weight int) error {
	if limiterID == "" {
		return ErrMissingID
	}
	if err := validateLimiterID(limiterID); err != nil {
		return err
	}
	if weight <= 0 {
		return ErrInvalidWeight
	}

	rs.mu.RLock()
	client := rs.client
	ctx := rs.ctx
	rs.mu.RUnlock()
	if client == nil {
		return ErrStoreClosed
	}

	err := client.Eval(ctx, redisRegisterDoneScript, []string{stateKey(limiterID)},
		weight,
		defaultStateTTL.Milliseconds(),
	).Err()
	if err != nil {
		return fmt.Errorf("redis register done error: %w", err)
	}

	return nil
}

// Disconnect releases this store's resources. The *redis.Client passed to
// NewRedisStore was created by the caller and stays open: other stores,
// limiters or application components may still be using it. Callers close the
// client themselves when they are done with it.
func (rs *RedisStore) Disconnect() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.cancelFunc != nil {
		rs.cancelFunc()
		rs.cancelFunc = nil
	}

	// Dropping the reference marks the store closed; subsequent calls return
	// ErrStoreClosed.
	rs.client = nil

	return nil
}

// Close disconnects the store and additionally closes the underlying
// *redis.Client. Use it only when this store is the sole owner of the client;
// Disconnect leaves the client open for other users.
func (rs *RedisStore) Close() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.cancelFunc != nil {
		rs.cancelFunc()
		rs.cancelFunc = nil
	}

	client := rs.client
	rs.client = nil
	if client != nil {
		return client.Close()
	}

	return nil
}

const redisRegisterDoneScript = `
local key = KEYS[1]
local weight = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local running = tonumber(redis.call("HGET", key, "running") or "0")
running = running - weight
if running < 0 then
    running = 0
end
redis.call("HSET", key, "running", running)
redis.call("PEXPIRE", key, ttl_ms)
return running
`

func isRedisNoScript(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "NOSCRIPT")
}
