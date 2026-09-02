// FILENAME: config_consistency_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
	"github.com/go-redis/redis/v8"
)

// A limiter ID names a shared policy. If two processes use the same ID with
// different MaxConcurrent, MinTime or LeaseTTL, the effective distributed limit
// becomes whichever process happened to call Redis — so the store records the
// admission configuration on first use and rejects a later disagreement instead
// of silently resolving it.
//
// Only datastore semantics count. RetryInterval, MaxQueueSize, SchedPolicy and
// OnError shape how one process queues work locally, so instances may differ on
// them freely.

// configStores runs a body against both lease stores, using two independent
// store instances for the Redis case — as two application processes would.
func configStores(t *testing.T, body func(t *testing.T, first, second gothrottle.LeaseDatastore)) {
	t.Helper()

	t.Run("local", func(t *testing.T) {
		// One LocalStore is the whole process's state, so both "instances" are
		// the same store — which is exactly how a shared ID behaves in-process.
		store := gothrottle.NewLocalStore()
		defer func() { _ = store.Disconnect() }()
		body(t, store, store)
	})

	t.Run("redis", func(t *testing.T) {
		first, err := gothrottle.NewRedisStore(newTestRedisClient(t))
		if err != nil {
			t.Fatalf("NewRedisStore failed: %v", err)
		}
		defer func() { _ = first.Disconnect() }()

		second, err := gothrottle.NewRedisStore(newTestRedisClient(t))
		if err != nil {
			t.Fatalf("NewRedisStore failed: %v", err)
		}
		defer func() { _ = second.Disconnect() }()

		body(t, first, second)
	})
}

func TestConfig_IdenticalConfigurationsCoordinate(t *testing.T) {
	configStores(t, func(t *testing.T, first, second gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("config-agree")
		opts := gothrottle.Options{MaxConcurrent: 2, MinTime: 10 * time.Millisecond, LeaseTTL: 2 * time.Second}

		a, _, err := first.Acquire(ctx, id, 1, opts)
		if err != nil || a == nil {
			t.Fatalf("first Acquire = (%v, %v), want a lease", a, err)
		}

		// Same configuration from another instance: it shares the limit rather
		// than getting its own.
		time.Sleep(20 * time.Millisecond)
		b, _, err := second.Acquire(ctx, id, 1, opts)
		if err != nil || b == nil {
			t.Fatalf("second Acquire = (%v, %v), want a lease", b, err)
		}

		time.Sleep(20 * time.Millisecond)
		third, _, err := second.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if third != nil {
			t.Fatal("two instances sharing an ID were each given the full MaxConcurrent")
		}

		if err := first.Release(ctx, a); err != nil {
			t.Fatalf("Release failed: %v", err)
		}
		if err := second.Release(ctx, b); err != nil {
			t.Fatalf("Release failed: %v", err)
		}
	})
}

func TestConfig_ConflictingSettingsAreRejected(t *testing.T) {
	base := gothrottle.Options{MaxConcurrent: 2, MinTime: 50 * time.Millisecond, LeaseTTL: 2 * time.Second}

	conflicts := map[string]gothrottle.Options{
		"MaxConcurrent": {MaxConcurrent: 5, MinTime: base.MinTime, LeaseTTL: base.LeaseTTL},
		"MinTime":       {MaxConcurrent: base.MaxConcurrent, MinTime: 500 * time.Millisecond, LeaseTTL: base.LeaseTTL},
		"LeaseTTL":      {MaxConcurrent: base.MaxConcurrent, MinTime: base.MinTime, LeaseTTL: 9 * time.Second},
	}

	for name, conflicting := range conflicts {
		conflicting := conflicting
		t.Run(name, func(t *testing.T) {
			configStores(t, func(t *testing.T, first, second gothrottle.LeaseDatastore) {
				ctx := context.Background()
				id := uniqueLimiterID("config-conflict-" + strings.ToLower(name))

				lease, _, err := first.Acquire(ctx, id, 1, base)
				if err != nil || lease == nil {
					t.Fatalf("first Acquire = (%v, %v), want a lease", lease, err)
				}

				_, _, err = second.Acquire(ctx, id, 1, conflicting)
				if !errors.Is(err, gothrottle.ErrLimiterConfigMismatch) {
					t.Fatalf("Acquire with a different %s = %v, want ErrLimiterConfigMismatch", name, err)
				}
				// The message has to name the disagreement, or an operator
				// cannot tell which instance is misconfigured.
				if !strings.Contains(err.Error(), id) {
					t.Fatalf("mismatch error does not name the limiter: %v", err)
				}

				// The agreeing instance is unaffected.
				if err := first.Renew(ctx, lease); err != nil {
					t.Fatalf("a rejected configuration disturbed a live lease: %v", err)
				}
				if err := first.Release(ctx, lease); err != nil {
					t.Fatalf("Release failed: %v", err)
				}
			})
		})
	}
}

