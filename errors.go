// FILENAME: errors.go
package gothrottle

import (
	"errors"
	"fmt"
)

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

	// ErrInvalidMaxConcurrent is returned when MaxConcurrent is negative. Only
	// zero means unlimited; a negative value is a configuration mistake and
	// would otherwise silently disable the concurrency limit.
	ErrInvalidMaxConcurrent = errors.New("MaxConcurrent must not be negative")

	// ErrInvalidMinTime is returned when MinTime is negative. A negative value
	// would otherwise silently disable the minimum spacing between jobs.
	ErrInvalidMinTime = errors.New("MinTime must not be negative")

	// ErrInvalidMaxQueueSize is returned when MaxQueueSize is negative. Only
	// zero means unbounded.
	ErrInvalidMaxQueueSize = errors.New("MaxQueueSize must not be negative")

	// ErrInvalidRetryInterval is returned when RetryInterval is negative.
	ErrInvalidRetryInterval = errors.New("RetryInterval must not be negative")

	// ErrInvalidLeaseTTL is returned when LeaseTTL is negative.
	ErrInvalidLeaseTTL = errors.New("LeaseTTL must not be negative")

	// ErrInvalidSchedPolicy is returned when SchedPolicy is not a known policy.
	ErrInvalidSchedPolicy = errors.New("SchedPolicy is not a known scheduling policy")

	// ErrValueOutOfRange is returned when a configuration value or job weight is
	// too large to be enforced exactly. Redis decides admission inside Lua, whose
	// numbers are IEEE-754 doubles, so an integer above 2^53-1 would be compared
	// as a rounded approximation: two different limits could look equal, and a
	// limit could be silently shifted. Errors wrapping it name the field and the
	// supported maximum.
	ErrValueOutOfRange = errors.New("value exceeds the range that can be enforced exactly")

	// ErrNilClient is returned when a Redis store is constructed without a
	// client. It unwraps to ErrStoreClosed, both because a store with no client
	// can never serve a request and so that code written against the previous
	// behavior — which returned ErrStoreClosed here — keeps working.
	ErrNilClient error = nilClientError{}

	// ErrQueueFull is returned when the queue has reached MaxQueueSize.
	ErrQueueFull = errors.New("limiter queue is full")

	// ErrLimiterConfigMismatch is returned by a LeaseDatastore when the
	// admission-relevant configuration supplied for a limiter ID disagrees with
	// the configuration already recorded for it. Sharing an ID with different
	// MaxConcurrent, MinTime or LeaseTTL values makes the effective distributed
	// policy depend on which process reaches the store first, so it is rejected
	// rather than silently resolved. Errors wrapping it carry both
	// configurations.
	ErrLimiterConfigMismatch = errors.New("limiter configuration does not match the configuration already registered for this ID")
)

// nilClientError implements ErrNilClient. It is a type rather than
// errors.New so that it can unwrap to ErrStoreClosed.
type nilClientError struct{}

func (nilClientError) Error() string { return "redis client must not be nil" }

func (nilClientError) Unwrap() error { return ErrStoreClosed }

// PanicError carries the value a task panicked with and the stack trace
// captured at the point of recovery, so a panic in a scheduled task can be
// diagnosed without the goroutine's stack being lost.
//
// It matches errors.Is(err, ErrTaskPanic), so existing checks keep working:
//
//	if errors.Is(err, gothrottle.ErrTaskPanic) { ... }
//
// Use errors.As to reach the stack:
//
//	var perr *gothrottle.PanicError
//	if errors.As(err, &perr) { log.Print(perr.Stack) }
type PanicError struct {
	// Value is whatever was passed to panic.
	Value interface{}
	// Stack is the stack trace captured where the panic was recovered.
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("%s: %v", ErrTaskPanic.Error(), e.Value)
}

// Unwrap makes errors.Is(err, ErrTaskPanic) report true.
func (e *PanicError) Unwrap() error { return ErrTaskPanic }
