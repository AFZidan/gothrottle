// FILENAME: limiter.go
package gothrottle

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// Defaults applied by Schedule and ScheduleContext.
const (
	defaultPriority = 5
	defaultWeight   = 1
)

// Limiter manages job scheduling and rate limiting.
type Limiter struct {
	opts      Options
	datastore Datastore

	// leaseStore is set when the datastore tracks individual reservations. The
	// limiter prefers it because a shared counter cannot distinguish a slow job
	// from a dead one; a nil value means the legacy Request/RegisterDone path.
	leaseStore LeaseDatastore

	// ownsDatastore records whether Stop is allowed to disconnect the
	// datastore. It is only true for the LocalStore the limiter creates for
	// itself, or when the caller opts in with Options.CloseDatastoreOnStop.
	ownsDatastore bool

	queue   *PriorityQueue
	mu      sync.RWMutex
	running bool

	// seq assigns each job a monotonic sequence number so equal-priority jobs
	// keep FIFO order. Guarded by mu.
	seq uint64

	// localWeight is the weight this limiter currently has running. The
	// scheduler uses it to decide whether waiting for a local completion can
	// unblock a concurrency-blocked queue, or whether only another process can.
	// Guarded by mu.
	localWeight int

	// wake carries scheduling events (job enqueued, capacity released). It is
	// buffered with size 1 and sent to non-blockingly: a pending wake-up is all
	// the scheduler needs, since it drains the whole queue on each pass.
	wake chan struct{}

	// Shutdown state machine. stopCh is closed as soon as Stop is called so
	// the scheduler and any pending waits unblock; stoppedCh is closed only
	// after the scheduler and all workers have finished and the datastore has
	// been released, so concurrent Stop callers all observe a completed
	// shutdown and the same error.
	stopCh    chan struct{}
	stoppedCh chan struct{}
	stopOnce  sync.Once
	stopErr   error

	// opCtx is cancelled when Stop begins. It is what the limiter passes to
	// LeaseDatastore calls that acquire capacity, so a store blocking on a
	// network round trip cannot outlive shutdown. Releases deliberately do not
	// use it: handing capacity back has to succeed *because* we are shutting
	// down, so it gets its own bounded context.
	opCtx    context.Context
	opCancel context.CancelFunc

	wg       sync.WaitGroup
	workerWG sync.WaitGroup
}

