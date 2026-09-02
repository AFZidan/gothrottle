// FILENAME: lease.go
package gothrottle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Lease errors.
var (
	// ErrLeaseLost is returned when renewing or releasing a lease that the
	// store no longer holds, because it expired or was already released.
	ErrLeaseLost = errors.New("lease is no longer held")

	// ErrNilLease is returned when a nil lease is passed to Renew or Release.
	ErrNilLease = errors.New("lease must not be nil")
)

// defaultLeaseTTL is how long a lease survives without renewal. A process that
// dies mid-job has its capacity reclaimed within this window.
const defaultLeaseTTL = 30 * time.Second

// minLeaseTTL keeps a TTL long enough that the renewal interval (TTL/3) stays
// above typical network jitter.
const minLeaseTTL = time.Second

// Lease is a reservation of capacity held by one job. Unlike a shared counter,
// each lease is individually identified, so releasing one cannot disturb
// another, and an expired lease can be reclaimed without guessing how much
// weight it accounted for.
type Lease struct {
	// Token uniquely identifies this reservation. Release and Renew act on the
	// token, so a stale release from a job whose lease already expired cannot
	// decrement a newer job's reservation.
	Token string
	// LimiterID is the limiter the capacity belongs to.
	LimiterID string
	// Weight is the capacity held.
	Weight int
	// TTL is the window each renewal grants. Renew reuses it so extending a
	// lease never silently changes how long a crashed holder would keep the
	// capacity.
	TTL time.Duration
	// ExpiresAt is when the store will reclaim this lease unless it is renewed.
	// It is the store's clock, not the caller's.
	ExpiresAt time.Time
}

// ttlOrDefault returns the lease's renewal window, falling back to the package
// default for a lease built without one.
func (l *Lease) ttlOrDefault() time.Duration {
	if l.TTL > 0 {
		return l.TTL
	}
	return defaultLeaseTTL
}

// LeaseDatastore is a Datastore that tracks individual reservations rather than
// a single shared counter.
//
// A counter cannot express "this job is still running": the only way to keep
// state from leaking after a crash is to expire it, and expiring a counter while
// a job is still running lets another job start over the limit. Per-lease expiry
// with renewal separates "the holder is gone" from "the holder is slow".
//
// A store may implement this alongside Datastore; the limiter uses the lease
// path when available and falls back to Request/RegisterDone otherwise.
//
// Every method takes a context, and the limiter passes one that is cancelled
// when Limiter.Stop begins, so an implementation that blocks — on a network
// round trip, a lock, or a queue — must observe it. Cancellation guarantees are
// therefore stronger here than on the legacy Datastore methods, which take no
// context at all and can only be waited out.
type LeaseDatastore interface {
	Datastore

	// Acquire reserves weight for limiterID. When capacity is unavailable it
	// returns a nil lease and, if the wait is bounded (a MinTime window),
	// how long to wait before retrying.
	//
	// A distributed implementation may require every instance sharing a
	// limiterID to agree on the admission-relevant configuration —
	// MaxConcurrent, MinTime and LeaseTTL — and report a disagreement as an
	// error matching ErrLimiterConfigMismatch. RedisStore does; LocalStore has
	// no other process to disagree with.
	Acquire(ctx context.Context, limiterID string, weight int, opts Options) (lease *Lease, retryAfter time.Duration, err error)

	// Renew extends a lease's expiry. It returns ErrLeaseLost if the lease has
	// already expired or been released, which means the capacity has been
	// handed to someone else and the caller is now over the limit.
	//
	// Renewal must not disturb rate-spacing history: MinTime is measured from
	// when a job started, and that fact outlives the reservation.
	Renew(ctx context.Context, lease *Lease) error

	// Release returns a lease's capacity. Releasing an unknown or expired lease
	// is not an error: the store has already reclaimed it, and reporting a
	// failure would only invite a retry that cannot help.
	//
	// Like Renew, it must leave rate-spacing history alone.
	Release(ctx context.Context, lease *Lease) error
}

// leaseTTL returns the effective lease duration for these options.
func (o Options) leaseTTL() time.Duration {
	ttl := o.LeaseTTL
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	if ttl < minLeaseTTL {
		ttl = minLeaseTTL
	}
	return ttl
}