// TestConfig_RejectionLeavesStateIntact checks a rejected acquisition is inert:
// no lease, no spacing update, no TTL change. Otherwise a misconfigured instance
// could disrupt a correctly configured one just by trying.
func TestConfig_RejectionLeavesStateIntact(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	other, err := gothrottle.NewRedisStore(newTestRedisClient(t))
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = other.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("config-inert")
	opts := gothrottle.Options{MaxConcurrent: 2, MinTime: 30 * time.Second, LeaseTTL: 4 * time.Second}
	keys := gothrottle.RedisKeys(id)

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}

	before := redisSnapshot(t, client, keys)

	// A second instance with a much shorter LeaseTTL: if the rejection were not
	// atomic it could shorten the keys protecting the live lease.
	shorter := opts
	shorter.LeaseTTL = time.Second
	shorter.MinTime = time.Millisecond
	if _, _, err := other.Acquire(ctx, id, 1, shorter); !errors.Is(err, gothrottle.ErrLimiterConfigMismatch) {
		t.Fatalf("Acquire with a conflicting configuration = %v, want ErrLimiterConfigMismatch", err)
	}

	after := redisSnapshot(t, client, keys)

	if after.leaseCount != before.leaseCount {
		t.Fatalf("rejected acquisition changed the lease count from %d to %d", before.leaseCount, after.leaseCount)
	}
	if after.lastStart != before.lastStart {
		t.Fatalf("rejected acquisition moved the spacing window from %q to %q", before.lastStart, after.lastStart)
	}
	// TTLs may only have counted down, never been reset shorter by the rejected
	// client's own (shorter) windows.
	if after.leaseTTL < before.leaseTTL-2*time.Second {
		t.Fatalf("rejected acquisition shortened the lease key TTL from %v to %v", before.leaseTTL, after.leaseTTL)
	}
	if after.startTTL < before.startTTL-2*time.Second {
		t.Fatalf("rejected acquisition shortened the last-start TTL from %v to %v", before.startTTL, after.startTTL)
	}
	if after.configTTL < before.configTTL-2*time.Second {
		t.Fatalf("rejected acquisition shortened the config TTL from %v to %v", before.configTTL, after.configTTL)
	}

	// And the original holder still owns its capacity.
	if err := store.Renew(ctx, lease); err != nil {
		t.Fatalf("Renew after a rejected acquisition failed: %v", err)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestConfig_NewConfigurationAllowedOnceIdle checks the record is not permanent:
// once every lease and the spacing window have lapsed, the ID may be registered
// with different settings. Otherwise a deployment could never change a limit.
func TestConfig_NewConfigurationAllowedOnceIdle(t *testing.T) {
	configStores(t, func(t *testing.T, first, second gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("config-idle")
		initial := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		lease, _, err := first.Acquire(ctx, id, 1, initial)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}
		if err := first.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		// Still in force: the reservation window has not lapsed.
		changed := gothrottle.Options{MaxConcurrent: 4, LeaseTTL: time.Second}
		if _, _, err := second.Acquire(ctx, id, 1, changed); !errors.Is(err, gothrottle.ErrLimiterConfigMismatch) {
			t.Fatalf("Acquire with a changed configuration while active = %v, want ErrLimiterConfigMismatch", err)
		}

		// 2 x LeaseTTL with no MinTime: the limiter is genuinely idle.
		time.Sleep(2200 * time.Millisecond)

		next, _, err := second.Acquire(ctx, id, 1, changed)
		if err != nil {
			t.Fatalf("Acquire with a changed configuration after idling = %v, want it to be accepted", err)
		}
		if next == nil {
			t.Fatal("Acquire returned no lease after the configuration was re-registered")
		}
		if err := second.Release(ctx, next); err != nil {
			t.Fatalf("Release failed: %v", err)
		}
	})
}

