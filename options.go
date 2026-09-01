// FILENAME: options.go
package gothrottle

import (
	"time"
	"unicode"
)

const maxLimiterIDLength = 512

// defaultRetryInterval is how long the scheduler waits before re-asking a
// distributed datastore for capacity after it was refused. The release happens
// in another process and produces no local event, so this is the one case that
// still needs polling.
const defaultRetryInterval = 10 * time.Millisecond

// defaultRegisterDoneAttempts is how many times a worker tries to hand back its
// capacity before giving up and reporting through OnError. Losing this call
// leaks capacity until the store's own expiry reclaims it.
const defaultRegisterDoneAttempts = 3

// SchedPolicy selects what the scheduler does when the highest priority queued
// job does not fit in the currently available capacity.
type SchedPolicy int

const (
	// SchedStrict waits for the highest priority job to fit. A heavy job holds
	// the queue, so priority is never inverted, at the cost of leaving
	// capacity idle (head-of-line blocking).
	SchedStrict SchedPolicy = iota

	// SchedBestFit lets lighter, lower priority jobs use capacity the head job
	// cannot fill yet. Throughput improves, but a heavy high priority job can
	// be overtaken by lighter work.
	SchedBestFit
)

// Options holds the configuration for a Limiter.
type Options struct {
	ID            string        // A unique ID for the limiter, required for Redis mode.
	MaxConcurrent int           // Max number of jobs running at once. 0 means unlimited.
	MinTime       time.Duration // Minimum time between jobs. 0 means no spacing.
	Datastore     Datastore     // Optional datastore for clustering. Defaults to local if nil.

	// CloseDatastoreOnStop transfers ownership of an injected Datastore to the
	// limiter, so Limiter.Stop disconnects it. It defaults to false because a
	// datastore — and the Redis client inside it — is typically shared with
	// other limiters and other parts of the application, and stopping one
	// limiter must not break them. A datastore the limiter creates for itself
	// is always closed on Stop regardless of this setting.
	CloseDatastoreOnStop bool

	// SchedPolicy controls how weighted jobs compete for capacity.
	// Defaults to SchedStrict.
	SchedPolicy SchedPolicy

	// RetryInterval is how often the scheduler re-checks a distributed
	// datastore that refused capacity. Defaults to 10ms. It has no effect on an
	// idle limiter, which does not wake at all.
	RetryInterval time.Duration

	// MaxQueueSize caps how many jobs may wait in the queue. Scheduling beyond
	// it returns ErrQueueFull, which keeps an overloaded producer from growing
	// the queue without bound. 0 means unbounded.
	MaxQueueSize int

	// LeaseTTL is how long a capacity reservation survives without renewal,
	// when the datastore implements LeaseDatastore. It bounds how long a
	// crashed process can hold capacity; the limiter renews every LeaseTTL/3
	// while a job runs, so a long-running job is not affected. Defaults to 30s,
	// clamped to a 1s minimum.
	LeaseTTL time.Duration

	// OnError receives errors that have no caller to return them to — most
	// importantly a failure to hand capacity back to the datastore, which
	// otherwise leaves capacity reserved with no visibility. It is called from
	// limiter goroutines, so it must be safe for concurrent use and must not
	// block or call back into the limiter.
	OnError func(error)
}

// Validate reports configuration mistakes that would otherwise silently
// weaken or disable throttling.
func (o Options) Validate() error {
	if o.ID != "" {
		if err := validateLimiterID(o.ID); err != nil {
			return err
		}
	}
	// Negative limits are rejected rather than treated as "unlimited": a
	// miscalculated limit should fail loudly, not turn the limiter off.
	if o.MaxConcurrent < 0 {
		return ErrInvalidMaxConcurrent
	}
	if o.MinTime < 0 {
		return ErrInvalidMinTime
	}
	if o.MaxQueueSize < 0 {
		return ErrInvalidMaxQueueSize
	}
	if o.RetryInterval < 0 {
		return ErrInvalidRetryInterval
	}
	if o.LeaseTTL < 0 {
		return ErrInvalidLeaseTTL
	}
	switch o.SchedPolicy {
	case SchedStrict, SchedBestFit:
	default:
		return ErrInvalidSchedPolicy
	}
	return nil
}

// retryInterval returns the effective distributed retry interval.
func (o Options) retryInterval() time.Duration {
	if o.RetryInterval > 0 {
		return o.RetryInterval
	}
	return defaultRetryInterval
}

// reportError hands an error with no caller to Options.OnError, if one is set.
func (o Options) reportError(err error) {
	if err == nil || o.OnError == nil {
		return
	}
	o.OnError(err)
}

func validateLimiterID(id string) error {
	if len(id) > maxLimiterIDLength {
		return ErrInvalidID
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return ErrInvalidID
		}
	}
	return nil
}
