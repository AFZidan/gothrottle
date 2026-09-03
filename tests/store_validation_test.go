// FILENAME: store_validation_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// The exact-Lua boundary: 2^53-1 is the largest integer Redis Lua's IEEE-754
// numbers represent exactly, and Redis is where admission is decided, so nothing
// above it can be enforced as written.
//
// These are vars rather than consts so the int conversion happens at run time. As
// a constant it would not compile on a 32-bit platform, where the boundary does
// not fit in an int — and there the right answer is to skip, not to fail to
// build. exactBoundaryReachable says which case applies.
var (
	maxExactLua            int64 = 1<<53 - 1
	maxExactInt                  = int(maxExactLua)
	exactBoundaryReachable       = int64(maxExactInt) == maxExactLua
	maxDurationForTests          = time.Duration(1<<63 - 1)
)

// requireExactBoundary skips a test whose subject is the 2^53 boundary when the
// platform's int cannot reach it.
func requireExactBoundary(t *testing.T) {
	t.Helper()

	if !exactBoundaryReachable {
		t.Skip("int is too narrow on this platform to reach the 2^53 boundary")
	}
}

// admissionCase is an admission call every store must reject identically.
type admissionCase struct {
	name   string
	weight int
	opts   gothrottle.Options
	want   error
	// boundary marks a case that only exists on a platform whose int can reach
	// 2^53.
	boundary bool
}

// invalidAdmissionCases are the option and weight combinations every admission
// path must reject. NewLimiter has always run Options.Validate, but a direct
// LocalStore.Request/Acquire or RedisStore.Request/Acquire call did not, so a
// negative MaxConcurrent reached the store, failed its `> 0` guard and was
// honored as "unlimited" — exactly the silent failure validation exists to
// prevent.
func invalidAdmissionCases() []admissionCase {
	return []admissionCase{
		{
			name:   "negative MaxConcurrent",
			weight: 1,
			opts:   gothrottle.Options{MaxConcurrent: -1},
			want:   gothrottle.ErrInvalidMaxConcurrent,
		},
		{
			name:   "negative MinTime",
			weight: 1,
			opts:   gothrottle.Options{MinTime: -time.Second},
			want:   gothrottle.ErrInvalidMinTime,
		},
		{
			name:   "negative LeaseTTL",
			weight: 1,
			opts:   gothrottle.Options{LeaseTTL: -time.Second},
			want:   gothrottle.ErrInvalidLeaseTTL,
		},
		{
			name:   "negative MaxQueueSize",
			weight: 1,
			opts:   gothrottle.Options{MaxQueueSize: -1},
			want:   gothrottle.ErrInvalidMaxQueueSize,
		},
		{
			name:   "negative RetryInterval",
			weight: 1,
			opts:   gothrottle.Options{RetryInterval: -time.Millisecond},
			want:   gothrottle.ErrInvalidRetryInterval,
		},
		{
			name:   "unknown SchedPolicy",
			weight: 1,
			opts:   gothrottle.Options{SchedPolicy: gothrottle.SchedPolicy(42)},
			want:   gothrottle.ErrInvalidSchedPolicy,
		},
		{
			name:   "zero weight",
			weight: 0,
			opts:   gothrottle.Options{MaxConcurrent: 2},
			want:   gothrottle.ErrInvalidWeight,
		},
		{
			name:   "negative weight",
			weight: -3,
			opts:   gothrottle.Options{MaxConcurrent: 2},
			want:   gothrottle.ErrInvalidWeight,
		},
		{
			name:   "weight above MaxConcurrent",
			weight: 5,
			opts:   gothrottle.Options{MaxConcurrent: 2},
			want:   gothrottle.ErrWeightExceedsMax,
		},
		{
			name:   "malformed Options.ID",
			weight: 1,
			opts:   gothrottle.Options{ID: "bad\x00id"},
			want:   gothrottle.ErrInvalidID,
		},
		{
			name:     "MaxConcurrent beyond exact Lua range",
			weight:   1,
			opts:     gothrottle.Options{MaxConcurrent: maxExactInt + 1},
			want:     gothrottle.ErrValueOutOfRange,
			boundary: true,
		},
		{
			name:     "weight beyond exact Lua range",
			weight:   maxExactInt + 1,
			opts:     gothrottle.Options{},
			want:     gothrottle.ErrValueOutOfRange,
			boundary: true,
		},
		{
			name:   "MinTime beyond exact Lua range in microseconds",
			weight: 1,
			opts:   gothrottle.Options{MinTime: maxDurationForTests},
			want:   gothrottle.ErrValueOutOfRange,
		},
		{
			name:   "LeaseTTL beyond exact Lua range in microseconds",
			weight: 1,
			opts:   gothrottle.Options{LeaseTTL: maxDurationForTests},
			want:   gothrottle.ErrValueOutOfRange,
		},
	}
}

