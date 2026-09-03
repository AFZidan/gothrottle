// FILENAME: bounds.go
package gothrottle

import "time"

// Numeric policy
//
// Three number systems meet in this package and none of them agrees with the
// others about range. Go `int` is 32 bits on some builds and 64 on others.
// time.Duration is an int64 nanosecond count, so it tops out near 292 years.
// Redis Lua numbers are IEEE-754 doubles, exact only to 2^53-1 — and Redis is
// where admission is actually decided.
//
// The policy is: reject at validation what cannot be enforced exactly, and make
// the arithmetic that survives validation overflow-proof rather than trusting the
// range.
//
//   - MaxConcurrent, job weights, and MinTime/LeaseTTL as microsecond counts are
//     all rejected above maxExactLuaInt with ErrValueOutOfRange (see
//     Options.Validate). Above that boundary two distinct values can compare
//     equal inside Lua, so the limit being enforced would not be the limit that
//     was configured.
//   - Every value handed to a Lua script goes through luaMicros, luaMillis or
//     luaInt, which apply the same bound. That includes the legacy RegisterDone
//     path and the TTLs derived from a caller-constructed Lease, neither of which
//     passes through Options.Validate.
//   - Sub-microsecond durations round *up* to 1µs rather than truncating to 0.
//     Truncation silently disabled the spacing it was asked to enforce; rounding
//     up errs toward more spacing, which is the safe direction.
//   - Arithmetic that survives validation still saturates rather than wrapping,
//     because the derived windows (2 × MinTime) and running-weight sums can
//     exceed their types even when every input was individually valid.
//   - Weight accumulation saturates at maxInt rather than wrapping negative,
//     which would read as free capacity.
//   - Capacity comparisons are written as subtraction against the limit — in Go
//     and in both Lua scripts — so no sum is formed that could exceed its type.
//   - Values arriving from Redis are treated as untrusted and clamped on
//     conversion, so a nonsense reply cannot become a negative wait that the
//     scheduler reads as "no deadline".

// addClamped returns a+b, saturating at maxInt instead of wrapping. Both
// arguments are non-negative in every caller; a wrapped negative total would read
// as free capacity, which is the one outcome a limiter must never produce.
func addClamped(a, b int) int {
	if a > maxInt-b {
		return maxInt
	}
	return a + b
}

// fitsWithin reports whether weight can be admitted on top of running without
// exceeding maxConcurrent. A non-positive maxConcurrent means unlimited.
//
// The comparison is subtraction rather than "running + weight > maxConcurrent"
// because that sum can exceed int on a 32-bit build even when both operands are
// individually valid: the wrap turns a refusal into an admission. Both terms of
// the subtraction are non-negative, so it cannot itself overflow.
func fitsWithin(running, weight, maxConcurrent int) bool {
	if maxConcurrent <= 0 {
		return true
	}
	if running >= maxConcurrent {
		return false
	}
	return weight <= maxConcurrent-running
}

// doubleDuration returns 2*d, saturating at the largest Duration. It backs the
// state windows, which are sized to outlast the thing they protect: a wrapped
// negative would shorten a window instead of extending it, and a shortened
// window means throttling state expiring while it is still in force.
func doubleDuration(d time.Duration) time.Duration {
	if d > maxDuration/2 {
		return maxDuration
	}
	return 2 * d
}

// maxDuration is the largest representable time.Duration.
const maxDuration = time.Duration(1<<63 - 1)

// microsToDuration converts a microsecond count from Redis into a Duration,
// clamping instead of overflowing. Redis replies are external input: a value
// above maxDuration/1000 would wrap into a negative wait, which the scheduler
// would read as "no deadline" and stop waiting on entirely.
func microsToDuration(us int64) time.Duration {
	if us <= 0 {
		return 0
	}
	if us > int64(maxDuration/time.Microsecond) {
		return maxDuration
	}
	return time.Duration(us) * time.Microsecond
}

// microsToTime converts a Redis microsecond timestamp to a time.Time. Scaling to
// nanoseconds overflows int64 somewhere past the year 2262, and a wrapped
// negative would present a live lease as long expired, so the conversion is
// clamped. Redis TIME itself is well inside the range — microseconds since the
// epoch stay exact as a Lua double until the year 2255 — but the value arrives
// over the wire and is treated as untrusted.
func microsToTime(us int64) time.Time {
	const maxMicros = int64(maxDuration) / int64(time.Microsecond)
	if us <= 0 {
		return time.Time{}
	}
	if us > maxMicros {
		us = maxMicros
	}
	return time.Unix(0, us*int64(time.Microsecond))
}

// durationMicros converts a duration to whole microseconds, rounding a positive
// remainder *up*.
//
// Truncating is what .Microseconds() does, and it turns a sub-microsecond MinTime
// into 0 — and 0 means "no spacing at all", so a caller who asked for a window
// silently got none. Rounding up yields 1µs: not the window that was requested
// either, since Redis cannot express finer, but wrong in the direction of more
// spacing rather than none.
//
// Values already on a microsecond boundary are unchanged, so nothing exact
// becomes inexact.
func durationMicros(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	us := d / time.Microsecond
	if d%time.Microsecond != 0 {
		us++
	}
	return int64(us)
}

// maxSafeMillis is the largest millisecond duration that can be passed to Redis
// PEXPIRE and read back via PTTL without overflowing time.Duration (int64 nanoseconds).
const maxSafeMillis = (maxDuration - 365*24*time.Hour) / time.Millisecond

// durationMillis converts a duration to whole milliseconds, rounding a positive
// remainder up. These become PEXPIRE arguments, where truncating a sub-millisecond
// window to 0 deletes the key outright — the opposite of setting an expiry.
func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	if ms > maxSafeMillis {
		ms = maxSafeMillis
	}
	return int64(ms)
}

// luaMicros is durationMicros plus the exact-range check, for a duration that is
// about to be handed to a Lua script. field names the value in the error.
//
// Everything crossing into Lua goes through this or its siblings, not just what
// Options.Validate covers: the legacy RegisterDone path takes no Options at all,
// and Renew and Release derive their TTLs from a caller-constructed Lease that no
// validator has seen.
func luaMicros(field string, d time.Duration) (int64, error) {
	us := durationMicros(d)
	if err := checkLuaExactRange(field, us); err != nil {
		return 0, err
	}
	return us, nil
}

// luaMillis is durationMillis plus the exact-range check.
func luaMillis(field string, d time.Duration) (int64, error) {
	ms := durationMillis(d)
	if err := checkLuaExactRange(field, ms); err != nil {
		return 0, err
	}
	return ms, nil
}

// luaInt widens an int for Lua after checking it is exactly representable there.
func luaInt(field string, value int) (int64, error) {
	widened := int64(value)
	if err := checkLuaExactRange(field, widened); err != nil {
		return 0, err
	}
	return widened, nil
}
