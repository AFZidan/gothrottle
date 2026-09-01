// FILENAME: options.go
package gothrottle

import (
	"time"
	"unicode"
)

const maxLimiterIDLength = 512

// defaultRetryInterval is how long the scheduler waits before re-asking the
// datastore for capacity when it is concurrency-blocked and has no local job
// running. Only a release by another process can unblock it, and there is no
// event to wake on, so this is the one case that still needs polling.
const defaultRetryInterval = 10 * time.Millisecond

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
	MaxConcurrent int           // Max number of jobs running at once.
	MinTime       time.Duration // Minimum time between jobs.
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
	// datastore while it is concurrency-blocked with nothing running locally.
	// Defaults to 10ms. It has no effect on an idle limiter, which does not
	// wake at all.
	RetryInterval time.Duration
	// Future fields like HighWater, Strategy, etc. can be added here.
}

// retryInterval returns the effective distributed retry interval.
func (o Options) retryInterval() time.Duration {
	if o.RetryInterval > 0 {
		return o.RetryInterval
	}
	return defaultRetryInterval
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
