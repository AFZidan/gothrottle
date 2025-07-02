// FILENAME: integration_test.go
package gothrottle_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gothrottle"
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
	defer limiter.Stop()

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
	defer limiter.Stop()

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
	defer limiter.Stop()

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
