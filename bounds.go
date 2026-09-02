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
//   - Arithmetic that survives validation still saturates rather than wrapping,
//     because the derived windows (2 × MinTime) and running-weight sums can
//     exceed their types even when every input was individually valid.
//   - Weight accumulation saturates at maxInt rather than wrapping negative,
//     which would read as free capacity.
//   - Capacity comparisons are written as subtraction against the limit, so no
//     sum is formed that could exceed int on a 32-bit build.
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
