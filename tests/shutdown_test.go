// FILENAME: shutdown_test.go
package gothrottle_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// blockingStore is a Datastore whose Request can be held open, so a test can
// interleave a Stop with an in-flight capacity request.
type blockingStore struct {
	// release gates Request; when non-nil, Request blocks until it is closed.
	release chan struct{}
	// entered is closed the first time Request blocks.
	entered chan struct{}

	requests     int32
	registerDone int32
	disconnects  int32
}

func newBlockingStore() *blockingStore {
	return &blockingStore{
		release: make(chan struct{}),
		entered: make(chan struct{}),
	}
}

func (b *blockingStore) Request(_ string, _ int, _ gothrottle.Options) (bool, time.Duration, error) {
	if atomic.AddInt32(&b.requests, 1) == 1 {
		close(b.entered)
		<-b.release
	}
	return true, 0, nil
}

func (b *blockingStore) RegisterDone(_ string, _ int) error {
	atomic.AddInt32(&b.registerDone, 1)
	return nil
}

func (b *blockingStore) Disconnect() error {
	atomic.AddInt32(&b.disconnects, 1)
	return nil
}

// TestLimiter_StopPreventsWorkStartedDuringAcquisition covers the race where
// Stop is called while the datastore has already granted capacity: the task
// must not run, and the reservation must be handed back.
func TestLimiter_StopPreventsWorkStartedDuringAcquisition(t *testing.T) {
	store := newBlockingStore()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:        "stop-during-acquire",
		Datastore: store,
	})
	if err != nil {
		t.Fatal(err)
	}

	var taskRan int32
	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			atomic.AddInt32(&taskRan, 1)
			return nil, nil
		})
		scheduleDone <- err
	}()

	// Wait until the scheduler is blocked inside Datastore.Request.
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler never called Datastore.Request")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- limiter.Stop() }()

	// Give Stop time to flip the shutdown state while Request is in flight.
	time.Sleep(50 * time.Millisecond)
	close(store.release)

	select {
	case err := <-scheduleDone:
		if !errors.Is(err, gothrottle.ErrStoreClosed) {
			t.Fatalf("job error = %v, want ErrStoreClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job neither ran nor was canceled")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}

	if got := atomic.LoadInt32(&taskRan); got != 0 {
		t.Fatalf("task ran %d times after Stop, want 0", got)
	}
	// The granted reservation must be released so shared stores do not leak
	// capacity.
	if got := atomic.LoadInt32(&store.registerDone); got != 1 {
		t.Fatalf("RegisterDone called %d times, want 1 to release the reservation", got)
	}
}

// TestLimiter_ConcurrentStopAllWaitForWorkers verifies that a second Stop does
// not return early while the first is still draining running jobs.
func TestLimiter_ConcurrentStopAllWaitForWorkers(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var jobFinished int32

	scheduleDone := make(chan error, 1)
	go func() {
		_, err := limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-release
			atomic.StoreInt32(&jobFinished, 1)
			return nil, nil
		})
		scheduleDone <- err
	}()

	<-started

	const stoppers = 4
	stopErrs := make(chan error, stoppers)
	finishedBeforeJob := make(chan int, stoppers)
	for i := 0; i < stoppers; i++ {
		go func() {
			err := limiter.Stop()
			finishedBeforeJob <- int(atomic.LoadInt32(&jobFinished))
			stopErrs <- err
		}()
	}

	// No Stop caller may return while the job is still running.
	select {
	case <-stopErrs:
		t.Fatal("Stop returned while a job was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	for i := 0; i < stoppers; i++ {
		select {
		case err := <-stopErrs:
			if err != nil {
				t.Fatalf("Stop returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a concurrent Stop call did not return")
		}

		if observed := <-finishedBeforeJob; observed != 1 {
			t.Fatal("Stop returned before the running job completed")
		}
	}

	if err := <-scheduleDone; err != nil {
		t.Fatalf("job returned error: %v", err)
	}
}

// countingStore records Disconnect calls so ownership behavior is observable.
type countingStore struct {
	*gothrottle.LocalStore
	disconnects int32
}

func newCountingStore() *countingStore {
	return &countingStore{LocalStore: gothrottle.NewLocalStore()}
}

func (c *countingStore) Disconnect() error {
	atomic.AddInt32(&c.disconnects, 1)
	return c.LocalStore.Disconnect()
}

// TestLimiter_StopLeavesInjectedDatastoreOpen documents the ownership rule: a
// caller-provided datastore survives Stop, so limiters and other components
// sharing it keep working.
func TestLimiter_StopLeavesInjectedDatastoreOpen(t *testing.T) {
	store := newCountingStore()

	first, err := gothrottle.NewLimiter(gothrottle.Options{ID: "shared-a", Datastore: store})
	if err != nil {
		t.Fatal(err)
	}
	second, err := gothrottle.NewLimiter(gothrottle.Options{ID: "shared-b", Datastore: store})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Stop() }()

	if _, err := first.Schedule(func() (interface{}, error) { return "a", nil }); err != nil {
		t.Fatalf("first limiter job failed: %v", err)
	}

	if err := first.Stop(); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if got := atomic.LoadInt32(&store.disconnects); got != 0 {
		t.Fatalf("injected datastore was disconnected %d times, want 0", got)
	}

	// The second limiter must still be able to schedule work.
	result, err := second.Schedule(func() (interface{}, error) { return "b", nil })
	if err != nil {
		t.Fatalf("second limiter job failed after first limiter stopped: %v", err)
	}
	if result != "b" {
		t.Fatalf("second limiter result = %v, want \"b\"", result)
	}
}

// TestLimiter_StopClosesInjectedDatastoreWhenOptedIn covers the explicit
// ownership transfer.
func TestLimiter_StopClosesInjectedDatastoreWhenOptedIn(t *testing.T) {
	store := newCountingStore()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:                   "owned-store",
		Datastore:            store,
		CloseDatastoreOnStop: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := limiter.Stop(); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if got := atomic.LoadInt32(&store.disconnects); got != 1 {
		t.Fatalf("datastore disconnects = %d, want 1", got)
	}
}

// TestLimiter_StopClosesOwnDefaultStore verifies the limiter still cleans up
// the LocalStore it creates for itself.
func TestLimiter_StopClosesOwnDefaultStore(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Stop(); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	// A second Stop is a no-op and must not report a "closed store" error from
	// disconnecting twice.
	if err := limiter.Stop(); err != nil {
		t.Fatalf("second Stop returned error: %v", err)
	}
}
