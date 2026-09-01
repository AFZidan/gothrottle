// FILENAME: adversarial_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// TestAdversarial_MinTimeIsEnforcedServerSide pins the reason timing moved into
// Redis. Spacing is measured by the Redis server, so two "instances" — here two
// independent clients and stores, as separate processes would be — agree on the
// window without agreeing on the time, and the window advances with Redis's
// clock rather than any caller's.
func TestAdversarial_MinTimeIsEnforcedServerSide(t *testing.T) {
	clientA := newTestRedisClient(t)
	clientB := newTestRedisClient(t)

	storeA, err := gothrottle.NewRedisStore(clientA)
	if err != nil {
		t.Fatalf("NewRedisStore(A) failed: %v", err)
	}
	defer func() { _ = storeA.Disconnect() }()

	storeB, err := gothrottle.NewRedisStore(clientB)
	if err != nil {
		t.Fatalf("NewRedisStore(B) failed: %v", err)
	}
	defer func() { _ = storeB.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("server-side-mintime")
	opts := gothrottle.Options{MinTime: 400 * time.Millisecond}

	redisBefore, err := clientA.Time(ctx).Result()
	if err != nil {
		t.Fatalf("TIME failed: %v", err)
	}

	first, _, err := storeA.Acquire(ctx, id, 1, opts)
	if err != nil || first == nil {
		t.Fatalf("Acquire(A) = (%v, %v), want a lease", first, err)
	}
	if err := storeA.Release(ctx, first); err != nil {
		t.Fatalf("Release(A) failed: %v", err)
	}

	// A different instance must see the same window: the start timestamp lives
	// in Redis, not in instance A's memory or clock.
	second, retryAfter, err := storeB.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatal("a second instance skipped the MinTime window written by the first")
	}
	if retryAfter <= 0 || retryAfter > opts.MinTime {
		t.Fatalf("retryAfter = %v, want in (0, %v]", retryAfter, opts.MinTime)
	}

	// The remaining wait must shrink as Redis's clock advances, which is what
	// proves the countdown is server-side.
	time.Sleep(150 * time.Millisecond)
	_, laterRetry, err := storeB.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if laterRetry >= retryAfter {
		t.Fatalf("remaining wait did not shrink: %v then %v", retryAfter, laterRetry)
	}

	time.Sleep(laterRetry + 50*time.Millisecond)

	third, _, err := storeB.Acquire(ctx, id, 1, opts)
	if err != nil || third == nil {
		t.Fatalf("Acquire after the window = (%v, %v), want a lease", third, err)
	}

	redisAfter, err := clientA.Time(ctx).Result()
	if err != nil {
		t.Fatalf("TIME failed: %v", err)
	}
	if !redisAfter.After(redisBefore) {
		t.Fatalf("Redis clock did not advance across the test: %v to %v", redisBefore, redisAfter)
	}
}