// NewLimiter creates a new Limiter instance.
func NewLimiter(opts Options) (*Limiter, error) {
	if opts.Datastore != nil && opts.ID == "" {
		return nil, ErrMissingID
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	// Default to LocalStore if no datastore is provided. A store the limiter
	// creates itself is owned by the limiter; an injected one is not, unless
	// the caller explicitly transfers ownership.
	datastore := opts.Datastore
	ownsDatastore := opts.CloseDatastoreOnStop
	if datastore == nil {
		datastore = NewLocalStore()
		ownsDatastore = true
		if opts.ID == "" {
			opts.ID = "default"
		}
	}

	limiter := &Limiter{
		opts:          opts,
		datastore:     datastore,
		ownsDatastore: ownsDatastore,
		queue:         NewPriorityQueue(),
		wake:          make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		stoppedCh:     make(chan struct{}),
	}
	limiter.opCtx, limiter.opCancel = context.WithCancel(context.Background())
	if leaseStore, ok := datastore.(LeaseDatastore); ok {
		limiter.leaseStore = leaseStore
	}

	limiter.start()

	return limiter, nil
}

// Schedule submits a job to be executed and blocks until completion.
func (l *Limiter) Schedule(task func() (interface{}, error)) (interface{}, error) {
	return l.ScheduleWithOptions(task, defaultPriority, defaultWeight)
}

// ScheduleWithOptions submits a job with custom priority and weight.
func (l *Limiter) ScheduleWithOptions(task func() (interface{}, error), priority, weight int) (interface{}, error) {
	return l.schedule(context.Background(), task, priority, weight)
}

// ScheduleContext submits a job and blocks until it completes or ctx is done.
// If ctx ends while the job is still queued, the job is removed from the queue
// and ctx.Err() is returned; a job that has already started is left to run to
// completion, since the limiter cannot interrupt a task function.
func (l *Limiter) ScheduleContext(ctx context.Context, task func() (interface{}, error)) (interface{}, error) {
	return l.schedule(ctx, task, defaultPriority, defaultWeight)
}

// ScheduleWithOptionsContext is ScheduleContext with a custom priority and
// weight.
func (l *Limiter) ScheduleWithOptionsContext(ctx context.Context, task func() (interface{}, error), priority, weight int) (interface{}, error) {
	return l.schedule(ctx, task, priority, weight)
}

func (l *Limiter) schedule(ctx context.Context, task func() (interface{}, error), priority, weight int) (interface{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if task == nil {
		return nil, ErrNilTask
	}
	if weight <= 0 {
		return nil, ErrInvalidWeight
	}
	if l.opts.MaxConcurrent > 0 && weight > l.opts.MaxConcurrent {
		return nil, ErrWeightExceedsMax
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	job := &Job{
		Task:       task,
		Priority:   priority,
		Weight:     weight,
		resultChan: make(chan interface{}, 1),
		errorChan:  make(chan error, 1),
	}
	if err := l.enqueue(job); err != nil {
		return nil, err
	}

	// Wake the scheduler so the job is considered immediately.
	l.signal()

	select {
	case result := <-job.resultChan:
		return result, nil
	case err := <-job.errorChan:
		return nil, err
	case <-ctx.Done():
		return l.cancelQueued(job, ctx.Err())
	}
}

// enqueue admits a job to the queue, assigning the sequence number that keeps
// equal priorities in submission order. It reports why admission was refused:
// the limiter has stopped, or the queue is at MaxQueueSize.
func (l *Limiter) enqueue(job *Job) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.running {
		return ErrStoreClosed
	}
	if l.opts.MaxQueueSize > 0 && l.queue.Len() >= l.opts.MaxQueueSize {
		return ErrQueueFull
	}
	l.seq++
	job.seq = l.seq
	l.queue.PushJob(job)
	return nil
}

// cancelQueued handles a context that ended while the caller was waiting. A job
// still in the queue is removed and the context error returned. A job that has
// already been dispatched cannot be interrupted, so its real outcome is awaited
// and returned instead — reporting a cancellation while the task keeps running
// would misrepresent what happened.
func (l *Limiter) cancelQueued(job *Job, cause error) (interface{}, error) {
	l.mu.Lock()
	removed := l.queue.Remove(job)
	l.mu.Unlock()

	if removed {
		return nil, cause
	}

	select {
	case result := <-job.resultChan:
		return result, nil
	case err := <-job.errorChan:
		return nil, err
	}
}

// signal nudges the scheduler. The wake channel is buffered with size 1, so a
// pending notification is coalesced: the scheduler drains the whole queue on
// each pass, and a missed duplicate would be redundant.
func (l *Limiter) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Wrap creates a wrapper function that applies rate limiting to any function.
func (l *Limiter) Wrap(fn func() (interface{}, error)) func() (interface{}, error) {
	return func() (interface{}, error) {
		return l.Schedule(fn)
	}
}

// QueueLen returns how many jobs are waiting for capacity. It is a point-in-time
// reading, useful for monitoring queue depth against Options.MaxQueueSize.
func (l *Limiter) QueueLen() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.queue.Len()
}

// Running returns the total weight of jobs currently executing. Unweighted jobs
// count as 1 each, so this is the job count in the common case.
func (l *Limiter) Running() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.localWeight
}

// start begins the scheduler goroutine.
func (l *Limiter) start() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		return
	}

	l.running = true
	l.wg.Add(1)
	go l.scheduler()
}

// Stop stops the limiter, cancels queued jobs and waits for running jobs to
// finish. It is safe to call concurrently and repeatedly: every caller blocks
// until shutdown has completed and receives the same error.
//
// Shutdown cancels the context the limiter passes to a LeaseDatastore, so a
// store blocked inside Acquire or Renew is unblocked rather than holding Stop
// open. Releases are exempt and get a bounded context of their own: capacity
// still has to be handed back. A legacy Datastore takes no context, so its
// Request and RegisterDone calls can only be waited out.
//
// The datastore is only disconnected if the limiter owns it (see
// Options.CloseDatastoreOnStop). An injected datastore stays usable so that
// other limiters, or other parts of the application sharing the same Redis
// client, are unaffected.
//
// Stop must not be called from inside a scheduled task; doing so deadlocks
// because Stop waits for that task to finish.
func (l *Limiter) Stop() error {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		l.running = false
		close(l.stopCh)
		l.mu.Unlock()

		// Unblock any datastore call that is waiting on the limiter's context.
		// Without this a store blocked in Acquire would keep the scheduler, and
		// therefore Stop, waiting indefinitely.
		l.opCancel()

		// Wait for the scheduler to finish (it cancels queued jobs on its way
		// out), then for every worker so their RegisterDone calls land before
		// the datastore is released.
		l.wg.Wait()
		l.workerWG.Wait()

		if l.ownsDatastore {
			l.stopErr = l.datastore.Disconnect()
		}

		close(l.stoppedCh)
	})

	<-l.stoppedCh
	return l.stopErr
}