func TestLocalStore_DirectCallsEnforceOptions(t *testing.T) {
	for _, tc := range invalidAdmissionCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.boundary {
				requireExactBoundary(t)
			}

			store := gothrottle.NewLocalStore()
			defer func() { _ = store.Disconnect() }()

			canRun, wait, err := store.Request("direct-request", tc.weight, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("LocalStore.Request = (%v, %v, %v), want error %v", canRun, wait, err, tc.want)
			}
			if canRun {
				t.Fatal("LocalStore.Request admitted a job it reported an error for")
			}

			lease, retryAfter, err := store.Acquire(context.Background(), "direct-acquire", tc.weight, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("LocalStore.Acquire = (%v, %v, %v), want error %v", lease, retryAfter, err, tc.want)
			}
			if lease != nil {
				t.Fatal("LocalStore.Acquire returned a lease alongside an error")
			}

			// Rejection must leave no state behind: a later valid call on the same
			// ID has to see a clean slate.
			if canRun, _, err := store.Request("direct-request", 1, gothrottle.Options{MaxConcurrent: 1}); err != nil || !canRun {
				t.Fatalf("after a rejected call, Request = (%v, %v), want (true, nil)", canRun, err)
			}
		})
	}
}

// TestRedisStore_DirectCallsEnforceOptions is the Redis counterpart, and also
// asserts a rejected call writes nothing. Validation has to precede token
// generation and every Redis command: otherwise a rejected client could register
// a configuration, extend a TTL or move the spacing window it was rejected over.
func TestRedisStore_DirectCallsEnforceOptions(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()

	for _, tc := range invalidAdmissionCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.boundary {
				requireExactBoundary(t)
			}

			id := uniqueLimiterID("validate")
			keys := gothrottle.RedisKeys(id)
			all := []string{keys.Leases, keys.Expirations, keys.LastStart, keys.Config, gothrottle.RedisStateKey(id)}

			canRun, wait, err := store.Request(id, tc.weight, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RedisStore.Request = (%v, %v, %v), want error %v", canRun, wait, err, tc.want)
			}
			if canRun {
				t.Fatal("RedisStore.Request admitted a job it reported an error for")
			}

			lease, retryAfter, err := store.Acquire(ctx, id, tc.weight, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RedisStore.Acquire = (%v, %v, %v), want error %v", lease, retryAfter, err, tc.want)
			}
			if lease != nil {
				t.Fatal("RedisStore.Acquire returned a lease alongside an error")
			}

			if existing, err := client.Exists(ctx, all...).Result(); err != nil {
				t.Fatalf("EXISTS failed: %v", err)
			} else if existing != 0 {
				t.Fatalf("a rejected call created %d of this limiter's keys; validation must precede every Redis command", existing)
			}
		})
	}
}

