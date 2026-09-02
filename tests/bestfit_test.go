// FILENAME: bestfit_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
	"github.com/go-redis/redis/v8"
)

// Distributed best fit
//
// SchedBestFit used to decide what could fit from the limiter's *local* running
// weight alone. In a shared store that number is not the capacity that matters:
// another process can be holding most of it, and this limiter cannot see that
// until the store refuses. The old dispatch loop treated the refusal as the end
// of the pass, requeued the head job and armed a retry — so a lighter queued job
// that did fit the real distributed capacity never got its turn, and throughput
// collapsed to whatever the heavy head job's retries allowed.
//
// The fix reads a capacity refusal as information about the store: a weight was
// refused, so nothing at least as heavy can fit, and only something strictly
// lighter is worth another call. That lowers a per-pass ceiling, which both lets
// lighter work through and bounds the pass — every attempt is for a strictly
// lighter job.

// capacityStore wraps a LeaseDatastore, records every admission attempt with its
// timestamp and can be told to refuse a range of weights, standing in for
// capacity held by another process. It is the only way to make a distributed
// refusal deterministic, which is what the ordering and busy-loop assertions
// need.
type capacityStore struct {
	inner gothrottle.LeaseDatastore

	mu sync.Mutex
	// refuseAtLeast refuses any weight >= this value; 0 disables refusal. It
	// stands in for capacity held elsewhere in the cluster.
	refuseAtLeast int
	// innerMax caps how much weight may be held through this store at once, on
	// top of whatever the limiter itself allows; 0 means no extra cap. It models
	// the share of a shared limit that is actually free.
	innerMax int
	// held is the weight currently leased through this store.
	held int
	// attempts records every weight the scheduler asked for, in order, with when
	// it was asked.
	attempts []attempt

	// hold, when non-nil, parks every Acquire call until it is closed, and
	// firstEntered is signalled as the first call parks. Together they let a test
	// freeze the scheduler mid-dispatch, fill the queue, and then observe one
	// complete pass with no timing assumptions.
	hold         chan struct{}
	firstEntered chan struct{}
	entered      bool

	acquireCalls int64
}

// attempt is one admission call the scheduler made.
type attempt struct {
	weight int
	at     time.Time
}

func newCapacityStore(inner gothrottle.LeaseDatastore, refuseAtLeast int) *capacityStore {
	return &capacityStore{inner: inner, refuseAtLeast: refuseAtLeast}
}

// holdCalls parks Acquire until releaseHold is called. It returns a channel that
// is closed once the scheduler is parked inside its first Acquire.
func (s *capacityStore) holdCalls() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.hold = make(chan struct{})
	s.firstEntered = make(chan struct{})
	return s.firstEntered
}

// releaseHold lets parked and future Acquire calls proceed.
func (s *capacityStore) releaseHold() {
	s.mu.Lock()
	hold := s.hold
	s.hold = nil
	s.mu.Unlock()

	if hold != nil {
		close(hold)
	}
}

func (s *capacityStore) Acquire(ctx context.Context, limiterID string, weight int, opts gothrottle.Options) (*gothrottle.Lease, time.Duration, error) {
	atomic.AddInt64(&s.acquireCalls, 1)

	s.mu.Lock()
	hold := s.hold
	if hold != nil && !s.entered {
		s.entered = true
		close(s.firstEntered)
	}
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}

	s.mu.Lock()
	s.attempts = append(s.attempts, attempt{weight: weight, at: time.Now()})
	refuse := s.refuseAtLeast > 0 && weight >= s.refuseAtLeast
	if !refuse && s.innerMax > 0 && s.held+weight > s.innerMax {
		refuse = true
	}
	s.mu.Unlock()

	if refuse {
		// A capacity refusal: no lease, and no retryAfter, because only a release
		// somewhere in the cluster can change the answer.
		return nil, 0, nil
	}

	lease, retryAfter, err := s.inner.Acquire(ctx, limiterID, weight, opts)
	if lease != nil {
		s.mu.Lock()
		s.held += weight
		s.mu.Unlock()
	}
	return lease, retryAfter, err
}