// scheduler is the main scheduling loop. It sleeps until something can change
// the outcome of a dispatch attempt — a job being enqueued, a worker releasing
// capacity, a MinTime deadline expiring, or shutdown — instead of polling on a
// fixed tick. An idle limiter therefore does not wake at all, and a burst of
// jobs is dispatched as fast as capacity allows rather than one per tick.
func (l *Limiter) scheduler() {
	defer l.wg.Done()

	deadline := newDeadlineTimer()
	defer deadline.stop()

	for {
		// A deadline is armed only when the queue is blocked on one: a MinTime
		// window that has to elapse, or a distributed retry.
		deadline.arm(l.dispatch())

		select {
		case <-l.stopCh:
			l.processRemainingJobs()
			return
		case <-l.wake:
		case <-deadline.fired():
			deadline.disarmed()
		}
	}
}

// deadlineTimer is a reusable one-shot timer. It exists to keep the disarm dance
// — stop, and drain the channel if the timer fired first — in one place instead
// of at every point the scheduler might abandon a pending deadline.
type deadlineTimer struct {
	timer *time.Timer
	armed bool
}

func newDeadlineTimer() *deadlineTimer {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	return &deadlineTimer{timer: timer}
}

// arm replaces any pending deadline with a new one. A non-positive duration
// leaves the timer disarmed, so the caller waits on events alone.
func (d *deadlineTimer) arm(after time.Duration) {
	d.stop()
	if after > 0 {
		d.timer.Reset(after)
		d.armed = true
	}
}

// stop cancels a pending deadline, draining a firing that raced with the stop.
func (d *deadlineTimer) stop() {
	if d.armed && !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
	d.armed = false
}

// fired is the channel a deadline arrives on.
func (d *deadlineTimer) fired() <-chan time.Time {
	return d.timer.C
}

// disarmed records that the deadline was consumed from fired(), so a later stop
// does not try to drain an already-empty channel.
func (d *deadlineTimer) disarmed() {
	d.armed = false
}

// dispatch starts every queued job that can run right now. It returns how long
// to wait before trying again, or 0 when only an external event (a new job or a
// capacity release) can change the outcome.
func (l *Limiter) dispatch() time.Duration {
	for {
		job, running := l.claimNextJob()
		if !running || job == nil {
			// Either shutting down, the queue is empty, or the queue is blocked
			// on local capacity — in which case a worker completion will signal
			// the scheduler.
			return 0
		}

		granted, waitTime, err := l.acquireCapacity(job)
		if err != nil {
			// A cancellation caused by our own shutdown is not a datastore
			// failure: the caller's job will never run, so it gets the same
			// terminal error as everything else in the queue.
			if l.shutdownCancelled(err) {
				l.failJob(job, ErrStoreClosed)
				return 0
			}
			// The claimed job is not requeued: its caller is unblocked with the
			// error and decides whether to retry. Back off before touching the
			// datastore again so one outage does not fail the whole queue in a
			// tight loop.
			l.failJob(job, fmt.Errorf("datastore error: %w", err))
			return l.opts.retryInterval()
		}

		if !granted {
			if !l.releaseJob(job) {
				l.failJob(job, ErrStoreClosed)
				return 0
			}

			if waitTime > 0 {
				// A MinTime window has to elapse; nothing else will wake us.
				return waitTime
			}
			// Concurrency-refused by the store. Capacity may be held by this
			// limiter (a local completion will signal us immediately), but it
			// may equally be held by another limiter on the same store or
			// another process, and those releases produce no local event. Poll
			// as a backstop; the wake channel still gives immediate dispatch in
			// the local case.
			return l.opts.retryInterval()
		}

		// Capacity has been reserved in the datastore. Stop may have been called
		// while the request was in flight, so re-check before starting work: a
		// task must never begin after shutdown started. The reservation is handed
		// back so the capacity is not leaked for other limiters sharing the store.
		if !l.startJob(job) {
			l.releaseCapacity(job)
			l.failJob(job, ErrStoreClosed)
			return 0
		}
	}
}