// TestRedisStore_MissingIDIsRejectedBeforeRedis keeps the empty-ID case on the
// same footing: RedisStore turns the ID into a key, so it must be refused before
// anything is written.
func TestRedisStore_MissingIDIsRejectedBeforeRedis(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	opts := gothrottle.Options{MaxConcurrent: 1}
	if _, _, err := store.Request("", 1, opts); !errors.Is(err, gothrottle.ErrMissingID) {
		t.Fatalf("Request with empty ID = %v, want ErrMissingID", err)
	}
	if _, _, err := store.Acquire(context.Background(), "", 1, opts); !errors.Is(err, gothrottle.ErrMissingID) {
		t.Fatalf("Acquire with empty ID = %v, want ErrMissingID", err)
	}
	if err := store.RegisterDone("", 1); !errors.Is(err, gothrottle.ErrMissingID) {
		t.Fatalf("RegisterDone with empty ID = %v, want ErrMissingID", err)
	}
}

// TestStores_NegativeMaxConcurrentIsNotUnlimited is the behavioral half of the
// regression. Before validation reached the stores, a negative MaxConcurrent
// failed the `> 0` guard in both admission paths and every request was admitted,
// so a limiter configured with a miscalculated limit enforced nothing at all.
func TestStores_NegativeMaxConcurrentIsNotUnlimited(t *testing.T) {
	store := gothrottle.NewLocalStore()
	defer func() { _ = store.Disconnect() }()

	opts := gothrottle.Options{MaxConcurrent: -1}
	for i := 0; i < 3; i++ {
		canRun, _, err := store.Request("unlimited-by-accident", 1, opts)
		if canRun {
			t.Fatalf("Request %d with MaxConcurrent -1 was admitted; a negative limit is not 'unlimited'", i)
		}
		if !errors.Is(err, gothrottle.ErrInvalidMaxConcurrent) {
			t.Fatalf("Request %d = %v, want ErrInvalidMaxConcurrent", i, err)
		}
	}
}

// TestLocalStore_RegisterDoneValidatesID pins the completion path's validation,
// which is deliberately narrower: RegisterDone carries no Options, so there is
// nothing else to check.
func TestLocalStore_RegisterDoneValidatesID(t *testing.T) {
	store := gothrottle.NewLocalStore()
	defer func() { _ = store.Disconnect() }()

	if err := store.RegisterDone("bad\x00id", 1); !errors.Is(err, gothrottle.ErrInvalidID) {
		t.Fatalf("RegisterDone(malformed id) = %v, want ErrInvalidID", err)
	}
	if err := store.RegisterDone("fine", 0); !errors.Is(err, gothrottle.ErrInvalidWeight) {
		t.Fatalf("RegisterDone(weight 0) = %v, want ErrInvalidWeight", err)
	}
}

// TestLimiter_RejectsWeightBeyondExactLuaRange covers the scheduler-side range
// check. With no MaxConcurrent there is no limit to compare against, so without
// this the weight would travel to Redis and be summed as an approximation.
func TestLimiter_RejectsWeightBeyondExactLuaRange(t *testing.T) {
	requireExactBoundary(t)

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	_, err = limiter.ScheduleWithOptions(func() (interface{}, error) { return nil, nil }, 5, maxExactInt+1)
	if !errors.Is(err, gothrottle.ErrValueOutOfRange) {
		t.Fatalf("ScheduleWithOptions(weight 2^53) = %v, want ErrValueOutOfRange", err)
	}
}

// TestOptions_ExactRangeBoundaryIsInclusive checks the boundary is a range check
// and not a new, lower cap: the largest exactly representable value is still
// valid configuration.
func TestOptions_ExactRangeBoundaryIsInclusive(t *testing.T) {
	requireExactBoundary(t)

	opts := gothrottle.Options{MaxConcurrent: maxExactInt}
	if err := opts.Validate(); err != nil {
		t.Fatalf("Validate with MaxConcurrent 2^53-1 = %v, want nil", err)
	}
	if err := (gothrottle.Options{MaxConcurrent: maxExactInt + 1}).Validate(); !errors.Is(err, gothrottle.ErrValueOutOfRange) {
		t.Fatalf("Validate with MaxConcurrent 2^53 = %v, want ErrValueOutOfRange", err)
	}
}