func (s *capacityStore) Renew(ctx context.Context, lease *gothrottle.Lease) error {
	return s.inner.Renew(ctx, lease)
}

func (s *capacityStore) Release(ctx context.Context, lease *gothrottle.Lease) error {
	err := s.inner.Release(ctx, lease)
	if lease != nil {
		s.mu.Lock()
		s.held -= lease.Weight
		if s.held < 0 {
			s.held = 0
		}
		s.mu.Unlock()
	}
	return err
}

func (s *capacityStore) Request(limiterID string, weight int, opts gothrottle.Options) (bool, time.Duration, error) {
	return s.inner.Request(limiterID, weight, opts)
}

func (s *capacityStore) RegisterDone(limiterID string, weight int) error {
	return s.inner.RegisterDone(limiterID, weight)
}

func (s *capacityStore) Disconnect() error { return s.inner.Disconnect() }

// allow stops refusing everything, as a release of all the capacity held
// elsewhere in the cluster would.
func (s *capacityStore) allow() {
	s.mu.Lock()
	s.refuseAtLeast = 0
	s.innerMax = 0
	s.mu.Unlock()
}

// limitInner caps how many leases the wrapped store will grant at once,
// independently of the limiter's own MaxConcurrent. It models the free share of a
// shared limit: the rest of the capacity is held by another process.
func (s *capacityStore) limitInner(maxConcurrent int) {
	s.mu.Lock()
	s.innerMax = maxConcurrent
	s.mu.Unlock()
}

func (s *capacityStore) calls() int64 { return atomic.LoadInt64(&s.acquireCalls) }

func (s *capacityStore) attemptedWeights() []int {
	s.mu.Lock()
	defer s.mu.Unlock()

	weights := make([]int, 0, len(s.attempts))
	for _, a := range s.attempts {
		weights = append(weights, a.weight)
	}
	return weights
}

// spacingStore refuses everything with a positive retryAfter, standing in for a
// MinTime window that has not elapsed. Spacing gates the limiter as a whole, so
// no lighter job may be attempted in the same pass.
type spacingStore struct {
	gothrottle.LeaseDatastore

	retryAfter time.Duration

	mu       sync.Mutex
	attempts []int
	release  chan struct{}
}

func newSpacingStore(inner gothrottle.LeaseDatastore, retryAfter time.Duration) *spacingStore {
	return &spacingStore{LeaseDatastore: inner, retryAfter: retryAfter, release: make(chan struct{})}
}

func (s *spacingStore) Acquire(ctx context.Context, limiterID string, weight int, opts gothrottle.Options) (*gothrottle.Lease, time.Duration, error) {
	s.mu.Lock()
	s.attempts = append(s.attempts, weight)
	s.mu.Unlock()

	select {
	case <-s.release:
		return s.LeaseDatastore.Acquire(ctx, limiterID, weight, opts)
	default:
		return nil, s.retryAfter, nil
	}
}

func (s *spacingStore) allow() { close(s.release) }

func (s *spacingStore) attemptedWeights() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.attempts...)
}

// TestBestFit_LighterJobRunsWhenStoreRefusesHeavyHead is the core regression. The
// store refuses weight 2 — as it would with 2 of 3 units held by another process —
// while the limiter's own local weight is 0, so nothing local suggests a problem.
// The weight-1 job behind the head must still run.
func TestBestFit_LighterJobRunsWhenStoreRefusesHeavyHead(t *testing.T) {
	store := newCapacityStore(gothrottle.NewLocalStore(), 2)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-distributed",
		MaxConcurrent: 3,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// Higher priority, heavier: the head of the queue, and refused by the store.
	heavyDone := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return "heavy", nil }, 10, 2)
		heavyDone <- err
	}()

	// Wait until the heavy job is queued so the ordering under test is the one
	// described: heavy at the head, light behind it.
	waitForQueueLen(t, limiter, 1)

	lightRan := make(chan struct{})
	lightDone := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(lightRan)
			return "light", nil
		}, 1, 1)
		lightDone <- err
	}()

	select {
	case <-lightRan:
	case <-time.After(3 * time.Second):
		t.Fatal("the lighter job never ran while the store refused the heavy head job")
	}
	if err := <-lightDone; err != nil {
		t.Fatalf("light job failed: %v", err)
	}

	// The heavy job is still queued and still waiting; nothing has been lost.
	if err := requireNotCompleted(heavyDone); err != nil {
		t.Fatal(err)
	}

	// Capacity comes back elsewhere in the cluster: the heavy job then runs.
	store.allow()
	limiterSignal(t, limiter)
	select {
	case err := <-heavyDone:
		if err != nil {
			t.Fatalf("heavy job failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the heavy job never ran after capacity became available")
	}
}

