// FILENAME: redis_store.go
package gothrottle

import (
	"context"
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// RedisStore is a Redis-based implementation of Datastore.
type RedisStore struct {
	client     *redis.Client
	scriptSHA  string
	ctx        context.Context
	cancelFunc context.CancelFunc
}

// NewRedisStore creates a new RedisStore instance.
func NewRedisStore(client *redis.Client) (*RedisStore, error) {
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

// The Lua script MUST be this exact script:
const redisScript = `
local key = KEYS[1]
local max_concurrent = tonumber(ARGV[1])
local min_time_ms = tonumber(ARGV[2])
local weight = tonumber(ARGV[3])
local current_time_ms = tonumber(ARGV[4])

local state = redis.call("HGETALL", key)
local running = 0
local last_start = 0

for i = 1, #state, 2 do
    if state[i] == "running" then
        running = tonumber(state[i+1])
    elseif state[i] == "last_start" then
        last_start = tonumber(state[i+1])
    end
end

if max_concurrent > 0 and running + weight > max_concurrent then
    return {0, -1}
end

local elapsed = current_time_ms - last_start
if min_time_ms > 0 and elapsed < min_time_ms then
    local wait = min_time_ms - elapsed
    return {0, wait}
end

redis.call("HINCRBY", key, "running", weight)
redis.call("HSET", key, "last_start", current_time_ms)
redis.call("PEXPIRE", key, 30000)

return {1, 0}
`

// loadScript loads the Lua script into Redis and stores its SHA.
func (rs *RedisStore) loadScript() error {
	sha := fmt.Sprintf("%x", sha1.Sum([]byte(redisScript)))

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
	if rs.client == nil {
		return false, 0, ErrStoreClosed
	}

	key := fmt.Sprintf("gothrottle:%s", limiterID)
	currentTimeMs := time.Now().UnixMilli()

	result, err := rs.client.EvalSha(rs.ctx, rs.scriptSHA, []string{key},
		opts.MaxConcurrent,
		opts.MinTime.Milliseconds(),
		weight,
		currentTimeMs,
	).Result()

	if err != nil {
		return false, 0, fmt.Errorf("redis eval error: %w", err)
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
		waitTime = time.Duration(waitTimeInt) * time.Millisecond
	}

	return canRun, waitTime, nil
}

// RegisterDone informs the store that a job has finished.
func (rs *RedisStore) RegisterDone(limiterID string, weight int) error {
	if rs.client == nil {
		return ErrStoreClosed
	}

	key := fmt.Sprintf("gothrottle:%s", limiterID)

	err := rs.client.HIncrBy(rs.ctx, key, "running", int64(-weight)).Err()
	if err != nil {
		return fmt.Errorf("redis hincrby error: %w", err)
	}

	return nil
}

// Disconnect cleans up any connections.
func (rs *RedisStore) Disconnect() error {
	if rs.cancelFunc != nil {
		rs.cancelFunc()
	}

	if rs.client != nil {
		err := rs.client.Close()
		rs.client = nil
		return err
	}

	return nil
}