// TestRedisStore_LargeButExactValuesAreEnforcedExactly runs the boundary through
// Redis. If Lua rounded the sum, a full limit would still look like it had room.
func TestRedisStore_LargeButExactValuesAreEnforcedExactly(t *testing.T) {
	requireExactBoundary(t)

	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("max-exact")
	keys := gothrottle.RedisKeys(id)
	opts := gothrottle.Options{MaxConcurrent: maxExactInt, LeaseTTL: 10 * time.Second}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys.Leases, keys.Expirations, keys.LastStart, keys.Config).Err()
	})

	lease, _, err := store.Acquire(ctx, id, maxExactInt, opts)
	if err != nil {
		t.Fatalf("Acquire at the exact-range boundary failed: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire at the exact-range boundary was refused")
	}

	blocked, retryAfter, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatal("a full MaxConcurrent admitted one more unit of weight; the Lua sum was rounded")
	}
	if retryAfter != 0 {
		t.Fatalf("capacity refusal carried retryAfter %v, want 0", retryAfter)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestRedisStore_LegacyWaitTimeClampedToWindow covers the legacy spacing path's
// clock guard. The lease script already clamped; Request did not, so a Redis
// clock that moved backwards produced a wait longer than MinTime itself and
// stalled the caller past the spacing it actually owed.
//
// The backwards jump is simulated by writing a future last_start_us directly,
// which is the state a rewound clock leaves behind.
func TestRedisStore_LegacyWaitTimeClampedToWindow(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("legacy-clock-skew")
	key := gothrottle.RedisStateKey(id)
	opts := gothrottle.Options{MinTime: 200 * time.Millisecond}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	now, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatalf("TIME failed: %v", err)
	}
	// An hour in the future: as if the server clock had been stepped back an hour
	// after the last admission.
	future := now.Add(time.Hour).UnixNano() / int64(time.Microsecond)
	if err := client.HSet(ctx, key, "running", 0, "last_start_us", future).Err(); err != nil {
		t.Fatalf("HSET failed: %v", err)
	}

	canRun, waitTime, err := store.Request(id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if canRun {
		t.Fatal("a future last-start admitted the job immediately")
	}
	if waitTime <= 0 {
		t.Fatalf("waitTime = %v, want a positive bounded wait", waitTime)
	}
	if waitTime > opts.MinTime {
		t.Fatalf("waitTime = %v, want at most MinTime %v; a backwards clock must not inflate the wait", waitTime, opts.MinTime)
	}
}

// TestRedisStore_LegacyStateTTLSurvivesHugeMinTime pins the overflow guard on
// 2*MinTime. Doubling a MinTime past half the Duration range wrapped negative,
// which compared as "shorter than the default" and left the window unprotected —
// the opposite of what the doubling is for.
func TestRedisStore_LegacyStateTTLSurvivesHugeMinTime(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("legacy-huge-mintime")
	key := gothrottle.RedisStateKey(id)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	// Just past half the Duration range, where 2*MinTime overflows, but still
	// inside the exact-Lua range so validation admits it.
	opts := gothrottle.Options{MinTime: time.Duration(1<<62 + 1)}
	if err := opts.Validate(); err != nil {
		t.Fatalf("MinTime %v should be valid: %v", opts.MinTime, err)
	}

	canRun, _, err := store.Request(id, 1, opts)
	if err != nil {
		t.Fatalf("Request with a huge MinTime failed: %v", err)
	}
	if !canRun {
		t.Fatal("the first Request was refused; there is no spacing history yet")
	}

	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("PTTL(%s) = %v, want a positive TTL; an overflowed doubling left no usable expiry", key, ttl)
	}
	if ttl < 30*time.Second {
		t.Fatalf("PTTL(%s) = %v, want at least the 30s default", key, ttl)
	}
}

// TestLease_StateWindowsSaturate checks the same guard on the lease path, where
// the doubled windows become PEXPIRE arguments. A wrapped negative would be
// rejected by Redis or round to an immediate deletion.
func TestLease_StateWindowsSaturate(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("huge-windows")
	keys := gothrottle.RedisKeys(id)
	opts := gothrottle.Options{
		MaxConcurrent: 1,
		MinTime:       time.Duration(1<<62 + 1),
		LeaseTTL:      time.Duration(1<<62 + 1),
	}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys.Leases, keys.Expirations, keys.LastStart, keys.Config).Err()
	})

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatalf("Acquire with saturating windows failed: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire with saturating windows was refused")
	}

	// pttl fails the test on a missing key or a missing expiry, which is what a
	// wrapped negative would produce.
	for _, key := range []string{keys.Leases, keys.Expirations, keys.LastStart, keys.Config} {
		pttl(t, client, key)
	}
}