// TestConfig_LocalSchedulerOptionsMayDiffer pins what is deliberately *not*
// compared. Two instances with different queueing behavior still coordinate.
func TestConfig_LocalSchedulerOptionsMayDiffer(t *testing.T) {
	configStores(t, func(t *testing.T, first, second gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("config-local-opts")

		a := gothrottle.Options{
			MaxConcurrent: 2,
			LeaseTTL:      2 * time.Second,
			RetryInterval: 5 * time.Millisecond,
			MaxQueueSize:  10,
			SchedPolicy:   gothrottle.SchedStrict,
		}
		b := gothrottle.Options{
			MaxConcurrent: 2,
			LeaseTTL:      2 * time.Second,
			RetryInterval: 250 * time.Millisecond,
			MaxQueueSize:  1000,
			SchedPolicy:   gothrottle.SchedBestFit,
			OnError:       func(error) {},
		}

		lease, _, err := first.Acquire(ctx, id, 1, a)
		if err != nil || lease == nil {
			t.Fatalf("first Acquire = (%v, %v), want a lease", lease, err)
		}
		other, _, err := second.Acquire(ctx, id, 1, b)
		if err != nil {
			t.Fatalf("Acquire with different local scheduler options = %v, want it to be accepted", err)
		}
		if other == nil {
			t.Fatal("Acquire returned no lease despite available capacity")
		}

		if err := first.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}
		if err := second.Release(ctx, other); err != nil {
			t.Fatalf("Release failed: %v", err)
		}
	})
}

// TestConfig_LimiterReportsMismatchToCaller checks the error reaches the caller
// through the limiter rather than being swallowed as a generic datastore fault.
func TestConfig_LimiterReportsMismatchToCaller(t *testing.T) {
	store, err := gothrottle.NewRedisStore(newTestRedisClient(t))
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("config-limiter")

	agreeing, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            id,
		MaxConcurrent: 2,
		LeaseTTL:      3 * time.Second,
		Datastore:     store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agreeing.Stop() }()

	if _, err := agreeing.Schedule(func() (interface{}, error) { return "ok", nil }); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	disagreeing, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            id,
		MaxConcurrent: 7,
		LeaseTTL:      3 * time.Second,
		Datastore:     store,
		RetryInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disagreeing.Stop() }()

	_, err = disagreeing.Schedule(func() (interface{}, error) { return "should not run", nil })
	if !errors.Is(err, gothrottle.ErrLimiterConfigMismatch) {
		t.Fatalf("Schedule against a mismatched limiter = %v, want ErrLimiterConfigMismatch", err)
	}
}

// redisState is a point-in-time reading of one limiter's Redis keys.
type redisState struct {
	leaseCount int64
	lastStart  string
	leaseTTL   time.Duration
	startTTL   time.Duration
	configTTL  time.Duration
}

func redisSnapshot(t *testing.T, client *redis.Client, keys gothrottle.RedisKeyLayout) redisState {
	t.Helper()

	count, err := client.HLen(context.Background(), keys.Leases).Result()
	if err != nil {
		t.Fatalf("HLEN failed: %v", err)
	}
	lastStart, err := client.Get(context.Background(), keys.LastStart).Result()
	if err != nil {
		t.Fatalf("GET last-start failed: %v", err)
	}

	return redisState{
		leaseCount: count,
		lastStart:  lastStart,
		leaseTTL:   pttl(t, client, keys.Leases),
		startTTL:   pttl(t, client, keys.LastStart),
		configTTL:  pttl(t, client, keys.Config),
	}
}