// TestAdversarial_DistributedLimitAcrossInstances runs several limiters against
// one Redis store, as separate application instances would, and checks the
// global concurrency limit holds.
func TestAdversarial_DistributedLimitAcrossInstances(t *testing.T) {
	client := newTestRedisClient(t)

	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	const (
		instances     = 4
		jobsPer       = 10
		maxConcurrent = 3
	)
	id := uniqueLimiterID("distributed-limit")

	var running int32
	var peak int32
	var overLimit int32

	limiters := make([]*gothrottle.Limiter, 0, instances)
	for i := 0; i < instances; i++ {
		limiter, err := gothrottle.NewLimiter(gothrottle.Options{
			ID:            id,
			MaxConcurrent: maxConcurrent,
			Datastore:     store,
			RetryInterval: 5 * time.Millisecond,
			LeaseTTL:      5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		limiters = append(limiters, limiter)
	}
	defer func() {
		for _, limiter := range limiters {
			_ = limiter.Stop()
		}
	}()

	var wg sync.WaitGroup
	for _, limiter := range limiters {
		for j := 0; j < jobsPer; j++ {
			wg.Add(1)
			go func(l *gothrottle.Limiter) {
				defer wg.Done()
				_, err := l.Schedule(func() (interface{}, error) {
					current := atomic.AddInt32(&running, 1)
					if current > maxConcurrent {
						atomic.AddInt32(&overLimit, 1)
					}
					for {
						peaked := atomic.LoadInt32(&peak)
						if current <= peaked || atomic.CompareAndSwapInt32(&peak, peaked, current) {
							break
						}
					}
					time.Sleep(15 * time.Millisecond)
					atomic.AddInt32(&running, -1)
					return nil, nil
				})
				if err != nil {
					t.Errorf("Schedule failed: %v", err)
				}
			}(limiter)
		}
	}
	wg.Wait()

	if got := atomic.LoadInt32(&overLimit); got != 0 {
		t.Fatalf("the global limit of %d was exceeded %d times (peak %d)", maxConcurrent, got, atomic.LoadInt32(&peak))
	}
	t.Logf("peak concurrency across %d instances: %d (limit %d)", instances, atomic.LoadInt32(&peak), maxConcurrent)
}

// TestAdversarial_SharedStoreIsolationBetweenLimiterIDs checks that two limiters
// with different IDs on one store do not consume each other's capacity.
func TestAdversarial_SharedStoreIsolationBetweenLimiterIDs(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		first := uniqueLimiterID("isolation-a")
		second := uniqueLimiterID("isolation-b")
		opts := gothrottle.Options{MaxConcurrent: 1}

		a, _, err := store.Acquire(ctx, first, 1, opts)
		if err != nil || a == nil {
			t.Fatalf("Acquire(a) = (%v, %v), want a lease", a, err)
		}

		b, _, err := store.Acquire(ctx, second, 1, opts)
		if err != nil || b == nil {
			t.Fatalf("Acquire(b) = (%v, %v), want a lease; limiter IDs must not share capacity", b, err)
		}

		// Releasing one limiter's lease must not free the other's.
		if err := store.Release(ctx, a); err != nil {
			t.Fatalf("Release(a) failed: %v", err)
		}
		blocked, _, err := store.Acquire(ctx, second, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if blocked != nil {
			t.Fatal("releasing limiter a's lease freed limiter b's capacity")
		}
	})
}

// slowAcquireStore delays Acquire so a test can call Stop while a lease request
// is in flight against a lease-based store.
type slowAcquireStore struct {
	gothrottle.LeaseDatastore
	delay    time.Duration
	entered  chan struct{}
	once     sync.Once
	acquired int32
	released int32
}

func (s *slowAcquireStore) Acquire(ctx context.Context, id string, weight int, opts gothrottle.Options) (*gothrottle.Lease, time.Duration, error) {
	s.once.Do(func() { close(s.entered) })
	time.Sleep(s.delay)

	lease, retryAfter, err := s.LeaseDatastore.Acquire(ctx, id, weight, opts)
	if lease != nil {
		atomic.AddInt32(&s.acquired, 1)
	}
	return lease, retryAfter, err
}

func (s *slowAcquireStore) Release(ctx context.Context, lease *gothrottle.Lease) error {
	atomic.AddInt32(&s.released, 1)
	return s.LeaseDatastore.Release(ctx, lease)
}

// TestAdversarial_ShutdownDuringLeaseAcquisition is the lease-path version of
// the shutdown race: a lease granted while Stop is running must be released, not
// leaked, and its task must not run.
func TestAdversarial_ShutdownDuringLeaseAcquisition(t *testing.T) {
	store := &slowAcquireStore{
		LeaseDatastore: gothrottle.NewLocalStore(),
		delay:          150 * time.Millisecond,
		entered:        make(chan struct{}),
	}

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("shutdown-during-acquire"),
		MaxConcurrent: 1,
		Datastore:     store,
	})
	if err != nil {
		t.Fatal(err)
	}

	var ran int32
	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			atomic.AddInt32(&ran, 1)
			return nil, nil
		})
		scheduleDone <- err
	}()

	<-store.entered

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()

	select {
	case err := <-scheduleDone:
		if !errors.Is(err, gothrottle.ErrStoreClosed) {
			t.Fatalf("job error = %v, want ErrStoreClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job neither ran nor was cancelled")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}

	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("task ran %d times after Stop, want 0", got)
	}
	if acquired, released := atomic.LoadInt32(&store.acquired), atomic.LoadInt32(&store.released); acquired != released {
		t.Fatalf("%d leases acquired but %d released; capacity leaked on shutdown", acquired, released)
	}
}

