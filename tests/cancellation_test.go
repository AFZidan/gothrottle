// FILENAME: cancellation_test.go
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

// The limiter owns a context that is cancelled when Stop begins, and passes it
// to the LeaseDatastore calls that reserve capacity. Without that, a store
// blocked on a network round trip keeps the scheduler — and therefore Stop —
// waiting indefinitely. Releases are deliberately exempt: capacity has to be
// handed back precisely because shutdown is happening, so they get a bounded
// context of their own.
//
// These tests use blocking fakes with channel handshakes rather than sleeps, so
// the ordering they assert is established rather than hoped for.

// gateStore is a LeaseDatastore whose Acquire, Renew and Release can each be held
// open until the test lets them through, and which records what the limiter
// passed. It delegates to a LocalStore so the accounting stays real.
type gateStore struct {
	gothrottle.LeaseDatastore

	// acquireEntered is closed when Acquire is first called; acquireRelease, if
	// non-nil, gates it. A blocked Acquire returns as soon as either the gate
	// opens or its context ends, which is the behavior a real store has.
	acquireEntered chan struct{}
	acquireOnce    sync.Once
	acquireRelease chan struct{}

	// renewEntered receives once per Renew call, and renewRelease gates it.
	renewEntered chan struct{}
	renewRelease chan struct{}

	// releaseEntered receives once per Release call.
	releaseEntered chan struct{}

	acquires         int32
	acquireCancelled int32
	renews           int32
	renewCancelled   int32
	releases         int32
	releaseHadCtx    int32
	releaseCtxLive   int32
}

func newGateStore() *gateStore {
	return &gateStore{
		LeaseDatastore: gothrottle.NewLocalStore(),
		acquireEntered: make(chan struct{}),
		renewEntered:   make(chan struct{}, 64),
		releaseEntered: make(chan struct{}, 64),
	}
}

func (g *gateStore) Acquire(ctx context.Context, id string, weight int, opts gothrottle.Options) (*gothrottle.Lease, time.Duration, error) {
	atomic.AddInt32(&g.acquires, 1)
	g.acquireOnce.Do(func() { close(g.acquireEntered) })

	if g.acquireRelease != nil {
		select {
		case <-g.acquireRelease:
		case <-ctx.Done():
			atomic.AddInt32(&g.acquireCancelled, 1)
			return nil, 0, ctx.Err()
		}
	}
	return g.LeaseDatastore.Acquire(ctx, id, weight, opts)
}

func (g *gateStore) Renew(ctx context.Context, lease *gothrottle.Lease) error {
	atomic.AddInt32(&g.renews, 1)
	select {
	case g.renewEntered <- struct{}{}:
	default:
	}

	if g.renewRelease != nil {
		select {
		case <-g.renewRelease:
		case <-ctx.Done():
			atomic.AddInt32(&g.renewCancelled, 1)
			return ctx.Err()
		}
	}
	return g.LeaseDatastore.Renew(ctx, lease)
}

func (g *gateStore) Release(ctx context.Context, lease *gothrottle.Lease) error {
	atomic.AddInt32(&g.releases, 1)
	if ctx != nil {
		atomic.AddInt32(&g.releaseHadCtx, 1)
		if ctx.Err() == nil {
			atomic.AddInt32(&g.releaseCtxLive, 1)
		}
	}
	select {
	case g.releaseEntered <- struct{}{}:
	default:
	}
	return g.LeaseDatastore.Release(ctx, lease)
}

