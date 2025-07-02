// FILENAME: options.go
package gothrottle

import "time"

// Options holds the configuration for a Limiter.
type Options struct {
	ID            string        // A unique ID for the limiter, required for Redis mode.
	MaxConcurrent int           // Max number of jobs running at once.
	MinTime       time.Duration // Minimum time between jobs.
	Datastore     Datastore     // Optional datastore for clustering. Defaults to local if nil.
	// Future fields like HighWater, Strategy, etc. can be added here.
}
