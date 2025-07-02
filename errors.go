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
)
