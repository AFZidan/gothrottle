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

	wg       sync.WaitGroup
	workerWG sync.WaitGroup
}

// NewLimiter creates a new Limiter instance.
func NewLimiter(opts Options) (*Limiter, error) {
	// Validate options
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

	// Start the scheduler
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

	// Add job to queue
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil, ErrStoreClosed
	}
	if l.opts.MaxQueueSize > 0 && l.queue.Len() >= l.opts.MaxQueueSize {
		l.mu.Unlock()
		return nil, ErrQueueFull
	}
	l.seq++
	job.seq = l.seq
	l.queue.PushJob(job)
	l.mu.Unlock()

	// Wake the scheduler so the job is considered immediately.
	l.signal()

	// Wait for job completion
	select {
	case result := <-job.resultChan:
		return result, nil
	case err := <-job.errorChan:
		return nil, err
	case <-ctx.Done():
		return l.cancelQueued(job, ctx.Err())
	}
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

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	for {
		wait := l.dispatch()

		// Arm the timer only when the queue is blocked on a deadline: a MinTime
		// window that has to elapse, or a distributed retry.
		if timerArmed && !timer.Stop() {
			// Drain a timer that fired while we were dispatching.
			select {
			case <-timer.C:
			default:
			}
		}
		timerArmed = false
		if wait > 0 {
			timer.Reset(wait)
			timerArmed = true
		}

		select {
		case <-l.stopCh:
			if timerArmed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// Cancel anything still queued before stopping
			l.processRemainingJobs()
			return
		case <-l.wake:
		case <-timer.C:
			timerArmed = false
		}
	}
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

		canRun, waitTime, err := l.datastore.Request(l.opts.ID, job.Weight, l.opts)
		if err != nil {
			// The claimed job is not requeued: its caller is unblocked with the
			// error and decides whether to retry. Back off before touching the
			// datastore again so one outage does not fail the whole queue in a
			// tight loop.
			l.failJob(job, fmt.Errorf("datastore error: %w", err))
			return l.opts.retryInterval()
		}

		if !canRun {
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
			l.releaseCapacity(job.Weight)
			l.failJob(job, ErrStoreClosed)
			return 0
		}
	}
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
	defer func() {
		if r := recover(); r != nil {
			// Capture the stack here, while it is still the panicking
			// goroutine's; by the time the caller sees the error it is gone.
			l.failJob(job, &PanicError{Value: r, Stack: debug.Stack()})
		}

		l.mu.Lock()
		l.localWeight -= job.Weight
		if l.localWeight < 0 {
			l.localWeight = 0
		}
		l.mu.Unlock()

		l.releaseCapacity(job.Weight)
		// Freed capacity may let queued work start immediately.
		l.signal()
	}()

	// Execute the job
	result, err := job.Task()

	// Send result back
	if err != nil {
		l.failJob(job, err)
	} else {
		select {
		case job.resultChan <- result:
		default:
		}
	}
}

// releaseCapacity hands a job's reserved weight back to the datastore. Losing
// this call permanently inflates the store's running count — capacity stays
// reserved for work that has finished — so it is retried before being reported
// through Options.OnError.
func (l *Limiter) releaseCapacity(weight int) {
	var err error
	for attempt := 1; attempt <= defaultRegisterDoneAttempts; attempt++ {
		err = l.datastore.RegisterDone(l.opts.ID, weight)
		if err == nil {
			return
		}
		// A closed store will not recover, and during shutdown this is expected.
		if errors.Is(err, ErrStoreClosed) {
			break
		}
		if attempt < defaultRegisterDoneAttempts {
			select {
			case <-time.After(time.Duration(attempt) * l.opts.retryInterval()):
			case <-l.stopCh:
				// Try once more immediately, then give up: Stop is waiting on
				// this worker.
			}
		}
	}

	l.opts.reportError(fmt.Errorf("failed to release capacity for limiter %q (weight %d): %w", l.opts.ID, weight, err))
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