// TestRedisStore_HugeWaitReplyDoesNotWrap covers converting a Redis microsecond
// reply into a Duration. The reply is external input: scaling a value past
// maxDuration/1000 by 1000 wraps negative, and the scheduler reads a non-positive
// wait as "no deadline", so it stops waiting for the window at all.
func TestRedisStore_HugeWaitReplyDoesNotWrap(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("huge-wait")
	key := gothrottle.RedisStateKey(id)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	opts := gothrottle.Options{MinTime: time.Duration(1<<62 + 1)}
	if canRun, _, err := store.Request(id, 1, opts); err != nil || !canRun {
		t.Fatalf("first Request = (%v, %v), want (true, nil)", canRun, err)
	}

	canRun, waitTime, err := store.Request(id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if canRun {
		t.Fatal("the second Request bypassed a huge MinTime window")
	}
	if waitTime <= 0 {
		t.Fatalf("waitTime = %v, want positive; a wrapped conversion reads as 'no deadline'", waitTime)
	}
	if waitTime > opts.MinTime {
		t.Fatalf("waitTime = %v, want at most MinTime %v", waitTime, opts.MinTime)
	}
}

// TestLocalStore_WeightSumsDoNotWrap checks the local running total. A wrapped
// negative sum compares as free capacity, which is the one answer a limiter must
// never give.
func TestLocalStore_WeightSumsDoNotWrap(t *testing.T) {
	requireExactBoundary(t)

	store := gothrottle.NewLocalStore()
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := "saturating-weights"
	opts := gothrottle.Options{MaxConcurrent: maxExactInt, LeaseTTL: time.Minute}

	// Two leases that together fill the limit exactly. Summed as ints this is
	// fine; the point is that a third weight-1 job must still be refused.
	half := maxExactInt / 2
	first, _, err := store.Acquire(ctx, id, half, opts)
	if err != nil || first == nil {
		t.Fatalf("first Acquire = (%v, %v), want a lease", first, err)
	}
	second, _, err := store.Acquire(ctx, id, maxExactInt-half, opts)
	if err != nil || second == nil {
		t.Fatalf("second Acquire = (%v, %v), want a lease", second, err)
	}

	third, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Fatal("a full MaxConcurrent admitted another job; the weight sum wrapped")
	}

	if err := store.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, second); err != nil {
		t.Fatal(err)
	}
}

// TestStores_RejectExactlyWhatValidateRejects is the invariant behind "one
// validator, every path": every Options-level mistake is reported by
// Options.Validate and by a direct store call, with the same sentinel, so the
// entry point cannot change whether a misconfiguration is caught.
//
// Sentinels are compared rather than error values because the range errors wrap
// their sentinel with a message naming the field, so two independently
// constructed instances are not equal to each other — only to what they wrap.
func TestStores_RejectExactlyWhatValidateRejects(t *testing.T) {
	store := gothrottle.NewLocalStore()
	defer func() { _ = store.Disconnect() }()

	for _, tc := range invalidAdmissionCases() {
		tc := tc
		if tc.boundary && !exactBoundaryReachable {
			continue
		}
		// Weight mistakes have no Options.Validate equivalent, so there is
		// nothing to compare for them.
		if tc.opts.Validate() == nil {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Options.Validate = %v, want %v", err, tc.want)
			}
			if _, _, err := store.Request("cmp", 1, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("LocalStore.Request = %v, want %v", err, tc.want)
			}
			if _, _, err := store.Acquire(context.Background(), "cmp", 1, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("LocalStore.Acquire = %v, want %v", err, tc.want)
			}
		})
	}
}
