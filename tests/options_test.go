// FILENAME: options_test.go
package gothrottle_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

func TestOptions_RejectsNegativeLimits(t *testing.T) {
	tests := []struct {
		name string
		opts gothrottle.Options
		want error
	}{
		{
			name: "negative MaxConcurrent",
			opts: gothrottle.Options{MaxConcurrent: -1},
			want: gothrottle.ErrInvalidMaxConcurrent,
		},
		{
			name: "negative MinTime",
			opts: gothrottle.Options{MinTime: -time.Second},
			want: gothrottle.ErrInvalidMinTime,
		},
		{
			name: "negative MaxQueueSize",
			opts: gothrottle.Options{MaxQueueSize: -1},
			want: gothrottle.ErrInvalidMaxQueueSize,
		},
		{
			name: "negative RetryInterval",
			opts: gothrottle.Options{RetryInterval: -time.Millisecond},
			want: gothrottle.ErrInvalidRetryInterval,
		},
		{
			// LeaseTTL had a sentinel and a Validate check but no test. A negative
			// value falls through leaseTTL()'s `<= 0` branch to the 30s default, so
			// the mistake would be silently corrected rather than reported.
			name: "negative LeaseTTL",
			opts: gothrottle.Options{LeaseTTL: -time.Second},
			want: gothrottle.ErrInvalidLeaseTTL,
		},
		{
			name: "unknown SchedPolicy",
			opts: gothrottle.Options{SchedPolicy: gothrottle.SchedPolicy(42)},
			want: gothrottle.ErrInvalidSchedPolicy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}

			// A negative limit used to be silently treated as "unlimited", so
			// the limiter must refuse to be constructed with one.
			limiter, err := gothrottle.NewLimiter(tc.opts)
			if !errors.Is(err, tc.want) {
				if limiter != nil {
					_ = limiter.Stop()
				}
				t.Fatalf("NewLimiter() = %v, want %v", err, tc.want)
			}
			if limiter != nil {
				t.Fatal("NewLimiter returned a limiter alongside an error")
			}
		})
	}
}

func TestOptions_ZeroValuesAreValid(t *testing.T) {
	// Zero still means "no limit" / "use the default"; only negatives are errors.
	if err := (gothrottle.Options{}).Validate(); err != nil {
		t.Fatalf("zero Options.Validate() = %v, want nil", err)
	}
}

func TestLimiter_MaxQueueSizeRejectsOverflow(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 1,
		MaxQueueSize:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// Occupy the only slot so everything else queues.
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

	// Fill the queue.
	queued := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := limiter.Schedule(func() (interface{}, error) { return nil, nil })
			queued <- err
		}()
	}

	// Wait until both jobs are actually queued before testing overflow.
	deadline := time.Now().Add(2 * time.Second)
	for limiter.QueueLen() < 2 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("only %d of 2 jobs queued", limiter.QueueLen())
		}
		time.Sleep(time.Millisecond)
	}

	// The queue is full, so the next submission must be refused rather than
	// growing the queue without bound.
	_, overflow := limiter.Schedule(func() (interface{}, error) { return nil, nil })
	if !errors.Is(overflow, gothrottle.ErrQueueFull) {
		close(release)
		t.Fatalf("Schedule on full queue = %v, want ErrQueueFull", overflow)
	}

	close(release)
	<-blockerDone
	for i := 0; i < 2; i++ {
		if err := <-queued; err != nil {
			t.Fatalf("queued job failed: %v", err)
		}
	}
}

func TestLimiter_PanicErrorCarriesStack(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	_, err = limiter.Schedule(func() (interface{}, error) {
		panic("boom")
	})

	// The sentinel must keep matching so existing error checks still work.
	if !errors.Is(err, gothrottle.ErrTaskPanic) {
		t.Fatalf("panic error = %v, want to match ErrTaskPanic", err)
	}

	var panicErr *gothrottle.PanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("panic error = %T, want *gothrottle.PanicError", err)
	}
	if panicErr.Value != "boom" {
		t.Fatalf("PanicError.Value = %v, want \"boom\"", panicErr.Value)
	}
	if len(panicErr.Stack) == 0 {
		t.Fatal("PanicError.Stack is empty")
	}
	if !strings.Contains(string(panicErr.Stack), "gothrottle") {
		t.Fatalf("PanicError.Stack does not mention the package:\n%s", panicErr.Stack)
	}
}

func TestLocalStore_RejectsOverweightJob(t *testing.T) {
	store := gothrottle.NewLocalStore()

	// RedisStore already returned ErrWeightExceedsMax here. LocalStore used to
	// return canRun=false with no error, which would spin in the scheduler.
	_, _, err := store.Request("weights", 5, gothrottle.Options{MaxConcurrent: 2})
	if !errors.Is(err, gothrottle.ErrWeightExceedsMax) {
		t.Fatalf("LocalStore.Request(weight>max) = %v, want ErrWeightExceedsMax", err)
	}
}

func TestLocalStore_RejectsMalformedID(t *testing.T) {
	store := gothrottle.NewLocalStore()

	if _, _, err := store.Request("bad\x00id", 1, gothrottle.Options{}); !errors.Is(err, gothrottle.ErrInvalidID) {
		t.Fatalf("LocalStore.Request(malformed id) = %v, want ErrInvalidID", err)
	}
	if err := store.RegisterDone("bad\x00id", 1); !errors.Is(err, gothrottle.ErrInvalidID) {
		t.Fatalf("LocalStore.RegisterDone(malformed id) = %v, want ErrInvalidID", err)
	}
}

func TestLocalStore_SubMillisecondMinTime(t *testing.T) {
	store := gothrottle.NewLocalStore()
	opts := gothrottle.Options{MinTime: 500 * time.Microsecond}

	if canRun, _, err := store.Request("submilli", 1, opts); err != nil || !canRun {
		t.Fatalf("first Request = (%v, %v), want (true, nil)", canRun, err)
	}

	canRun, waitTime, err := store.Request("submilli", 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if canRun {
		t.Fatal("second Request should be refused by a sub-millisecond MinTime")
	}
	if waitTime <= 0 || waitTime > 500*time.Microsecond {
		t.Fatalf("waitTime = %v, want in (0, 500µs]", waitTime)
	}
}
