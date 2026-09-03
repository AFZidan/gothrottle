// FILENAME: redis_store.go
package gothrottle

import (
	"context"
	"crypto/sha1" // #nosec G505 - SHA1 is used for Redis script hashing, not cryptographic security
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// defaultStateTTL bounds how long shared counter state survives without
// updates, so a process that dies mid-job does not leak its reservation
// forever.
const defaultStateTTL = 30 * time.Second

// RedisStore is a Redis-based implementation of Datastore and LeaseDatastore.
type RedisStore struct {
	client     redis.UniversalClient
	scriptSHA  string
	ctx        context.Context
	cancelFunc context.CancelFunc
	mu         sync.RWMutex
}

// NewRedisStore creates a new RedisStore instance.
//
// The parameter is go-redis's UniversalClient, which *redis.Client satisfies, so
// existing call sites are unchanged. *redis.ClusterClient and *redis.Ring
// satisfy it too; see the package documentation on Redis Cluster for what is and
// is not supported there.
func NewRedisStore(client redis.UniversalClient) (*RedisStore, error) {
	if isNilClient(client) {
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

// isNilClient reports whether a client value is unusable. A nil interface and a
// typed nil pointer stored in one — NewRedisStore((*redis.Client)(nil)) — are
// both rejected here, because a typed nil is non-nil as an interface and would
// otherwise panic on first use.
func isNilClient(client redis.UniversalClient) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return value.IsNil()
	default:
		return false
	}
}

// redisScript admits or refuses a job atomically.
//
// Time comes from Redis TIME rather than the calling process's clock, so a
// machine with a skewed clock cannot admit work early or impose an inflated
// wait on everyone else. Durations are microseconds, because MinTime is a
// time.Duration and truncating to whole milliseconds silently dropped
// sub-millisecond spacing that LocalStore honored.
//
// This is the legacy counter path, kept for Datastore implementations and
// callers that use Request/RegisterDone directly. It cannot tell a slow holder
// from a dead one; the lease path is what the limiter uses.
//
// The TTL goes through redisEnsureTTLHelper because Request sizes it to cover
// the spacing window while RegisterDone knows nothing about MinTime: a
// completion that reset the TTL to its own default expired state that was still
// protecting an active window, so a 40s MinTime lost its spacing after 30s.
const redisScript = redisEnsureTTLHelper + `
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

-- Subtraction rather than "running + weight > max_concurrent": both operands are
-- individually within Lua's exact range, but their sum need not be, and a sum that
-- rounds turns a refusal into an admission. max_concurrent - running is bounded by
-- max_concurrent, so it stays exact. Matches the lease script and the Go side.
if max_concurrent > 0 and (running >= max_concurrent or weight > max_concurrent - running) then
    return {0, -1}
end

if min_time_us > 0 and last_start > 0 then
    local elapsed = now_us - last_start
    if elapsed < min_time_us then
        local wait_us = min_time_us - elapsed
        -- A Redis clock that moved backwards would otherwise report a wait
        -- longer than the window itself, stalling the caller past the spacing it
        -- actually owes. The lease script clamps the same way.
        if wait_us > min_time_us then
            wait_us = min_time_us
        end
        return {0, wait_us}
    end
end

redis.call("HINCRBY", key, "running", weight)
redis.call("HSET", key, "last_start_us", now_us)
ensure_ttl(key, ttl_ms)

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

	sha := scriptSHA(redisScript)

	exists, err := rs.client.ScriptExists(rs.ctx, sha).Result()
	if err != nil {
		return err
	}

	if len(exists) > 0 && exists[0] {
		rs.scriptSHA = sha
		return nil
	}

	loadedSHA, err := rs.client.ScriptLoad(rs.ctx, redisScript).Result()
	if err != nil {
		return err
	}

	rs.scriptSHA = loadedSHA
	return nil
}

// scriptSHA is the SHA1 Redis uses to identify a cached script. SHA1 here is
// Redis's addressing scheme, not a security choice.
func scriptSHA(script string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(script))) // #nosec G401 - SHA1 is Redis's script cache key, not a security primitive
}

// Request checks if a job can run according to the limiter's rules.
func (rs *RedisStore) Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error) {
	if limiterID == "" {
		return false, 0, ErrMissingID
	}
	if err := validateAdmission(limiterID, weight, opts); err != nil {
		return false, 0, err
	}

	// Validation has already bounded MaxConcurrent, weight and MinTime, but the
	// derived state TTL is computed here and has to be checked in its own right.
	stateTTLMS, err := luaMillis("legacy state TTL in milliseconds", rs.stateTTL(opts))
	if err != nil {
		return false, 0, err
	}

	reply, err := rs.evalScript(limiterID, []interface{}{
		opts.MaxConcurrent,
		durationMicros(opts.MinTime),
		weight,
		stateTTLMS,
	})
	if err != nil {
		return false, 0, err
	}

	return parseRequestReply(reply)
}

// parseRequestReply decodes the legacy admission script's reply: whether the job
// may run, and for a spacing refusal how long remains of the window.
func parseRequestReply(reply interface{}) (canRun bool, waitTime time.Duration, err error) {
	values, ok := reply.([]interface{})
	if !ok || len(values) != 2 {
		return false, 0, fmt.Errorf("unexpected redis script result format")
	}

	admitted, err := toInt64(values[0])
	if err != nil {
		return false, 0, fmt.Errorf("unexpected redis script result for canRun: %w", err)
	}
	waitUS, err := toInt64(values[1])
	if err != nil {
		return false, 0, fmt.Errorf("unexpected redis script result for waitTime: %w", err)
	}

	// A negative wait means the refusal has no deadline: capacity is held, and
	// only another instance releasing it can change the outcome. A positive one
	// is clamped on conversion, so a nonsense reply cannot wrap into a negative
	// Duration that the scheduler would read as "no deadline at all".
	return admitted == 1, microsToDuration(waitUS), nil
}

// evalScript runs the admission script, falling back to EVAL if Redis has
// forgotten it (NOSCRIPT), for example after a SCRIPT FLUSH or a failover. EVAL
// executes and caches the script on the node that serves it, so the fallback
// also covers a Cluster or Ring client where SCRIPT LOAD reached a different
// node than the one holding the key.
func (rs *RedisStore) evalScript(limiterID string, args []interface{}) (interface{}, error) {
	keys := []string{stateKey(limiterID)}

	rs.mu.RLock()
	client := rs.client
	ctx := rs.ctx
	sha := rs.scriptSHA
	rs.mu.RUnlock()
	if client == nil {
		return nil, ErrStoreClosed
	}

	result, err := client.EvalSha(ctx, sha, keys, args...).Result()
	if isRedisNoScript(err) {
		result, err = client.Eval(ctx, redisScript, keys, args...).Result()
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
// The doubling saturates rather than wrapping. A MinTime past half the Duration
// range would otherwise produce a negative TTL, which reads as "shorter than the
// default" and would leave the window unprotected — the opposite of the intent.
//
// This does not fix the underlying flaw — a job that runs longer than the TTL
// still has its state expire while it is running, which is what tokenized
// leases address.
func (rs *RedisStore) stateTTL(opts Options) time.Duration {
	ttl := defaultStateTTL
	if minimum := doubleDuration(opts.MinTime); minimum > ttl {
		ttl = minimum
	}
	return ttl
}

// stateKey is the legacy counter key for a limiter ID. It is a single key, so it
// needs no hash tag: the legacy script is single-key and routes to whatever slot
// the ID hashes to.
func stateKey(limiterID string) string {
	return fmt.Sprintf("gothrottle:%s", limiterID)
}

// RedisStateKey returns the key the legacy Request/RegisterDone path uses for a
// limiter ID. Like RedisKeys it is exported for operational inspection.
func RedisStateKey(limiterID string) string {
	return stateKey(limiterID)
}

// RegisterDone informs the store that a job has finished.
func (rs *RedisStore) RegisterDone(limiterID string, weight int) error {
	if limiterID == "" {
		return ErrMissingID
	}
	if err := validateCompletion(limiterID, weight); err != nil {
		return err
	}

	// This path takes no Options, so Options.Validate never sees the weight. It
	// still reaches a Lua script, so it is bounded here.
	luaWeight, err := luaInt("weight", weight)
	if err != nil {
		return err
	}
	stateTTLMS, err := luaMillis("legacy state TTL in milliseconds", defaultStateTTL)
	if err != nil {
		return err
	}

	rs.mu.RLock()
	client := rs.client
	ctx := rs.ctx
	rs.mu.RUnlock()
	if client == nil {
		return ErrStoreClosed
	}

	if err := client.Eval(ctx, redisRegisterDoneScript, []string{stateKey(limiterID)},
		luaWeight,
		stateTTLMS,
	).Err(); err != nil {
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

// redisRegisterDoneScript records a completion on the legacy counter.
//
// The fallback TTL applies only when the key has no usable expiry of its own —
// see redisEnsureTTLHelper for why. last_start_us is left untouched either way,
// so spacing is measured from the job's start regardless of when it finished.
const redisRegisterDoneScript = redisEnsureTTLHelper + `
local key = KEYS[1]
local weight = tonumber(ARGV[1])
local ttl_ms = tonumber(ARGV[2])
local running = tonumber(redis.call("HGET", key, "running") or "0")
running = running - weight
if running < 0 then
    running = 0
end
redis.call("HSET", key, "running", running)
ensure_ttl(key, ttl_ms)
return running
`

func isRedisNoScript(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "NOSCRIPT")
}