// acquireCapacity reserves capacity for a job. With a LeaseDatastore the
// resulting lease is attached to the job, so its release later targets that one
// reservation rather than decrementing a shared counter.
//
// The lease call receives the limiter's operation context, so a store that
// blocks observes shutdown. The legacy path takes no context and can only be
// waited out.
func (l *Limiter) acquireCapacity(job *Job) (granted bool, retryAfter time.Duration, err error) {
	if l.leaseStore == nil {
		canRun, waitTime, err := l.datastore.Request(l.opts.ID, job.Weight, l.opts)
		return canRun, waitTime, err
	}

	lease, retryAfter, err := l.leaseStore.Acquire(l.opCtx, l.opts.ID, job.Weight, l.opts)
	if err != nil {
		return false, 0, err
	}
	if lease == nil {
		return false, retryAfter, nil
	}

	job.lease = lease
	return true, 0, nil
}

// shutdownCancelled reports whether an error is this limiter's own shutdown
// cancelling a datastore call, rather than a failure of the store. A store may
// return ctx.Err() directly or wrap it, so both are recognized.
func (l *Limiter) shutdownCancelled(err error) bool {
	if err == nil {
		return false
	}
	if l.opCtx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// claimNextJob removes the next dispatchable job from the queue. It returns nil
// when nothing is eligible — the queue is empty, or this limiter's own
// concurrency window is full and a worker completion has to free it first — and
// reports whether the limiter is still running.
func (l *Limiter) claimNextJob() (job *Job, running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.running {
		return nil, false
	}
	if l.queue.IsEmpty() {
		return nil, true
	}

	next := l.queue.Peek()
	if l.opts.MaxConcurrent > 0 {
		free := l.opts.MaxConcurrent - l.localWeight
		if free <= 0 {
			// Fully committed locally. Skip the datastore round trip; a worker
			// completion will wake us.
			return nil, true
		}
		if next.Weight > free {
			if l.opts.SchedPolicy != SchedBestFit {
				// Strict priority: the head job holds the queue until it fits.
				return nil, true
			}
			// Best fit: let a lighter job use the capacity the head job cannot
			// fill yet.
			next = l.queue.peekFit(free)
			if next == nil {
				return nil, true
			}
		}
	}

	if !l.queue.Remove(next) {
		return nil, true
	}
	return next, true
}

// releaseJob puts a claimed job back in the queue after capacity was refused.
// It reports false when the limiter stopped in the meantime, in which case the
// caller must cancel the job.
func (l *Limiter) releaseJob(job *Job) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.running {
		return false
	}
	l.queue.PushJob(job)
	return true
}

// startJob accounts for a job whose capacity has been granted and launches its
// worker. It reports false when the limiter stopped while capacity was being
// acquired, in which case no worker is started.
func (l *Limiter) startJob(job *Job) bool {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return false
	}
	l.localWeight += job.Weight
	l.mu.Unlock()

	l.workerWG.Add(1)
	go l.executeJob(job)
	return true
}

// executeJob runs a job and handles its completion.
func (l *Limiter) executeJob(job *Job) {
	defer l.workerWG.Done()

	// A lease expires unless it is renewed, which is what lets the store tell a
	// dead holder from a slow one. Renew for as long as the task runs so a job
	// longer than the TTL keeps its capacity.
	stopRenewal := l.startRenewal(job)
	defer l.finishJob(job, stopRenewal)

	result, err := job.Task()
	if err != nil {
		l.failJob(job, err)
		return
	}
	select {
	case job.resultChan <- result:
	default:
	}
}

// finishJob releases everything a finished — or panicking — job holds: its
// renewal goroutine, this limiter's share of the concurrency window, and the
// datastore reservation. It runs deferred, so a panicking task takes the same
// path as a returning one.
func (l *Limiter) finishJob(job *Job, stopRenewal func()) {
	if r := recover(); r != nil {
		// Capture the stack here, while it is still the panicking goroutine's;
		// by the time the caller sees the error it is gone.
		l.failJob(job, &PanicError{Value: r, Stack: debug.Stack()})
	}

	stopRenewal()

	l.mu.Lock()
	l.localWeight -= job.Weight
	if l.localWeight < 0 {
		l.localWeight = 0
	}
	l.mu.Unlock()

	l.releaseCapacity(job)
	// Freed capacity may let queued work start immediately.
	l.signal()
}

