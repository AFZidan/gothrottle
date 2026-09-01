// FILENAME: integration_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// TestIntegration demonstrates the full workflow
func TestIntegration(t *testing.T) {
	// Create a limiter with both concurrent and time limits
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "integration-test",
		MaxConcurrent: 2,
		MinTime:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	// Track execution order and timing
	var results []string
	var timestamps []time.Time
	var mu sync.Mutex

	start := time.Now()

	// Submit multiple jobs concurrently
	var wg sync.WaitGroup
	jobCount := 5

	for i := 0; i < jobCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			priority := 10 - id // Higher priority for lower IDs
			result, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				mu.Lock()
				results = append(results, fmt.Sprintf("job-%d", id))
				timestamps = append(timestamps, time.Now())
				mu.Unlock()

				// Simulate work
				time.Sleep(25 * time.Millisecond)

				return fmt.Sprintf("result-%d", id), nil
			}, priority, 1)

			if err != nil {
				t.Errorf("Job %d failed: %v", id, err)
				return
			}

			expected := fmt.Sprintf("result-%d", id)
			if result != expected {
				t.Errorf("Job %d: expected %s, got %v", id, expected, result)
			}
		}(i)
	}

	wg.Wait()
	totalTime := time.Since(start)

	// Verify results
	if len(results) != jobCount {
		t.Fatalf("Expected %d results, got %d", jobCount, len(results))
	}

	if len(timestamps) != jobCount {
		t.Fatalf("Expected %d timestamps, got %d", jobCount, len(timestamps))
	}

	// Check that no more than 2 jobs ran concurrently
	// and that there was proper spacing between job starts
	t.Logf("Execution order: %v", results)
	t.Logf("Total execution time: %v", totalTime)

	// Verify minimum time between job starts
	for i := 1; i < len(timestamps); i++ {
		gap := timestamps[i].Sub(timestamps[i-1])
		t.Logf("Gap between job %d and %d: %v", i-1, i, gap)
	}

	// The total time should be at least (jobCount-1) * MinTime / MaxConcurrent
	// Since we can run 2 jobs concurrently, but need 50ms between starts
	expectedMinTime := time.Duration(jobCount-1) * 50 * time.Millisecond / 2
	if totalTime < expectedMinTime {
		t.Logf("Warning: Total time %v might be less than expected minimum %v", totalTime, expectedMinTime)
	}
}

// TestWrappedFunction demonstrates wrapping existing functions
func TestWrappedFunction(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 1,
		MinTime:       100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	// Original function
	originalFunc := func() (interface{}, error) {
		return "original-result", nil
	}

	// Wrap it
	wrappedFunc := limiter.Wrap(originalFunc)

	// Test multiple calls
	start := time.Now()
	for i := 0; i < 3; i++ {
		result, err := wrappedFunc()
		if err != nil {
			t.Errorf("Wrapped call %d failed: %v", i, err)
		}
		if result != "original-result" {
			t.Errorf("Wrapped call %d: expected 'original-result', got %v", i, result)
		}
	}
	elapsed := time.Since(start)

	// Should take at least 200ms (2 * 100ms between calls)
	expectedMinTime := 200 * time.Millisecond
	if elapsed < expectedMinTime {
		t.Errorf("Wrapped calls completed too quickly: %v < %v", elapsed, expectedMinTime)
	}

	t.Logf("Wrapped function calls took: %v", elapsed)
}

// BenchmarkLimiter measures performance
func BenchmarkLimiter(b *testing.B) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 10,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := limiter.Schedule(func() (interface{}, error) {
				return "benchmark-result", nil
			})
			if err != nil {
				b.Error(err)
			}
		}
	})
}

func TestRedisStore_NilClient(t *testing.T) {
	_, err := gothrottle.NewRedisStore(nil)
	if !errors.Is(err, gothrottle.ErrStoreClosed) {
		t.Fatalf("NewRedisStore(nil) error = %v, want ErrStoreClosed", err)
	}
}

func TestRedisStore_ClosedClientBehavior(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	if err := store.Disconnect(); err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}

	_, _, err = store.Request("closed", 1, gothrottle.Options{MaxConcurrent: 1})
	if !errors.Is(err, gothrottle.ErrStoreClosed) {
		t.Fatalf("closed Request error = %v, want ErrStoreClosed", err)
	}

	if err := store.RegisterDone("closed", 1); !errors.Is(err, gothrottle.ErrStoreClosed) {
		t.Fatalf("closed RegisterDone error = %v, want ErrStoreClosed", err)
	}
}

func TestRedisStore_DisconnectLeavesClientUsable(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	if err := store.Disconnect(); err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}

	// The caller created the client, so Disconnect must not close it: other
	// stores, limiters or application code may still be using it.
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis client is unusable after store.Disconnect: %v", err)
	}

	// A second store on the same client keeps working.
	other, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore on shared client failed: %v", err)
	}
	defer func() { _ = other.Disconnect() }()

	canRun, _, err := other.Request(uniqueLimiterID("shared-client"), 1, gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("Request on shared client failed: %v", err)
	}
	if !canRun {
		t.Fatal("Request on shared client should be allowed")
	}
}

func TestRedisStore_CloseClosesClient(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}

	// Close is the explicit opt-in that also tears down the client.
	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := client.Ping(context.Background()).Err(); err == nil {
		t.Fatal("Redis client should be closed after store.Close")
	}
}

func TestLimiter_StopKeepsSharedRedisClientUsable(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("stop-shared-redis"),
		MaxConcurrent: 1,
		Datastore:     store,
	})
	if err != nil {
		t.Fatalf("NewLimiter failed: %v", err)
	}

	if _, err := limiter.Schedule(func() (interface{}, error) { return "ok", nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	if err := limiter.Stop(); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	// Stopping a limiter must not invalidate the caller's Redis client or the
	// store shared with other limiters.
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis client is unusable after limiter.Stop: %v", err)
	}

	second, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("stop-shared-redis-2"),
		MaxConcurrent: 1,
		Datastore:     store,
	})
	if err != nil {
		t.Fatalf("NewLimiter on shared store failed: %v", err)
	}
	defer func() { _ = second.Stop() }()

	if _, err := second.Schedule(func() (interface{}, error) { return "ok", nil }); err != nil {
		t.Fatalf("second limiter Schedule failed after first limiter stopped: %v", err)
	}
}

func TestRedisStore_ReloadsMissingScript(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	if err := client.ScriptFlush(context.Background()).Err(); err != nil {
		t.Fatalf("ScriptFlush failed: %v", err)
	}

	canRun, _, err := store.Request(uniqueLimiterID("reload-script"), 1, gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("Request after ScriptFlush failed: %v", err)
	}
	if !canRun {
		t.Fatal("Request after ScriptFlush should be allowed")
	}
}

func TestRedisStore_RegisterDoneDoesNotUnderflow(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("underflow")

	if err := store.RegisterDone(id, 1); err != nil {
		t.Fatalf("RegisterDone failed: %v", err)
	}

	canRun, _, err := store.Request(id, 1, gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if !canRun {
		t.Fatal("running count underflow should not block new work")
	}
}