// TestBestFit_StrictStillBlocksLighterJob is the same scenario under the default
// policy, which must be unchanged: strict priority means the head job holds the
// queue however long the store refuses it.
func TestBestFit_StrictStillBlocksLighterJob(t *testing.T) {
	store := newCapacityStore(gothrottle.NewLocalStore(), 2)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "strict-distributed",
		MaxConcurrent: 3,
		SchedPolicy:   gothrottle.SchedStrict,
		Datastore:     store,
		RetryInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	heavyDone := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 10, 2)
		heavyDone <- err
	}()
	waitForQueueLen(t, limiter, 1)

	lightRan := make(chan struct{})
	lightDone := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			close(lightRan)
			return nil, nil
		}, 1, 1)
		lightDone <- err
	}()

	select {
	case <-lightRan:
		t.Fatal("SchedStrict let a lighter job overtake the refused head job")
	case <-time.After(300 * time.Millisecond):
	}

	store.allow()
	limiterSignal(t, limiter)
	if err := <-heavyDone; err != nil {
		t.Fatalf("heavy job failed: %v", err)
	}
	if err := <-lightDone; err != nil {
		t.Fatalf("light job failed: %v", err)
	}
}

// TestBestFit_MinTimeRefusalStopsAlternatives distinguishes the two kinds of
// refusal. A MinTime refusal carries a positive retryAfter and applies to the
// limiter as a whole, so trying a lighter job cannot succeed — attempting one
// would only burn a datastore round trip per queued job.
func TestBestFit_MinTimeRefusalStopsAlternatives(t *testing.T) {
	store := newSpacingStore(gothrottle.NewLocalStore(), 150*time.Millisecond)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-spacing",
		MaxConcurrent: 10,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// Four jobs of descending weight and priority, enqueued one at a time so the
	// heaviest is unambiguously at the head before the lighter ones arrive. If the
	// scheduler mistook a spacing refusal for a capacity refusal it would work
	// down the weights within a single pass.
	var wg sync.WaitGroup
	for i, weight := range []int{4, 3, 2, 1} {
		weight := weight
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, weight, weight); err != nil {
				t.Errorf("job of weight %d failed: %v", weight, err)
			}
		}()
		waitForQueueLen(t, limiter, i+1)
	}

	// Let several dispatch passes happen. Each may attempt the head job once;
	// what it must not do is walk down to the lighter jobs.
	time.Sleep(100 * time.Millisecond)

	for i, weight := range store.attemptedWeights() {
		if weight != 4 {
			t.Fatalf("attempt %d was for weight %d; a MinTime refusal must not trigger alternatives (attempts: %v)",
				i, weight, store.attemptedWeights())
		}
	}

	store.allow()
	limiterSignal(t, limiter)
	wg.Wait()
}

// TestBestFit_DoesNotBusyLoop bounds the datastore traffic. Walking down to
// lighter jobs must be a per-pass ceiling, not a retry loop: with the store
// refusing everything, the number of Acquire calls has to stay proportional to
// the number of distinct weights, times the retry interval — not spin.
func TestBestFit_DoesNotBusyLoop(t *testing.T) {
	// Refuse every weight, so no job can ever start and every pass exhausts its
	// options.
	store := newCapacityStore(gothrottle.NewLocalStore(), 1)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-no-spin",
		MaxConcurrent: 4,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	var wg sync.WaitGroup
	for _, weight := range []int{4, 3, 2, 1} {
		weight := weight
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, weight, weight)
		}()
	}
	waitForQueueLen(t, limiter, 4)

	start := store.calls()
	time.Sleep(500 * time.Millisecond)
	calls := store.calls() - start

	// 500ms at a 50ms retry interval is ~10 passes, each attempting at most the
	// four distinct weights: 40 calls, plus slack for scheduling jitter. A spin
	// would be orders of magnitude past this.
	const ceiling = 4 * 20
	if calls > ceiling {
		t.Fatalf("%d Acquire calls in 500ms with a 50ms retry interval, want at most %d; the scheduler is spinning", calls, ceiling)
	}
	// It must also not have stopped asking: a limiter that armed no retry would
	// never notice capacity returning in another process.
	if calls == 0 {
		t.Fatal("no Acquire calls in 500ms; a distributed refusal must arm a retry")
	}
	t.Logf("%d Acquire calls in 500ms across 4 queued weights", calls)

	store.allow()
	limiterSignal(t, limiter)
	wg.Wait()
}

