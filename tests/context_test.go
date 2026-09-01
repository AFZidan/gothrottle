// FILENAME: context_test.go
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

func TestScheduleContext_CancelWhileQueued(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// Occupy the only slot so the context-bound job has to queue.
	started := make(chan struct{})
	release := make(chan struct{})
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		if _, err := limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-release
			return nil, nil
		}); err != nil {
			t.Errorf("blocking job failed: %v", err)
		}
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())

	var ran int32
	result := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleContext(ctx, func() (interface{}, error) {
			atomic.AddInt32(&ran, 1)
			return nil, nil
		})
		result <- err
	}()

	// Wait until the job is queued, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for limiter.QueueLen() == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("job never reached the queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			close(release)
			t.Fatalf("ScheduleContext error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("ScheduleContext did not return after cancellation")
	}

	// The cancelled job must be gone from the queue, not merely abandoned.
	if got := limiter.QueueLen(); got != 0 {
		close(release)
		t.Fatalf("queue length after cancellation = %d, want 0", got)
	}

	close(release)
	<-blockerDone

	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("cancelled task ran %d times, want 0", got)
	}
}

func TestScheduleContext_DeadlineWhileQueued(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	started := make(chan struct{})
	release := make(chan struct{})
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		if _, err := limiter.Schedule(func() (interface{}, error) {
			close(started)
			<-release
			return nil, nil
		}); err != nil {
			t.Errorf("blocking job failed: %v", err)
		}
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = limiter.ScheduleContext(ctx, func() (interface{}, error) { return nil, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("ScheduleContext error = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	<-blockerDone
}

func TestScheduleContext_AlreadyCancelled(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran int32
	_, err = limiter.ScheduleContext(ctx, func() (interface{}, error) {
		atomic.AddInt32(&ran, 1)
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScheduleContext error = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Fatalf("task ran %d times for an already-cancelled context, want 0", got)
	}
}

func TestScheduleContext_RunningJobCompletes(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	result := make(chan interface{}, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := limiter.ScheduleContext(ctx, func() (interface{}, error) {
			close(started)
			time.Sleep(100 * time.Millisecond)
			return "finished", nil
		})
		result <- res
		errs <- err
	}()

	<-started
	// The task cannot be interrupted, so cancelling now must report the real
	// outcome rather than a cancellation the limiter did not perform.
	cancel()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("running job error = %v, want nil", err)
		}
		if got := <-result; got != "finished" {
			t.Fatalf("running job result = %v, want \"finished\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ScheduleContext did not return")
	}
}

func TestScheduleContext_WithOptions(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	result, err := limiter.ScheduleWithOptionsContext(context.Background(), func() (interface{}, error) {
		return "weighted", nil
	}, 9, 2)
	if err != nil {
		t.Fatalf("ScheduleWithOptionsContext failed: %v", err)
	}
	if result != "weighted" {
		t.Fatalf("result = %v, want \"weighted\"", result)
	}
}

// failingDoneStore fails RegisterDone a configurable number of times so the
// retry and OnError paths are observable.
type failingDoneStore struct {
	*gothrottle.LocalStore
	mu        sync.Mutex
	failures  int
	attempts  int
	permanent bool
}

func (f *failingDoneStore) RegisterDone(id string, weight int) error {
	f.mu.Lock()
	f.attempts++
	shouldFail := f.permanent || f.failures > 0
	if f.failures > 0 {
		f.failures--
	}
	f.mu.Unlock()

	if shouldFail {
		return errors.New("simulated store failure")
	}
	return f.LocalStore.RegisterDone(id, weight)
}

func (f *failingDoneStore) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func TestLimiter_RegisterDoneIsRetried(t *testing.T) {
	store := &failingDoneStore{LocalStore: gothrottle.NewLocalStore(), failures: 2}

	var reported []error
	var mu sync.Mutex
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "retry-done",
		MaxConcurrent: 1,
		Datastore:     store,
		RetryInterval: time.Millisecond,
		OnError: func(err error) {
			mu.Lock()
			reported = append(reported, err)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	if _, err := limiter.Schedule(func() (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	// Two transient failures then success: the release must not be lost, and
	// nothing is reported because the retry recovered.
	deadline := time.Now().Add(2 * time.Second)
	for store.attemptCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("RegisterDone attempts = %d, want 3", store.attemptCount())
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 0 {
		t.Fatalf("OnError called %d times after a recovered retry, want 0: %v", len(reported), reported)
	}
}

func TestLimiter_RegisterDoneFailureIsReported(t *testing.T) {
	store := &failingDoneStore{LocalStore: gothrottle.NewLocalStore(), permanent: true}

	reported := make(chan error, 4)
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "report-done",
		MaxConcurrent: 1,
		Datastore:     store,
		RetryInterval: time.Millisecond,
		OnError:       func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	if _, err := limiter.Schedule(func() (interface{}, error) { return nil, nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	// A permanently failing release must surface instead of being discarded:
	// capacity stays reserved for work that has already finished.
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("OnError received a nil error")
		}
		if errors.Unwrap(err) == nil {
			t.Fatalf("reported error does not wrap the underlying cause: %v", err)
		}
		t.Logf("reported: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("OnError was not called for a permanently failing RegisterDone")
	}
}
