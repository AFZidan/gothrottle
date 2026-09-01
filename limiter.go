// FILENAME: limiter.go
package gothrottle

import (
	"fmt"
	"sync"
	"time"
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
	if opts.ID != "" {
		if err := validateLimiterID(opts.ID); err != nil {
			return nil, err
		}
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
		stopCh:        make(chan struct{}),
		stoppedCh:     make(chan struct{}),
	}

	// Start the scheduler
	limiter.start()

	return limiter, nil
}

// Schedule submits a job to be executed and blocks until completion.
func (l *Limiter) Schedule(task func() (interface{}, error)) (interface{}, error) {
	return l.ScheduleWithOptions(task, 5, 1) // Default priority 5, weight 1
}

// ScheduleWithOptions submits a job with custom priority and weight.
func (l *Limiter) ScheduleWithOptions(task func() (interface{}, error), priority, weight int) (interface{}, error) {
	if task == nil {
		return nil, ErrNilTask
	}
	if weight <= 0 {
		return nil, ErrInvalidWeight
	}
	if l.opts.MaxConcurrent > 0 && weight > l.opts.MaxConcurrent {
		return nil, ErrWeightExceedsMax
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
	l.queue.PushJob(job)
	l.mu.Unlock()

	// Wait for job completion
	select {
	case result := <-job.resultChan:
		return result, nil
	case err := <-job.errorChan:
		return nil, err
	}
}

// Wrap creates a wrapper function that applies rate limiting to any function.
func (l *Limiter) Wrap(fn func() (interface{}, error)) func() (interface{}, error) {
	return func() (interface{}, error) {
		return l.Schedule(fn)
	}
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

// isRunning reports whether the limiter is still accepting and starting work.
func (l *Limiter) isRunning() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.running
}

// scheduler is the main scheduling loop that runs in a background goroutine.
func (l *Limiter) scheduler() {
	defer l.wg.Done()

	ticker := time.NewTicker(10 * time.Millisecond) // Small polling interval
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			// Cancel anything still queued before stopping
			l.processRemainingJobs()
			return
		case <-ticker.C:
			l.processJobs()
		}
	}
}

// processJobs checks for pending jobs and executes them if allowed.
func (l *Limiter) processJobs() {
	l.mu.Lock()
	if l.queue.IsEmpty() || !l.running {
		l.mu.Unlock()
		return
	}

	job := l.queue.PopJob()
	if job == nil {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	// Check if job can run
	canRun, waitTime, err := l.datastore.Request(l.opts.ID, job.Weight, l.opts)
	if err != nil {
		l.failJob(job, fmt.Errorf("datastore error: %w", err))
		return
	}

	if !canRun {
		// Put job back in queue
		l.mu.Lock()
		if l.running {
			l.queue.PushJob(job)
			l.mu.Unlock()
		} else {
			l.mu.Unlock()
			l.failJob(job, ErrStoreClosed)
		}

		// Sleep if wait time is suggested
		if waitTime > 0 {
			timer := time.NewTimer(waitTime)
			select {
			case <-timer.C:
			case <-l.stopCh:
				if !timer.Stop() {
					<-timer.C
				}
			}
		}
		return
	}

	// Capacity has been reserved in the datastore. Stop may have been called
	// while the request was in flight, so re-check before starting work: a
	// task must never begin after shutdown started. The reservation is handed
	// back so the capacity is not leaked for other limiters sharing the store.
	if !l.isRunning() {
		l.releaseCapacity(job.Weight)
		l.failJob(job, ErrStoreClosed)
		return
	}

	// Execute job asynchronously
	l.workerWG.Add(1)
	go l.executeJob(job)
}

// executeJob runs a job and handles its completion.
func (l *Limiter) executeJob(job *Job) {
	defer l.workerWG.Done()
	defer func() {
		if r := recover(); r != nil {
			l.failJob(job, fmt.Errorf("%w: %v", ErrTaskPanic, r))
		}
		l.releaseCapacity(job.Weight)
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

// releaseCapacity returns a job's reserved weight to the datastore.
func (l *Limiter) releaseCapacity(weight int) {
	if err := l.datastore.RegisterDone(l.opts.ID, weight); err != nil {
		// TODO(phase 4): surface through Options.OnError with retries.
		_ = err
	}
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