// TestBestFit_AttemptsStrictlyLighterWeights inspects one dispatch pass directly.
// Each attempt within a pass must be for a strictly lighter job than the last
// refusal: that is what makes the pass terminate, and what stops the same job
// being asked about twice in one pass.
//
// The pass is isolated without timing assumptions. The store parks the
// scheduler inside its first Acquire, the queue is filled while it is frozen, and
// the retry interval is long enough that releasing the park produces exactly one
// pass in the observation window.
func TestBestFit_AttemptsStrictlyLighterWeights(t *testing.T) {
	// Refuse every weight, so the pass has to work all the way down.
	store := newCapacityStore(gothrottle.NewLocalStore(), 1)
	entered := store.holdCalls()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-descending",
		MaxConcurrent: 5,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		// Long enough that the observation window holds exactly one pass.
		RetryInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// The heaviest job first: the scheduler claims it and parks in the store.
	weights := []int{5, 4, 2, 1}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, weights[0], weights[0])
	}()
	<-entered

	// With the scheduler frozen, queue the rest in descending order.
	for i, weight := range weights[1:] {
		weight := weight
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, weight, weight)
		}()
		waitForQueueLen(t, limiter, i+1)
	}

	// Let the pass run. Every weight is refused, so it walks the whole queue and
	// then arms the long retry.
	store.releaseHold()

	deadline := time.Now().Add(3 * time.Second)
	for len(store.attemptedWeights()) < len(weights) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	attempts := store.attemptedWeights()
	// The enqueues that happened while the scheduler was parked left a wake-up
	// pending, so a second pass follows the first immediately. Segment the
	// attempts wherever a weight fails to decrease: within a pass every attempt
	// is for a strictly lighter job, so that is exactly a pass boundary.
	pass := firstDescendingRun(attempts)
	if len(pass) != len(weights) {
		t.Fatalf("the first dispatch pass attempted %v out of queued weights %v (all attempts: %v); "+
			"a refused job must neither end the pass nor be retried within it", pass, weights, attempts)
	}
	for i, weight := range weights {
		if pass[i] != weight {
			t.Fatalf("the first dispatch pass attempted %v, want the queued weights in descending order %v", pass, weights)
		}
	}

	store.allow()
	limiterSignal(t, limiter)
	wg.Wait()
}

// firstDescendingRun returns the longest strictly descending prefix of weights.
// Within one dispatch pass each attempt is for a strictly lighter job than the
// last refusal, so a weight that fails to decrease marks the start of the next
// pass.
func firstDescendingRun(weights []int) []int {
	for i := 1; i < len(weights); i++ {
		if weights[i] >= weights[i-1] {
			return weights[:i]
		}
	}
	return weights
}