// TestCancel_StopUnblocksAcquire is the core case: a store waiting on ctx.Done()
// inside Acquire must be released by Stop, and the queued caller must be told the
// limiter shut down rather than shown a raw context error.
func TestCancel_StopUnblocksAcquire(t *testing.T) {
	store := newGateStore()
	// Never opened: only cancellation can end this Acquire.
	store.acquireRelease = make(chan struct{})

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-acquire"),
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

	<-store.acquireEntered

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return; a blocked Acquire was not cancelled")
	}

	select {
	case err := <-scheduleDone:
		// The job never ran, so the caller gets the terminal shutdown error,
		// not an ambiguous datastore failure.
		if !errors.Is(err, gothrottle.ErrStoreClosed) {
			t.Fatalf("job error = %v, want ErrStoreClosed", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("job error surfaced the internal cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Schedule never returned")
	}

	if got := atomic.LoadInt32(&store.acquireCancelled); got == 0 {
		t.Fatal("Acquire was not cancelled; the limiter passed a context that never ends")
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("task ran %d times after shutdown, want 0", got)
	}
}

// TestCancel_RenewalExitsPromptly checks a renewal blocked in the store is
// cancelled when the job it belongs to finishes, rather than the worker waiting
// for the call to come back on its own.
func TestCancel_RenewalExitsPromptly(t *testing.T) {
	store := newGateStore()
	// Never opened: a renewal that gets in must be cancelled to get out.
	store.renewRelease = make(chan struct{})

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-renew"),
		MaxConcurrent: 1,
		Datastore:     store,
		// The 1s floor gives a ~333ms renewal interval.
		LeaseTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	taskDone := make(chan struct{})
	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			// Stay running until a renewal is in flight and blocked.
			<-store.renewEntered
			close(taskDone)
			return nil, nil
		})
		scheduleDone <- err
	}()

	select {
	case <-taskDone:
	case <-time.After(3 * time.Second):
		t.Fatal("no renewal was attempted while the job ran")
	}

	// The task has returned with a renewal still blocked inside the store. If
	// stopping renewal waited for that call rather than cancelling it, Schedule
	// would never return.
	select {
	case err := <-scheduleDone:
		if err != nil {
			t.Fatalf("job returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job completion blocked on an in-flight renewal")
	}

	// The result reaches the caller before the worker finishes its cleanup, so
	// wait for shutdown — which waits for the worker, and therefore for the
	// renewal goroutine — before reading what the store observed.
	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked on an in-flight renewal")
	}

	if got := atomic.LoadInt32(&store.renewCancelled); got == 0 {
		t.Fatal("the blocked renewal was not cancelled")
	}
}

// TestCancel_ReleaseSurvivesShutdown is the counterpart guarantee: shutdown must
// not cancel the release, or capacity would leak from a shared store exactly when
// a process is going away.
func TestCancel_ReleaseSurvivesShutdown(t *testing.T) {
	store := newGateStore()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-release"),
		MaxConcurrent: 1,
		Datastore:     store,
		LeaseTTL:      2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	finish := make(chan struct{})
	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-finish
			return nil, nil
		})
		scheduleDone <- err
	}()
	<-started

	// Shut down with the job running, then let it finish. Its release happens
	// after cancellation, which is the case that used to be at risk.
	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()
	time.Sleep(50 * time.Millisecond)
	close(finish)

	if err := <-scheduleDone; err != nil {
		t.Fatalf("job returned error: %v", err)
	}
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}

	if got := atomic.LoadInt32(&store.releases); got != 1 {
		t.Fatalf("Release called %d times during shutdown, want 1", got)
	}
	if got := atomic.LoadInt32(&store.releaseHadCtx); got != 1 {
		t.Fatalf("Release received a nil context %d times", 1-got)
	}
	if got := atomic.LoadInt32(&store.releaseCtxLive); got != 1 {
		t.Fatal("Release was handed an already-cancelled context; the reservation could not be returned")
	}

	// The capacity really is back: a fresh limiter on the same store and ID can
	// take the single slot.
	fresh, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-release-successor"),
		MaxConcurrent: 1,
		Datastore:     store,
		LeaseTTL:      2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Stop() }()
	if _, err := fresh.Schedule(func() (interface{}, error) { return "ok", nil }); err != nil {
		t.Fatalf("Schedule after shutdown release failed: %v", err)
	}
}

// blockingReleaseStore hangs in Release regardless of context, which is what a
// wedged custom store does. Stop must still complete.
type blockingReleaseStore struct {
	gothrottle.LeaseDatastore
	entered  chan struct{}
	once     sync.Once
	attempts int32
}

