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
	queue     *PriorityQueue
	mu        sync.RWMutex
	running   bool
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewLimiter creates a new Limiter instance.
func NewLimiter(opts Options) (*Limiter, error) {
	// Validate options
	if opts.Datastore != nil && opts.ID == "" {
		return nil, ErrMissingID
	}

	// Default to LocalStore if no datastore is provided
	datastore := opts.Datastore
	if datastore == nil {
		datastore = NewLocalStore()
		if opts.ID == "" {
			opts.ID = "default"
		}
	}

	limiter := &Limiter{
		opts:      opts,
		datastore: datastore,
		queue:     NewPriorityQueue(),
		stopCh:    make(chan struct{}),
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
	if weight <= 0 {
		return nil, ErrInvalidWeight
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

// Stop stops the limiter and waits for all jobs to complete.
func (l *Limiter) Stop() error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	l.running = false
	close(l.stopCh)
	l.mu.Unlock()

	// Wait for scheduler to finish
	l.wg.Wait()

	// Disconnect datastore
	return l.datastore.Disconnect()
}

// scheduler is the main scheduling loop that runs in a background goroutine.
func (l *Limiter) scheduler() {
	defer l.wg.Done()

	ticker := time.NewTicker(10 * time.Millisecond) // Small polling interval
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			// Process remaining jobs before stopping
			l.processRemainingJobs()
			return
		case <-ticker.C:
			l.processJobs()
		}
	}
}

// processJobs checks for pending jobs and executes them if allowed.
func (l *Limiter) processJobs() {
	l.mu.RLock()
	if l.queue.IsEmpty() || !l.running {
		l.mu.RUnlock()
		return
	}

	// Peek at the next job without removing it
	job := l.queue.PopJob()
	if job == nil {
		l.mu.RUnlock()
		return
	}
	l.mu.RUnlock()

	// Check if job can run
	canRun, waitTime, err := l.datastore.Request(l.opts.ID, job.Weight, l.opts)
	if err != nil {
		job.errorChan <- fmt.Errorf("datastore error: %w", err)
		return
	}

	if !canRun {
		// Put job back in queue
		l.mu.Lock()
		l.queue.PushJob(job)
		l.mu.Unlock()

		// Sleep if wait time is suggested
		if waitTime > 0 {
			time.Sleep(waitTime)
		}
		return
	}

	// Execute job asynchronously
	go l.executeJob(job)
}

// executeJob runs a job and handles its completion.
func (l *Limiter) executeJob(job *Job) {
	defer func() {
		// Register job completion
		if err := l.datastore.RegisterDone(l.opts.ID, job.Weight); err != nil {
			// Log error but don't fail the job
			// In a real implementation, you might want to use a logger here
			_ = err
		}
	}()

	// Execute the job
	result, err := job.Task()

	// Send result back
	if err != nil {
		select {
		case job.errorChan <- err:
		default:
		}
	} else {
		select {
		case job.resultChan <- result:
		default:
		}
	}
}

// processRemainingJobs processes any remaining jobs when stopping.
func (l *Limiter) processRemainingJobs() {
	for {
		l.mu.RLock()
		if l.queue.IsEmpty() {
			l.mu.RUnlock()
			break
		}

		job := l.queue.PopJob()
		l.mu.RUnlock()

		if job == nil {
			break
		}

		// Cancel remaining jobs
		job.errorChan <- ErrStoreClosed
	}
}