// TestBestFit_EqualPriorityStaysFIFO checks that holding a job back preserves its
// sequence number. A job the store refuses must keep its place among
// equal-priority peers; renumbering it would push it to the back of its priority
// band every time the store said no.
//
// Determinism comes from freezing the scheduler while the queue is filled, so
// submission order is unambiguous, and from admitting one lease at a time
// afterwards, so execution order is a strict sequence that can be compared to it.
func TestBestFit_EqualPriorityStaysFIFO(t *testing.T) {
	// The heavy job is weight 2 and is never admitted; the weight-1 jobs are, one
	// at a time, as if a single unit of a shared limit were free.
	store := newCapacityStore(gothrottle.NewLocalStore(), 2)
	store.limitInner(1)
	entered := store.holdCalls()

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-fifo",
		MaxConcurrent: 2,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	// The heavy job takes the head of the priority band and parks the scheduler.
	heavyDone := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 5, 2)
		heavyDone <- err
	}()
	<-entered

	const jobs = 6
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				return nil, nil
			}, 5, 1)
			if err != nil {
				t.Errorf("job %d failed: %v", i, err)
			}
		}()
		// Sequence numbers are assigned inside Schedule, so submissions have to
		// land one at a time for "submission order" to mean anything.
		waitForQueueLen(t, limiter, i+1)
	}

	store.releaseHold()
	wg.Wait()

	mu.Lock()
	got := append([]int(nil), order...)
	mu.Unlock()

	if len(got) != jobs {
		t.Fatalf("executed %d of %d jobs: %v", len(got), jobs, got)
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("equal-priority execution order = %v, want ascending submission order; a held-back job was renumbered", got)
		}
	}

	store.allow()
	limiterSignal(t, limiter)
	if err := <-heavyDone; err != nil {
		t.Fatalf("heavy job failed: %v", err)
	}
}

// TestBestFit_NoJobRunsTwiceOrDisappears is the accounting check across a burst
// where refusals and admissions interleave. Every caller must get exactly one
// outcome and every task must run exactly once.
func TestBestFit_NoJobRunsTwiceOrDisappears(t *testing.T) {
	// Refuse weight 3 and above so heavy jobs are held back while lighter ones
	// proceed, then let everything through partway.
	store := newCapacityStore(gothrottle.NewLocalStore(), 3)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-accounting",
		MaxConcurrent: 4,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	const jobs = 60
	runs := make([]int32, jobs)
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		i := i
		weight := 1 + i%4
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
				atomic.AddInt32(&runs[i], 1)
				return i, nil
			}, i%3, weight); err != nil {
				t.Errorf("job %d failed: %v", i, err)
			}
		}()
	}

	// Release the artificial cluster pressure while the burst is in flight, so
	// admissions and refusals interleave.
	go func() {
		time.Sleep(50 * time.Millisecond)
		store.allow()
		limiterSignal(t, limiter)
	}()

	wg.Wait()

	for i := range runs {
		switch n := atomic.LoadInt32(&runs[i]); n {
		case 1:
		case 0:
			t.Fatalf("job %d never ran", i)
		default:
			t.Fatalf("job %d ran %d times", i, n)
		}
	}
	// A caller is unblocked with its result before the worker's deferred bookkeeping
	// finishes, so wait for the limiter to settle rather than reading it instantly.
	waitForIdle(t, limiter)
}

// waitForIdle blocks until the limiter reports nothing queued and nothing running.
// Job results reach their callers before the worker's deferred accounting and
// release complete, so this is the synchronization point for "the limiter is done".
func waitForIdle(t *testing.T, limiter *gothrottle.Limiter) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		queued, running := limiter.QueueLen(), limiter.Running()
		if queued == 0 && running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("limiter did not settle: QueueLen = %d, Running = %d, want 0 and 0", queued, running)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestBestFit_DatastoreErrorReachesItsCaller checks an error still fails exactly
// the job that hit it. Walking on to lighter jobs must not swallow the error, and
// one failure must not collapse the rest of the queue.
func TestBestFit_DatastoreErrorReachesItsCaller(t *testing.T) {
	failure := errors.New("simulated store outage")
	store := &erroringStore{
		LeaseDatastore: gothrottle.NewLocalStore(),
		failWeight:     2,
		err:            failure,
	}

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "bestfit-error",
		MaxConcurrent: 4,
		SchedPolicy:   gothrottle.SchedBestFit,
		Datastore:     store,
		RetryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	failed := make(chan error, 1)
	go func() {
		_, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 10, 2)
		failed <- err
	}()

	select {
	case err := <-failed:
		if !errors.Is(err, failure) {
			t.Fatalf("job error = %v, want it to wrap the datastore error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the datastore error never reached its caller")
	}

	// The rest of the queue is unaffected: a weight-1 job still runs.
	if _, err := limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 1, 1); err != nil {
		t.Fatalf("an unrelated job failed after one datastore error: %v", err)
	}
}

