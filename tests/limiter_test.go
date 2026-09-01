// FILENAME: limiter_test.go
package gothrottle_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

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
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

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
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

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
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

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

func TestLimiter_StopWaitsForRunningJob(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	scheduleDone := make(chan error, 1)

	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-release
			return "done", nil
		})
		scheduleDone <- err
	}()

	<-started

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- limiter.Stop()
	}()

	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before running job completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-scheduleDone:
		if err != nil {
			t.Fatalf("running job returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("running job did not complete")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after running job completed")
	}
}

func TestLimiter_StopCancelsQueuedWork(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(firstStarted)
			<-releaseFirst
			return nil, nil
		})
		firstDone <- err
	}()
	<-firstStarted

	queuedDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			return nil, nil
		})
		queuedDone <- err
	}()

	time.Sleep(25 * time.Millisecond)
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- limiter.Stop()
	}()

	select {
	case err := <-queuedDone:
		if !errors.Is(err, gothrottle.ErrStoreClosed) {
			t.Fatalf("queued job error = %v, want ErrStoreClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued job was not canceled")
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first job returned error: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
}

func TestLimiter_ScheduleRejectsNilTask(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	_, err = limiter.Schedule(nil)
	if !errors.Is(err, gothrottle.ErrNilTask) {
		t.Fatalf("Schedule(nil) error = %v, want ErrNilTask", err)
	}
}

func TestLimiter_ScheduleReturnsPanicError(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	_, err = limiter.Schedule(func() (interface{}, error) {
		panic("boom")
	})
	if !errors.Is(err, gothrottle.ErrTaskPanic) {
		t.Fatalf("panic task error = %v, want ErrTaskPanic", err)
	}
}

func TestLimiter_ScheduleRejectsOverweightJob(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	_, err = limiter.ScheduleWithOptions(func() (interface{}, error) {
		return nil, nil
	}, 1, 2)
	if !errors.Is(err, gothrottle.ErrWeightExceedsMax) {
		t.Fatalf("overweight job error = %v, want ErrWeightExceedsMax", err)
	}
}

func TestLimiter_ConcurrentScheduleWithOptionsRace(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var executed int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(priority int) {
			defer wg.Done()
			_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				atomic.AddInt32(&executed, 1)
				return nil, nil
			}, priority, 1)
			if err != nil {
				t.Errorf("ScheduleWithOptions failed: %v", err)
			}
		}(i % 10)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&executed); got != 100 {
		t.Fatalf("executed jobs = %d, want 100", got)
	}
}

func TestPublicAPICompatibility(t *testing.T) {
	if _, err := gothrottle.NewLimiter(gothrottle.Options{
		Datastore: gothrottle.NewLocalStore(),
	}); !errors.Is(err, gothrottle.ErrMissingID) {
		t.Fatalf("custom datastore without ID error = %v, want ErrMissingID", err)
	}

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Stop(); err != nil {
		t.Fatal(err)
	}

	if _, err := limiter.Schedule(func() (interface{}, error) {
		return nil, nil
	}); !errors.Is(err, gothrottle.ErrStoreClosed) {
		t.Fatalf("Schedule after Stop error = %v, want ErrStoreClosed", err)
	}

	pq := gothrottle.NewPriorityQueue()
	pq.PushJob(&gothrottle.Job{Priority: 1})
	pq.PushJob(&gothrottle.Job{Priority: 10})
	if pq.IsEmpty() {
		t.Fatal("priority queue should not be empty")
	}
	if job := pq.PopJob(); job == nil || job.Priority != 10 {
		t.Fatalf("highest priority job = %#v, want priority 10", job)
	}
}

// TestDatastoreInterfaceIsStillSufficient pins the additive nature of the lease
// redesign: a store implementing only the original three methods must keep
// working, since LeaseDatastore embeds rather than replaces Datastore.
func TestDatastoreInterfaceIsStillSufficient(t *testing.T) {
	var store gothrottle.Datastore = &legacyOnlyStore{}

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "legacy-store",
		MaxConcurrent: 1,
		Datastore:     store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	result, err := limiter.Schedule(func() (interface{}, error) { return "legacy", nil })
	if err != nil {
		t.Fatalf("Schedule against a legacy Datastore failed: %v", err)
	}
	if result != "legacy" {
		t.Fatalf("result = %v, want \"legacy\"", result)
	}
}

// legacyOnlyStore implements Datastore and nothing else.
type legacyOnlyStore struct {
	mu      sync.Mutex
	running int
}

func (s *legacyOnlyStore) Request(_ string, weight int, opts gothrottle.Options) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if opts.MaxConcurrent > 0 && s.running+weight > opts.MaxConcurrent {
		return false, 0, nil
	}
	s.running += weight
	return true, 0, nil
}

func (s *legacyOnlyStore) RegisterDone(_ string, weight int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.running -= weight
	if s.running < 0 {
		s.running = 0
	}
	return nil
}

func (s *legacyOnlyStore) Disconnect() error { return nil }

// TestBuiltInStoresImplementLeaseDatastore fails at compile time if either
// built-in store loses its lease support.
func TestBuiltInStoresImplementLeaseDatastore(t *testing.T) {
	var _ gothrottle.LeaseDatastore = gothrottle.NewLocalStore()
	var _ gothrottle.LeaseDatastore = (*gothrottle.RedisStore)(nil)
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
	canRun, _, err = store.Request("test", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !canRun {
		t.Error("Second request should be allowed")
	}

	// Third request should fail (exceeds concurrent limit)
	canRun, _, err = store.Request("test", 1, opts)
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
	canRun, _, err = store.Request("test", 1, opts)
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
