// FILENAME: scheduler_test.go
package gothrottle_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// TestScheduler_ThroughputWithoutLimits guards against the old ticker-based
// loop, which started at most one job per 10ms tick and needed ~10s for 1000
// trivial jobs even with no limits configured.
func TestScheduler_ThroughputWithoutLimits(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	const jobs = 1000

	var executed int32
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := limiter.Schedule(func() (interface{}, error) {
				atomic.AddInt32(&executed, 1)
				return nil, nil
			}); err != nil {
				t.Errorf("Schedule failed: %v", err)
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&executed); got != jobs {
		t.Fatalf("executed = %d, want %d", got, jobs)
	}
	// The old scheduler needed ~10s for this; 1s leaves ample headroom for slow
	// CI machines while still catching a regression to per-tick dispatch.
	if elapsed > time.Second {
		t.Fatalf("%d unlimited jobs took %v, want under 1s (per-tick dispatch regression?)", jobs, elapsed)
	}
	t.Logf("%d jobs in %v", jobs, elapsed)
}

// TestScheduler_DispatchesUpToCapacityImmediately verifies a burst fills the
// concurrency window at once instead of admitting one job per tick.
func TestScheduler_DispatchesUpToCapacityImmediately(t *testing.T) {
	const maxConcurrent = 8

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var running int32
	var peak int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := limiter.Schedule(func() (interface{}, error) {
				current := atomic.AddInt32(&running, 1)
				for {
					peaked := atomic.LoadInt32(&peak)
					if current <= peaked || atomic.CompareAndSwapInt32(&peak, peaked, current) {
						break
					}
				}
				<-release
				atomic.AddInt32(&running, -1)
				return nil, nil
			})
			if err != nil {
				t.Errorf("Schedule failed: %v", err)
			}
		}()
	}

	// All jobs should be running well before maxConcurrent ticks of the old
	// 10ms poll would have elapsed.
	deadline := time.After(500 * time.Millisecond)
	for atomic.LoadInt32(&running) < maxConcurrent {
		select {
		case <-deadline:
			close(release)
			wg.Wait()
			t.Fatalf("only %d of %d jobs started within 500ms", atomic.LoadInt32(&running), maxConcurrent)
		case <-time.After(time.Millisecond):
		}
	}

	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&peak); got != maxConcurrent {
		t.Fatalf("peak concurrency = %d, want %d", got, maxConcurrent)
	}
}

// TestScheduler_IdleLimiterDoesNotBusyLoop checks that an idle limiter parks
// instead of waking 100 times a second.
func TestScheduler_IdleLimiterDoesNotBusyLoop(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var requests int32
	counting := &countingRequestStore{
		LocalStore: gothrottle.NewLocalStore(),
		requests:   &requests,
	}

	idle, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "idle",
		MaxConcurrent: 1,
		Datastore:     counting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Stop() }()

	// One job to prove the limiter works, then leave it idle.
	if _, err := idle.Schedule(func() (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	atomic.StoreInt32(&requests, 0)

	time.Sleep(200 * time.Millisecond)

	// The old loop polled the datastore every 10ms whether or not work existed.
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("idle limiter made %d datastore requests in 200ms, want 0", got)
	}
}

// countingRequestStore counts Request calls to detect idle polling.
type countingRequestStore struct {
	*gothrottle.LocalStore
	requests *int32
}

func (c *countingRequestStore) Request(id string, weight int, opts gothrottle.Options) (bool, time.Duration, error) {
	atomic.AddInt32(c.requests, 1)
	return c.LocalStore.Request(id, weight, opts)
}

// TestScheduler_EqualPriorityIsFIFO pins the ordering guarantee for
// equal-priority jobs, which container/heap alone does not provide.
func TestScheduler_EqualPriorityIsFIFO(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// Occupy the single slot so the rest of the jobs queue up in submission
	// order.
	blockStarted := make(chan struct{})
	release := make(chan struct{})
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(blockStarted)
			<-release
			return nil, nil
		})
		if err != nil {
			t.Errorf("blocking job failed: %v", err)
		}
	}()
	<-blockStarted

	const jobs = 20
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	for i := 0; i < jobs; i++ {
		id := i
		enqueued := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(enqueued)
			_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
				return nil, nil
			}, 5, 1)
			if err != nil {
				t.Errorf("job %d failed: %v", id, err)
			}
		}()
		<-enqueued
		// Serialize submissions so "submission order" is well defined.
		time.Sleep(2 * time.Millisecond)
	}

	close(release)
	<-blockerDone
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != jobs {
		t.Fatalf("executed %d jobs, want %d", len(order), jobs)
	}
	for i, id := range order {
		if id != i {
			t.Fatalf("equal-priority execution order = %v, want ascending submission order", order)
		}
	}
}

// TestScheduler_StrictPriorityBlocksOnHeavyJob documents the default policy:
// a heavy high-priority job holds the queue rather than letting lighter work
// jump ahead.
func TestScheduler_StrictPriorityBlocksOnHeavyJob(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	occupied := make(chan struct{})
	release := make(chan struct{})
	occupantDone := make(chan struct{})
	go func() {
		defer close(occupantDone)
		// Weight 2 of 4, leaving 2 free.
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(occupied)
			<-release
			return nil, nil
		}, 1, 2)
		if err != nil {
			t.Errorf("occupant failed: %v", err)
		}
	}()
	<-occupied

	// Heavy high-priority job needs all 4; it cannot start yet.
	heavyDone := make(chan struct{})
	go func() {
		defer close(heavyDone)
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 10, 4)
		if err != nil {
			t.Errorf("heavy job failed: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// Light low-priority job would fit in the 2 free slots, but strict priority
	// makes it wait behind the heavy job.
	lightRan := make(chan struct{})
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(lightRan)
			return nil, nil
		}, 1, 1)
		if err != nil {
			t.Errorf("light job failed: %v", err)
		}
	}()

	select {
	case <-lightRan:
		close(release)
		t.Fatal("light job ran ahead of the heavy high-priority job under SchedStrict")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-occupantDone
	<-heavyDone
	select {
	case <-lightRan:
	case <-time.After(2 * time.Second):
		t.Fatal("light job never ran")
	}
}

// TestScheduler_BestFitUsesFreeCapacity is the SchedBestFit counterpart: the
// light job is allowed to use capacity the heavy job cannot fill yet.
func TestScheduler_BestFitUsesFreeCapacity(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 4,
		SchedPolicy:   gothrottle.SchedBestFit,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	occupied := make(chan struct{})
	release := make(chan struct{})
	occupantDone := make(chan struct{})
	go func() {
		defer close(occupantDone)
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(occupied)
			<-release
			return nil, nil
		}, 1, 2)
		if err != nil {
			t.Errorf("occupant failed: %v", err)
		}
	}()
	<-occupied

	heavyDone := make(chan struct{})
	go func() {
		defer close(heavyDone)
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 10, 4)
		if err != nil {
			t.Errorf("heavy job failed: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	lightRan := make(chan struct{})
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(lightRan)
			return nil, nil
		}, 1, 1)
		if err != nil {
			t.Errorf("light job failed: %v", err)
		}
	}()

	select {
	case <-lightRan:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("SchedBestFit did not let the light job use free capacity")
	}

	close(release)
	<-occupantDone
	<-heavyDone
}

// BenchmarkSchedulerThroughput measures dispatch cost for trivial jobs.
func BenchmarkSchedulerThroughput(b *testing.B) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 64})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := limiter.Schedule(func() (interface{}, error) { return nil, nil }); err != nil {
				b.Error(err)
			}
		}
	})
}
