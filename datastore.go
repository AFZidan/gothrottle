// FILENAME: datastore.go
package gothrottle

import "time"

// Datastore defines the interface for state management.
type Datastore interface {
	// Request checks if a job can run according to the limiter's rules.
	// It must return whether the job can run now, and if not, a suggested wait time.
	Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error)

	// RegisterDone informs the store that a job has finished.
	RegisterDone(limiterID string, weight int) error

	// Disconnect cleans up any connections.
	Disconnect() error
}