func (b *blockingReleaseStore) Release(ctx context.Context, lease *gothrottle.Lease) error {
	atomic.AddInt32(&b.attempts, 1)
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// TestCancel_ReleaseDoesNotBlockForever checks the release budget: a store that
// never answers is given a bounded window, reported through OnError, and then
// abandoned so Stop can finish.
func TestCancel_ReleaseDoesNotBlockForever(t *testing.T) {
	store := &blockingReleaseStore{
		LeaseDatastore: gothrottle.NewLocalStore(),
		entered:        make(chan struct{}),
	}

	reported := make(chan error, 4)
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-release-wedged"),
		MaxConcurrent: 1,
		Datastore:     store,
		// The budget is max(LeaseTTL, 5s), so this bounds the test at ~5s.
		LeaseTTL:      time.Second,
		RetryInterval: 5 * time.Millisecond,
		OnError:       func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := limiter.Schedule(func() (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Release was never attempted")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Stop blocked on a wedged Release")
	}

	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("OnError received a nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("a release that never completed was not reported through OnError")
	}
}

// TestCancel_NoTaskStartsAfterShutdown checks the window between "capacity
// granted" and "worker launched" under contention: many callers, one slot, Stop
// while work is in flight. Every caller must either finish or be refused, and
// nothing may start once shutdown is observable.
func TestCancel_NoTaskStartsAfterShutdown(t *testing.T) {
	store := newGateStore()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-no-late-start"),
		MaxConcurrent: 1,
		Datastore:     store,
		LeaseTTL:      2 * time.Second,
		RetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	var stopObserved int32
	var lateStarts int32
	var started int32

	const callers = 40
	results := make(chan error, callers)
	block := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			_, err := limiter.Schedule(func() (interface{}, error) {
				if atomic.LoadInt32(&stopObserved) == 1 {
					atomic.AddInt32(&lateStarts, 1)
				}
				atomic.AddInt32(&started, 1)
				<-block
				return nil, nil
			})
			results <- err
		}()
	}

	// Wait until work is genuinely in flight, then shut down.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&started) == 0 {
		if time.Now().After(deadline) {
			close(block)
			t.Fatal("no job ever started")
		}
		time.Sleep(time.Millisecond)
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()

	// Establish the ordering rather than assuming it: a fresh Schedule returning
	// ErrStoreClosed proves the limiter has already marked itself stopped, so
	// from here on no dispatch may begin. Setting the flag before that would
	// race with a dispatch that started legitimately.
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := limiter.Schedule(func() (interface{}, error) { return nil, nil }); errors.Is(err, gothrottle.ErrStoreClosed) {
			break
		}
		if time.Now().After(deadline) {
			close(block)
			t.Fatal("Stop never became observable to new callers")
		}
		time.Sleep(time.Millisecond)
	}
	atomic.StoreInt32(&stopObserved, 1)

	close(block)

	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, gothrottle.ErrStoreClosed) {
				t.Fatalf("caller error = %v, want nil or ErrStoreClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d callers were unblocked", i, callers)
		}
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}

	if got := atomic.LoadInt32(&lateStarts); got != 0 {
		t.Fatalf("%d tasks started after shutdown was observable", got)
	}
	// Every reservation the store handed out came back.
	if acquires, releases := atomic.LoadInt32(&store.acquires), atomic.LoadInt32(&store.releases); releases > acquires {
		t.Fatalf("%d releases for %d acquisitions", releases, acquires)
	}
}

// TestCancel_ConcurrentStopSharesOneResult checks Stop remains a single shutdown
// with a shared outcome even when an Acquire has to be cancelled to get there.
func TestCancel_ConcurrentStopSharesOneResult(t *testing.T) {
	store := newGateStore()
	store.acquireRelease = make(chan struct{})

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            uniqueLimiterID("cancel-concurrent-stop"),
		MaxConcurrent: 1,
		Datastore:     store,
	})
	if err != nil {
		t.Fatal(err)
	}

	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) { return nil, nil })
		scheduleDone <- err
	}()
	<-store.acquireEntered

	const stoppers = 8
	errs := make(chan error, stoppers)
	var wg sync.WaitGroup
	for i := 0; i < stoppers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- limiter.Stop()
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop callers did not all return")
	}

	for i := 0; i < stoppers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Stop caller %d returned error: %v", i, err)
		}
	}

	if err := <-scheduleDone; !errors.Is(err, gothrottle.ErrStoreClosed) {
		t.Fatalf("job error = %v, want ErrStoreClosed", err)
	}
}

// TestCancel_ScheduleContextStillReturnsCallerError pins the distinction between
// the caller's context and the limiter's. A caller cancelling while queued gets
// context.Canceled — not ErrStoreClosed — because the limiter is still running.
func TestCancel_ScheduleContextStillReturnsCallerError(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	occupied := make(chan struct{})
	release := make(chan struct{})
	blocker := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(occupied)
			<-release
			return nil, nil
		})
		blocker <- err
	}()
	<-occupied

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleContext(ctx, func() (interface{}, error) { return nil, nil })
		queued <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for limiter.QueueLen() == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the second job never reached the queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			close(release)
			t.Fatalf("queued caller error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("cancelled caller did not return")
	}

	close(release)
	if err := <-blocker; err != nil {
		t.Fatalf("blocking job returned error: %v", err)
	}
}