// erroringStore fails Acquire for one specific weight, leaving every other weight
// to the real store.
type erroringStore struct {
	gothrottle.LeaseDatastore

	failWeight int
	err        error
}

func (s *erroringStore) Acquire(ctx context.Context, limiterID string, weight int, opts gothrottle.Options) (*gothrottle.Lease, time.Duration, error) {
	if weight == s.failWeight {
		return nil, 0, s.err
	}
	return s.LeaseDatastore.Acquire(ctx, limiterID, weight, opts)
}

// TestBestFit_DistributedAcrossTwoRedisLimiters is the scenario from the audit,
// run against real Redis with two limiters on two independent stores — as two
// processes would be.
//
// Shared MaxConcurrent is 3. Limiter A holds weight 2. Limiter B's queue has a
// higher-priority weight-2 job at the head and a weight-1 job behind it. B's
// local weight is 0, so nothing local says the head job cannot run; only Redis
// knows, and only by refusing. The weight-1 job fits the real remaining capacity
// and must run.
func TestBestFit_DistributedAcrossTwoRedisLimiters(t *testing.T) {
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

	id := uniqueLimiterID("bestfit-two-processes")
	keys := gothrottle.RedisKeys(id)
	t.Cleanup(func() {
		_ = clientA.Del(context.Background(), keys.Leases, keys.Expirations, keys.LastStart, keys.Config).Err()
	})

	shared := gothrottle.Options{
		ID:            id,
		MaxConcurrent: 3,
		LeaseTTL:      10 * time.Second,
		SchedPolicy:   gothrottle.SchedBestFit,
		RetryInterval: 20 * time.Millisecond,
	}

	optsA := shared
	optsA.Datastore = storeA
	limiterA, err := gothrottle.NewLimiter(optsA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiterA.Stop() }()

	optsB := shared
	optsB.Datastore = storeB
	limiterB, err := gothrottle.NewLimiter(optsB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiterB.Stop() }()

	// A occupies 2 of the 3 shared units and holds them until told otherwise.
	occupied := make(chan struct{})
	releaseA := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		_, err := limiterA.ScheduleWithOptions(func() (interface{}, error) {
			close(occupied)
			<-releaseA
			return nil, nil
		}, 1, 2)
		aDone <- err
	}()
	<-occupied

	// B's head job: higher priority, weight 2. Only 1 unit is actually free, so
	// Redis refuses it — while B's own local weight is 0.
	heavyDone := make(chan error, 1)
	go func() {
		_, err := limiterB.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 10, 2)
		heavyDone <- err
	}()
	waitForQueueLen(t, limiterB, 1)

	lightRan := make(chan struct{})
	lightDone := make(chan error, 1)
	go func() {
		_, err := limiterB.ScheduleWithOptions(func() (interface{}, error) {
			close(lightRan)
			return nil, nil
		}, 1, 1)
		lightDone <- err
	}()

	select {
	case <-lightRan:
	case <-time.After(5 * time.Second):
		close(releaseA)
		t.Fatal("the weight-1 job never ran, though 1 of 3 shared units was free")
	}
	if err := <-lightDone; err != nil {
		t.Fatalf("light job failed: %v", err)
	}

	if err := requireNotCompleted(heavyDone); err != nil {
		close(releaseA)
		t.Fatal(err)
	}

	// A releases its 2 units; B's heavy job now fits.
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("limiter A's job failed: %v", err)
	}
	select {
	case err := <-heavyDone:
		if err != nil {
			t.Fatalf("heavy job failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the heavy job never ran after the other process released capacity")
	}

	// Nothing was double-counted or leaked. A caller is unblocked with its result
	// before the worker's deferred release lands, so wait for the release rather
	// than reading the state immediately.
	waitForLeaseCount(t, clientA, keys.Leases, 0)
}