// leaseConfig is the part of Options that decides admission in a shared store,
// normalized to the values the Lua scripts compare. Local scheduler settings —
// RetryInterval, MaxQueueSize, SchedPolicy, OnError — are deliberately absent:
// they change how one process queues work, not what the store admits, so two
// instances may legitimately differ on them.
//
// Every field is int64, matching what Redis returns. Narrowing maxConcurrent
// back to int would be unsound on a 32-bit build, where a stored value above
// 2^31-1 wraps — and this value comes from Redis, not from local Options.
type leaseConfig struct {
	maxConcurrent int64
	minTimeUS     int64
	leaseTTLUS    int64
}

// leaseConfig returns the admission-relevant configuration these options
// describe.
func (o Options) leaseConfig() leaseConfig {
	return leaseConfig{
		maxConcurrent: int64(o.MaxConcurrent),
		minTimeUS:     o.MinTime.Microseconds(),
		leaseTTLUS:    o.leaseTTL().Microseconds(),
	}
}

// String renders a configuration for an error message.
func (c leaseConfig) String() string {
	return fmt.Sprintf("MaxConcurrent=%d MinTime=%v LeaseTTL=%v",
		c.maxConcurrent,
		time.Duration(c.minTimeUS)*time.Microsecond,
		time.Duration(c.leaseTTLUS)*time.Microsecond,
	)
}

// configMismatchError describes a rejected acquisition, naming both
// configurations so the disagreeing instance is identifiable from a log line.
//
// storedID is the limiter ID the configuration was registered under. Lease keys
// are named from a hash of the ID, so comparing it turns an astronomically
// unlikely hash collision into a reported mismatch rather than two limiters
// silently sharing capacity.
func configMismatchError(limiterID string, requested, stored leaseConfig, storedID string) error {
	if storedID != "" && storedID != limiterID {
		return fmt.Errorf("%w: limiter %q collides with limiter %q on the same Redis keys",
			ErrLimiterConfigMismatch, limiterID, storedID)
	}
	return fmt.Errorf("%w: limiter %q is registered with %s but this instance requested %s",
		ErrLimiterConfigMismatch, limiterID, stored, requested)
}

// configLifetime is how long a limiter ID's registered configuration is held.
// It must outlast both a lease nobody renews and the spacing window, otherwise
// a conflicting configuration could be accepted while either is still in force.
// A different configuration is only allowed once the limiter is genuinely idle,
// which is exactly when both windows have lapsed.
func configLifetime(leaseTTL, minTime time.Duration) time.Duration {
	lifetime := leaseStateWindow(leaseTTL)
	if spacing := spacingStateWindow(minTime); spacing > lifetime {
		lifetime = spacing
	}
	return lifetime
}

// minStateWindow floors the windows below. In Redis they become PEXPIRE
// arguments, and a sub-millisecond MinTime would otherwise round to 0, which
// deletes a key outright.
const minStateWindow = time.Second

// leaseStateWindow is how long reservation state for an idle limiter is kept.
// Correctness comes from per-lease expiry, so this only has to outlast a lease
// nobody renews.
func leaseStateWindow(leaseTTL time.Duration) time.Duration {
	window := doubleDuration(leaseTTL)
	if window < minStateWindow {
		window = minStateWindow
	}
	return window
}

// spacingStateWindow is how long the last-start record is kept. It derives from
// MinTime alone: the record exists only to enforce spacing, and tying it to the
// lease TTL is what previously let a released or renewed lease shorten a long
// spacing window into oblivion.
func spacingStateWindow(minTime time.Duration) time.Duration {
	window := doubleDuration(minTime)
	if window < minStateWindow {
		window = minStateWindow
	}
	return window
}

// renewInterval is how often a held lease is renewed. A third of the TTL allows
// two consecutive failures before the lease is lost.
func (o Options) renewInterval() time.Duration {
	interval := o.leaseTTL() / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	return interval
}

// newLeaseToken returns an unguessable lease identifier. Tokens are generated by
// the caller so that a lease can be released idempotently: the same token always
// refers to the same reservation, even across a retry.
func newLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate lease token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
