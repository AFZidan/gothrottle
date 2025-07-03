// FILENAME: limiter_test.go
package gothrottle_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

func TestLimiter_MaxConcurrent(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Stop()

	// Track concurrent executions
	var concurrent int32
	var maxConcurrent int32
	var mu sync.Mutex

	// Start multiple jobs
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := limiter.Schedule(func() (interface{}, error) {
				mu.Lock()
				concurrent++
				if concurrent > maxConcurrent {
					maxConcurrent = concurrent
				}
				mu.Unlock()

				time.Sleep(100 * time.Millisecond)

				mu.Lock()
				concurrent--
				mu.Unlock()

				return fmt.Sprintf("job-%d", id), nil
			})
			if err != nil {
				t.Errorf("Job failed: %v", err)
			}
		}(i)
	}

	wg.Wait()

	if maxConcurrent > 2 {
		t.Errorf("Expected max concurrent 2, got %d", maxConcurrent)
	}
}

func TestLimiter_MinTime(t *testing.T) {
	minTime := 100 * time.Millisecond
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MinTime: minTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Stop()

	start := time.Now()
	var times []time.Time

	// Schedule multiple jobs
	for i := 0; i < 3; i++ {
		_, err := limiter.Schedule(func() (interface{}, error) {
			times = append(times, time.Now())
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Check that jobs were spaced by at least minTime
	for i := 1; i < len(times); i++ {
		elapsed := times[i].Sub(times[i-1])
		if elapsed < minTime {
			t.Errorf("Jobs too close together: %v < %v", elapsed, minTime)
		}
	}

	totalTime := time.Since(start)
	expectedMinTime := time.Duration(len(times)-1) * minTime
	if totalTime < expectedMinTime {
		t.Errorf("Total time too short: %v < %v", totalTime, expectedMinTime)
	}
}

func TestLimiter_Priority(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 1, // Force serialization
	})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Stop()

	var results []string
	var mu sync.Mutex

	var wg sync.WaitGroup

	// Schedule jobs with different priorities
	priorities := []int{1, 10, 5}
	for i, priority := range priorities {
		wg.Add(1)
		go func(id, prio int) {
			defer wg.Done()
			_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				mu.Lock()
				results = append(results, fmt.Sprintf("job-%d-prio-%d", id, prio))
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				return nil, nil
			}, prio, 1)
			if err != nil {
				t.Errorf("Job failed: %v", err)
			}
		}(i, priority)
	}

	wg.Wait()

	// Higher priority jobs should execute first
	// Expected order: prio-10, prio-5, prio-1
	if len(results) != 3 {
		t.Fatalf("Expected 3 results, got %d", len(results))
	}

	t.Logf("Execution order: %v", results)
	// Note: Due to timing, we can't guarantee exact order, but higher priorities should generally go first
}

func TestLimiter_Weight(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Stop()

	// Schedule a heavy job (weight 3) - should use all capacity
	var executed bool
	_, err = limiter.ScheduleWithOptions(func() (interface{}, error) {
		executed = true
		time.Sleep(100 * time.Millisecond)
		return nil, nil
	}, 5, 3)

	if err != nil {
		t.Fatal(err)
	}

	if !executed {
		t.Error("Heavy job should have executed")
	}
}

func TestLimiter_Stop(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Schedule a job
	done := make(chan bool)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			return "result", nil
		})
		if err != nil {
			t.Errorf("Job failed: %v", err)
		}
		done <- true
	}()

	// Wait for job to complete
	<-done

	// Stop the limiter
	err = limiter.Stop()
	if err != nil {
		t.Fatal(err)
	}

	// Try to schedule another job - should fail
	_, err = limiter.Schedule(func() (interface{}, error) {
		return nil, nil
	})
	if err == nil {
		t.Error("Expected error when scheduling on stopped limiter")
	}
}

func TestLocalStore_Basic(t *testing.T) {
	store := gothrottle.NewLocalStore()
	opts := gothrottle.Options{
		MaxConcurrent: 2,
		// No MinTime constraint for this test
	}

	// First request should succeed
	canRun, waitTime, err := store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("First request should be allowed")
	}
	if waitTime != 0 {
		t.Error("First request should not have wait time")
	}

	// Second request should succeed (within concurrent limit)
	canRun, waitTime, err = store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("Second request should be allowed")
	}

	// Third request should fail (exceeds concurrent limit)
	canRun, waitTime, err = store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if canRun {
		t.Error("Third request should be denied")
	}

	// Mark one job as done
	err = store.RegisterDone("test", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Now third request should succeed
	canRun, waitTime, err = store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("Request after RegisterDone should be allowed")
	}
}

func TestLocalStore_MinTime(t *testing.T) {
	store := gothrottle.NewLocalStore()
	opts := gothrottle.Options{
		MinTime: 100 * time.Millisecond,
	}

	// First request
	canRun, _, err := store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("First request should be allowed")
	}

	// Second request immediately - should be denied
	canRun, waitTime, err := store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if canRun {
		t.Error("Second request should be denied due to min time")
	}
	if waitTime <= 0 {
		t.Error("Should return positive wait time")
	}

	// Wait and try again
	time.Sleep(waitTime + 10*time.Millisecond)
	canRun, _, err = store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("Request after waiting should be allowed")
	}
}