// TestAdversarial_HighContentionRespectsLimit hammers a single limiter to check
// the concurrency window is never exceeded under load.
func TestAdversarial_HighContentionRespectsLimit(t *testing.T) {
	const (
		maxConcurrent = 5
		jobs          = 500
	)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var running int32
	var overLimit int32
	var completed int32

	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := limiter.Schedule(func() (interface{}, error) {
				if atomic.AddInt32(&running, 1) > maxConcurrent {
					atomic.AddInt32(&overLimit, 1)
				}
				atomic.AddInt32(&running, -1)
				atomic.AddInt32(&completed, 1)
				return nil, nil
			})
			if err != nil {
				t.Errorf("Schedule failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&overLimit); got != 0 {
		t.Fatalf("MaxConcurrent %d exceeded %d times", maxConcurrent, got)
	}
	if got := atomic.LoadInt32(&completed); got != jobs {
		t.Fatalf("completed %d jobs, want %d", got, jobs)
	}
}

// TestAdversarial_WeightedContentionRespectsLimit does the same with mixed
// weights, where the capacity accounting is easier to get wrong.
func TestAdversarial_WeightedContentionRespectsLimit(t *testing.T) {
	const maxConcurrent = 10

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: maxConcurrent,
		SchedPolicy:   gothrottle.SchedBestFit,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var inFlight int32
	var overLimit int32

	weights := []int{1, 2, 3, 4, 5}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		weight := weights[i%len(weights)]
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				if atomic.AddInt32(&inFlight, int32(w)) > maxConcurrent {
					atomic.AddInt32(&overLimit, 1)
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&inFlight, -int32(w))
				return nil, nil
			}, 5, w)
			if err != nil {
				t.Errorf("ScheduleWithOptions(weight %d) failed: %v", w, err)
			}
		}(weight)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&overLimit); got != 0 {
		t.Fatalf("total weight exceeded MaxConcurrent %d on %d occasions", maxConcurrent, got)
	}
}

// TestAdversarial_MinTimeUnderContention checks spacing holds when many
// goroutines compete, not just in the single-threaded case.
func TestAdversarial_MinTimeUnderContention(t *testing.T) {
	const minTime = 40 * time.Millisecond

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MinTime: minTime})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var mu sync.Mutex
	var starts []time.Time

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := limiter.Schedule(func() (interface{}, error) {
				mu.Lock()
				starts = append(starts, time.Now())
				mu.Unlock()
				return nil, nil
			})
			if err != nil {
				t.Errorf("Schedule failed: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(starts); i++ {
		// Sort is unnecessary: each job records its start under the lock, and
		// spacing is enforced between consecutive starts.
		gap := starts[i].Sub(starts[i-1])
		if gap < 0 {
			gap = -gap
		}
		// Allow a small tolerance for scheduling jitter on loaded CI machines.
		if gap < minTime-5*time.Millisecond {
			t.Fatalf("jobs started %v apart, want at least %v", gap, minTime)
		}
	}
}

// TestAdversarial_StopWithFullQueueUnblocksEveryCaller checks that shutdown
// leaves no caller hanging.
func TestAdversarial_StopWithFullQueueUnblocksEveryCaller(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-release
			return nil, nil
		})
	}()
	<-started

	const waiters = 50
	results := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			_, err := limiter.Schedule(func() (interface{}, error) { return nil, nil })
			results <- err
		}()
	}

	// Wait until most callers are queued, then shut down.
	deadline := time.Now().Add(2 * time.Second)
	for limiter.QueueLen() < waiters/2 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()
	close(release)

	for i := 0; i < waiters; i++ {
		select {
		case err := <-results:
			// Each caller either ran before shutdown or was cancelled; nobody
			// may be left blocked.
			if err != nil && !errors.Is(err, gothrottle.ErrStoreClosed) {
				t.Fatalf("queued caller error = %v, want nil or ErrStoreClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d queued callers were unblocked by Stop", i, waiters)
		}
	}

	if err := <-stopDone; err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
}

// TestAdversarial_PanicDoesNotLeakCapacity checks a panicking task still hands
// its reservation back.
func TestAdversarial_PanicDoesNotLeakCapacity(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		id := uniqueLimiterID("panic-capacity")

		limiter, err := gothrottle.NewLimiter(gothrottle.Options{
			ID:            id,
			MaxConcurrent: 1,
			Datastore:     store,
			RetryInterval: 5 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Stop() }()

		for i := 0; i < 5; i++ {
			_, err := limiter.Schedule(func() (interface{}, error) {
				panic("boom")
			})
			if !errors.Is(err, gothrottle.ErrTaskPanic) {
				t.Fatalf("panic %d error = %v, want ErrTaskPanic", i, err)
			}
		}

		// If a panicking job leaked its lease, the single slot would now be
		// permanently occupied.
		done := make(chan error, 1)
		go func() {
			_, err := limiter.Schedule(func() (interface{}, error) { return "ok", nil })
			done <- err
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("job after panics failed: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("capacity was leaked by panicking jobs")
		}
	})
}
