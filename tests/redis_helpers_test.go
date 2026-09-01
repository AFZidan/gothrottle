// FILENAME: redis_helpers_test.go
package gothrottle_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// requireRedisEnv reports whether the environment demands that Redis-backed
// tests actually run. CI sets REQUIRE_REDIS=true so that a missing or
// unreachable Redis fails the build instead of silently skipping, which would
// leave the Redis matrix green without exercising any Redis code.
func requireRedisEnv() bool {
	return os.Getenv("REQUIRE_REDIS") == "true"
}

// redisTestAddr resolves the Redis address for integration tests. Without
// REDIS_ADDR the test skips locally and fails under REQUIRE_REDIS=true.
func redisTestAddr(t *testing.T) string {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		if requireRedisEnv() {
			t.Fatal("REDIS_ADDR is not set but REQUIRE_REDIS=true: Redis integration tests must not skip in CI")
		}
		t.Skip("REDIS_ADDR not set")
	}
	return addr
}

// newTestRedisClient returns a client connected to the test Redis instance and
// verifies reachability before the test proceeds. The client is closed when the
// test finishes.
func newTestRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: redisTestAddr(t)})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		if requireRedisEnv() {
			t.Fatalf("Redis at %s is unreachable but REQUIRE_REDIS=true: %v", client.Options().Addr, err)
		}
		t.Skipf("Redis at %s is unreachable: %v", client.Options().Addr, err)
	}
	return client
}

var testIDCounter uint64

// uniqueLimiterID builds a limiter ID that is unique per test run so parallel
// jobs and repeated runs against a shared Redis cannot observe each other's
// state.
func uniqueLimiterID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&testIDCounter, 1))
}
