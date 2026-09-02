// FILENAME: options.go
package gothrottle

import (
	"fmt"
	"time"
	"unicode"
)

const maxLimiterIDLength = 512

// maxExactLuaInt is the largest integer Redis Lua represents exactly. Lua 5.1
// numbers are IEEE-754 doubles, so 2^53-1 is the last integer whose immediate
// successor is also representable: above it two different limits can compare
// equal, and a limit can shift as it crosses the boundary. Redis decides
// admission inside Lua, so a value that cannot survive the trip is rejected up
// front rather than enforced approximately.
//
// Every quantity crossing into Lua is checked against it: MaxConcurrent, job
// weights, and the durations as microsecond counts. A time.Duration tops out at
// roughly 9.22e15µs, about 2% past this boundary, so a MinTime or LeaseTTL near
// the very top of the Duration range is rejected too — around 285 years, which
// no throttling configuration means.
//
// The constant is int64 deliberately: on a 32-bit build it does not fit in an
// int, so values are widened for the comparison instead.
const maxExactLuaInt int64 = 1<<53 - 1

// maxInt is the largest value an int holds on the building platform.
const maxInt = int(^uint(0) >> 1)

// defaultRetryInterval is how long the scheduler waits before re-asking a
// distributed datastore for capacity after it was refused. The release happens
// in another process and produces no local event, so this is the one case that
// still needs polling.
const defaultRetryInterval = 10 * time.Millisecond

// defaultRegisterDoneAttempts is how many times a worker tries to hand back its
// capacity before giving up and reporting through OnError. Losing this call
// leaks capacity until the store's own expiry reclaims it.
const defaultRegisterDoneAttempts = 3

// minReleaseBudget floors how long a worker may spend handing capacity back. The
// budget scales with LeaseTTL — beyond it the store reclaims the lease anyway —
// but a 1s lease TTL must still leave room for a retry on a slow network.
const minReleaseBudget = 5 * time.Second

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
//
// Every admission path runs this — NewLimiter, and both stores' Request and
// Acquire — so a direct datastore caller is held to exactly the same rules as a
// limiter-mediated one. That includes the scheduler-only fields (SchedPolicy,
// RetryInterval, MaxQueueSize) which no store reads: a negative RetryInterval is
// a configuration mistake wherever it appears, and one validator with one
// behavior is worth more than a store-specific subset that lets some mistakes
// through depending on which entry point was used.
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
	if err := checkLuaExactRange("MaxConcurrent", int64(o.MaxConcurrent)); err != nil {
		return err
	}
	if o.MinTime < 0 {
		return ErrInvalidMinTime
	}
	if err := checkLuaExactRange("MinTime in microseconds", o.MinTime.Microseconds()); err != nil {
		return err
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
	if err := checkLuaExactRange("LeaseTTL in microseconds", o.LeaseTTL.Microseconds()); err != nil {
		return err
	}
	switch o.SchedPolicy {
	case SchedStrict, SchedBestFit:
	default:
		return ErrInvalidSchedPolicy
	}
	return nil
}

// checkLuaExactRange rejects an integer that Redis Lua cannot compare exactly.
// Redis decides admission inside a Lua script, so a limit it can only
// approximate is a limit that is not being enforced as written.
func checkLuaExactRange(field string, value int64) error {
	if value > maxExactLuaInt {
		return fmt.Errorf("%w: %s is %d, but at most %d can be enforced exactly by the Redis Lua scripts",
			ErrValueOutOfRange, field, value, maxExactLuaInt)
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

// releaseBudget bounds the total time a worker spends handing its capacity back,
// including retries. Shutdown must not cancel a release — the whole point is to
// return the reservation — so it needs a deadline of its own to keep a wedged
// store from blocking Stop forever. Past the lease TTL there is nothing left to
// return: the store reclaims the lease itself.
func (o Options) releaseBudget() time.Duration {
	budget := o.leaseTTL()
	if budget < minReleaseBudget {
		budget = minReleaseBudget
	}
	return budget
}

// reportError hands an error with no caller to Options.OnError, if one is set.
func (o Options) reportError(err error) {
	if err == nil || o.OnError == nil {
		return
	}
	o.OnError(err)
}

// validateAdmission checks the preconditions shared by every admission call —
// Request on the legacy path and Acquire on the lease path — so both store
// implementations reject the same inputs.
//
// It validates the whole Options value, not just the fields a store reads.
// NewLimiter has always called Options.Validate, but a direct
// Request/Acquire caller bypassed it entirely, so a negative MaxConcurrent
// reached the store and was honored as "unlimited" — precisely the silent
// failure Validate exists to prevent. Anything Validate rejects for a limiter is
// rejected here too; see Options.Validate for why the scheduler-only fields are
// included rather than filtered out.
//
// An empty limiter ID is not checked here: it is a problem for RedisStore, where
// the ID becomes part of a key, and harmless for LocalStore, where it is just a
// map key. RedisStore rejects it before calling this.
func validateAdmission(limiterID string, weight int, opts Options) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if err := validateLimiterID(limiterID); err != nil {
		return err
	}
	if weight <= 0 {
		return ErrInvalidWeight
	}
	if err := checkLuaExactRange("weight", int64(weight)); err != nil {
		return err
	}
	if opts.MaxConcurrent > 0 && weight > opts.MaxConcurrent {
		// Without this the job would be refused forever with no error,
		// spinning in the scheduler's requeue loop.
		return ErrWeightExceedsMax
	}
	return nil
}

// validateCompletion checks the preconditions shared by every RegisterDone call.
// Like validateAdmission it leaves the empty-ID case to RedisStore.
func validateCompletion(limiterID string, weight int) error {
	if err := validateLimiterID(limiterID); err != nil {
		return err
	}
	if weight <= 0 {
		return ErrInvalidWeight
	}
	return nil
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