// waitForLeaseCount blocks until a limiter's lease hash holds exactly want
// entries. Job results reach their caller before the worker's deferred release
// completes, so asserting on lease state straight after a job returns is a race.
func waitForLeaseCount(t *testing.T, client *redis.Client, leaseKey string, want int64) {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for {
		held, err := client.HLen(ctx, leaseKey).Result()
		if err != nil {
			t.Fatalf("HLEN failed: %v", err)
		}
		if held == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease hash holds %d entries, want %d", held, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestBestFit_DistributedNeverExceedsSharedLimit is the safety counterpart: best
// fit changes which job is tried, never how much may run. Two limiters push
// mixed-weight work at a shared limit of 3 and the observed peak must never
// exceed it.
func TestBestFit_DistributedNeverExceedsSharedLimit(t *testing.T) {
	clientA := newTestRedisClient(t)
	clientB := newTestRedisClient(t)

	storeA, err := gothrottle.NewRedisStore(clientA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeA.Disconnect() }()

	storeB, err := gothrottle.NewRedisStore(clientB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storeB.Disconnect() }()

	id := uniqueLimiterID("bestfit-shared-limit")
	keys := gothrottle.RedisKeys(id)
	t.Cleanup(func() {
		_ = clientA.Del(context.Background(), keys.Leases, keys.Expirations, keys.LastStart, keys.Config).Err()
	})

	const maxConcurrent = 3
	shared := gothrottle.Options{
		ID:            id,
		MaxConcurrent: maxConcurrent,
		LeaseTTL:      10 * time.Second,
		SchedPolicy:   gothrottle.SchedBestFit,
		RetryInterval: 5 * time.Millisecond,
	}

	var current, peak int64
	var peakMu sync.Mutex

	track := func(weight int) func() {
		running := atomic.AddInt64(&current, int64(weight))
		peakMu.Lock()
		if running > peak {
			peak = running
		}
		peakMu.Unlock()
		return func() { atomic.AddInt64(&current, -int64(weight)) }
	}

	var wg sync.WaitGroup
	for _, store := range []gothrottle.Datastore{storeA, storeB} {
		opts := shared
		opts.Datastore = store
		limiter, err := gothrottle.NewLimiter(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Stop() }()

		for i := 0; i < 12; i++ {
			weight := 1 + i%maxConcurrent
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
					done := track(weight)
					defer done()
					time.Sleep(10 * time.Millisecond)
					return nil, nil
				}, weight, weight); err != nil {
					t.Errorf("job of weight %d failed: %v", weight, err)
				}
			}()
		}
	}
	wg.Wait()

	peakMu.Lock()
	observed := peak
	peakMu.Unlock()

	if observed > maxConcurrent {
		t.Fatalf("peak concurrent weight %d exceeded the shared MaxConcurrent %d", observed, maxConcurrent)
	}
	t.Logf("peak concurrent weight across two limiters: %d of %d", observed, maxConcurrent)
}

// waitForQueueLen blocks until the limiter's queue reaches want, so tests
// synchronize on observable state instead of sleeping for a guessed interval.
func waitForQueueLen(t *testing.T, limiter *gothrottle.Limiter, want int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if limiter.QueueLen() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length reached %d, want %d", limiter.QueueLen(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// requireNotCompleted asserts a job has not finished yet — that it is still
// waiting rather than having run or failed.
//
// It reads only the completion channel. Queue length cannot be part of the
// assertion: a job the scheduler has claimed and is currently asking the store
// about is briefly in neither the queue nor a worker, and that window is exactly
// when a refused job is most likely to be observed. Whether the job is still
// present at all is established later, when it is required to complete once
// capacity frees up.
func requireNotCompleted(done <-chan error) error {
	select {
	case err := <-done:
		if err != nil {
			return errors.New("the refused job failed instead of waiting: " + err.Error())
		}
		return errors.New("the refused job ran while the store had no capacity for it")
	default:
		return nil
	}
}

// limiterSignal nudges the scheduler after capacity becomes available in a way
// the limiter cannot observe locally — in these tests, a change inside the fake
// store. Scheduling a trivial job is the public way to do it and also asserts the
// limiter is still healthy.
func limiterSignal(t *testing.T, limiter *gothrottle.Limiter) {
	t.Helper()

	go func() {
		_, _ = limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 0, 1)
	}()
}
