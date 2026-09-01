// FILENAME: datastore.go
package gothrottle

import "time"

// Datastore defines the interface for state management.
//
// It is the original contract, built around a single shared counter, and it
// cannot express "this job is still running" — see LeaseDatastore, which the
// limiter prefers when a store implements it. The methods here take no context,
// so cancellation guarantees are weaker: the limiter can only wait for a
// Request or RegisterDone call to return.
type Datastore interface {
	// Request checks if a job can run according to the limiter's rules.
	// It must return whether the job can run now, and if not, a suggested wait time.
	Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error)

	// RegisterDone informs the store that a job has finished.
	//
	// It must not shorten the lifetime of state that Request sized to cover a
	// MinTime window: spacing is measured from when a job started, so the record
	// of that start has to outlive the job.
	RegisterDone(limiterID string, weight int) error

	// Disconnect cleans up any connections.
	Disconnect() error
}
