// FILENAME: errors.go
package gothrottle

import "errors"

var (
	// ErrStoreClosed is returned when attempting to use a closed store.
	ErrStoreClosed = errors.New("store is closed")

	// ErrMissingID is returned when a limiter ID is required but not provided.
	ErrMissingID = errors.New("limiter ID is required")

	// ErrInvalidWeight is returned when a job weight is invalid.
	ErrInvalidWeight = errors.New("job weight must be positive")

	// ErrWeightExceedsMax is returned when a job can never fit within the configured limit.
	ErrWeightExceedsMax = errors.New("job weight exceeds max concurrent limit")

	// ErrNilTask is returned when attempting to schedule a nil task.
	ErrNilTask = errors.New("task must not be nil")

	// ErrTaskPanic is returned when a scheduled task panics.
	ErrTaskPanic = errors.New("task panicked")

	// ErrInvalidID is returned when a limiter ID is malformed.
	ErrInvalidID = errors.New("limiter ID is invalid")
)