// startRenewal keeps a job's lease alive for as long as it runs and returns a
// function that stops the renewal. With no lease store it is a no-op.
//
// The returned function cancels an in-flight renewal before waiting for the
// goroutine, so a store blocked inside Renew cannot hold up job completion or
// shutdown.
func (l *Limiter) startRenewal(job *Job) func() {
	if l.leaseStore == nil || job.lease == nil {
		return func() {}
	}

	// Derived from the limiter's operation context so shutdown cancels renewal
	// too, and cancellable on its own so a finished job stops renewing
	// immediately even while the limiter keeps running.
	renewCtx, cancelRenew := context.WithCancel(l.opCtx)
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		ticker := time.NewTicker(l.opts.renewInterval())
		defer ticker.Stop()

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				err := l.leaseStore.Renew(renewCtx, job.lease)
				if err == nil {
					continue
				}
				if errors.Is(err, ErrStoreClosed) || renewCtx.Err() != nil {
					return
				}
				// A lost lease means the store handed this capacity to someone
				// else, so the limit is already being exceeded. The task cannot
				// be interrupted, so report it and stop renewing.
				l.opts.reportError(fmt.Errorf("lease renewal failed for limiter %q: %w", l.opts.ID, err))
				if errors.Is(err, ErrLeaseLost) {
					return
				}
			}
		}
	}()

	return func() {
		cancelRenew()
		<-stopped
	}
}

// releaseCapacity hands a job's reservation back to the datastore. Losing this
// call leaves capacity reserved for work that has already finished, so it is
// retried before being reported through Options.OnError.
//
// The whole effort is bounded by one deadline that does *not* derive from the
// limiter's operation context: a release has to succeed precisely because the
// limiter is shutting down, so shutdown must not cancel it. Bounding it keeps a
// wedged store from holding Stop open indefinitely.
func (l *Limiter) releaseCapacity(job *Job) {
	ctx, cancel := context.WithTimeout(context.Background(), l.opts.releaseBudget())
	defer cancel()

	var err error
	for attempt := 1; attempt <= defaultRegisterDoneAttempts; attempt++ {
		err = l.releaseOnce(ctx, job)
		if err == nil {
			return
		}
		// A closed store will not recover, and during shutdown this is expected.
		if errors.Is(err, ErrStoreClosed) {
			break
		}
		// The lease is already gone, so there is nothing left to hand back.
		if errors.Is(err, ErrLeaseLost) {
			return
		}
		// Out of budget: the store has had its chance, and retrying past the
		// lease TTL cannot help because the store reclaims the lease itself.
		if ctx.Err() != nil {
			break
		}
		if attempt < defaultRegisterDoneAttempts {
			select {
			case <-time.After(time.Duration(attempt) * l.opts.retryInterval()):
			case <-ctx.Done():
			case <-l.stopCh:
				// Try once more immediately, then give up: Stop is waiting on
				// this worker.
			}
		}
	}

	l.opts.reportError(fmt.Errorf("failed to release capacity for limiter %q (weight %d): %w", l.opts.ID, job.Weight, err))
}

// releaseOnce performs a single release attempt against whichever store
// interface is in use. The legacy Datastore takes no context, so its release
// cannot be bounded from here.
func (l *Limiter) releaseOnce(ctx context.Context, job *Job) error {
	if l.leaseStore != nil && job.lease != nil {
		return l.leaseStore.Release(ctx, job.lease)
	}
	return l.datastore.RegisterDone(l.opts.ID, job.Weight)
}

// failJob delivers a terminal error to a job's caller without blocking. The
// error channel is buffered, and a job only ever receives one outcome, so the
// default case is defensive.
func (l *Limiter) failJob(job *Job, err error) {
	select {
	case job.errorChan <- err:
	default:
	}
}

// processRemainingJobs cancels any jobs still queued when stopping.
func (l *Limiter) processRemainingJobs() {
	for {
		l.mu.Lock()
		job := l.queue.PopJob()
		l.mu.Unlock()

		if job == nil {
			return
		}

		l.failJob(job, ErrStoreClosed)
	}
}
