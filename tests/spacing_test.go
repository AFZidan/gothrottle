// FILENAME: spacing_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
	"github.com/go-redis/redis/v8"
)

// These tests pin the rule that rate-spacing history is independent of lease
// lifecycle. MinTime is measured from when a job *started*, so the record of
// that start has to outlive the reservation — through release, through renewal,
// and through the lease expiring because its holder crashed. Coupling the two,
// as the first lease implementation did, let a crashed holder hand the next job
// a free start.

// TestSpacing_SurvivesReleaseBeyondLeaseWindow proves the spacing window is not
// bounded by the reservation's garbage-collection window. With LeaseTTL at its
// 1s floor the reservation keys lapse after ~2s; the 4s MinTime must still be
// enforced after that.
func TestSpacing_SurvivesReleaseBeyondLeaseWindow(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("spacing-release")
		opts := gothrottle.Options{MinTime: 4 * time.Second, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}
		if err := store.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		// Past 2 x LeaseTTL: everything the reservation owned has expired.
		time.Sleep(2500 * time.Millisecond)

		next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil {
			t.Fatal("spacing was forgotten once the lease garbage-collection window passed")
		}
		if retryAfter <= 0 {
			t.Fatalf("retryAfter = %v, want a positive remaining window", retryAfter)
		}
		// ~1.5s should remain of the 4s window. A wait larger than MinTime would
		// mean the window restarted rather than continued.
		if retryAfter > 2*time.Second {
			t.Fatalf("retryAfter = %v, want roughly the 1.5s left of a 4s window", retryAfter)
		}
	})
}

// TestSpacing_SurvivesExpiredLease is the crashed-holder case: acquire, never
// renew, never release. The reservation must be reclaimed — that is what keeps a
// dead process from holding capacity — but the spacing window must not be.
func TestSpacing_SurvivesExpiredLease(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("spacing-crash")
		opts := gothrottle.Options{MaxConcurrent: 1, MinTime: 4 * time.Second, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		// The holder is gone. Wait past the lease TTL and the reservation keys'
		// own lifetime.
		time.Sleep(2500 * time.Millisecond)

		next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil {
			t.Fatal("a crashed holder's lease expiry erased the spacing window, letting the next job start early")
		}
		if retryAfter <= 0 || retryAfter > 2*time.Second {
			t.Fatalf("retryAfter = %v, want roughly the 1.5s left of a 4s window", retryAfter)
		}

		// Capacity itself must still have been reclaimed: the refusal above is
		// spacing, not the dead lease holding the slot. Once the window closes
		// the next job runs.
		time.Sleep(retryAfter + 200*time.Millisecond)

		reclaimed, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if reclaimed == nil {
			t.Fatal("capacity from a crashed holder was never reclaimed")
		}
	})
}

// TestSpacing_RenewalDoesNotShortenLongMinTime covers the TTL asymmetry that
// broke the previous implementation: Renew knows the lease TTL and nothing about
// MinTime, so refreshing the start record with the lease's window silently cut a
// long spacing window down to a couple of seconds.
//
// Renewal has to *stop* for that to show. While a job keeps renewing, the
// too-short window keeps being refreshed and looks healthy; the damage surfaces
// once the last renewal is more than 2 x LeaseTTL ago while the spacing window is
// still open.
func TestSpacing_RenewalDoesNotShortenLongMinTime(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("spacing-renew")
		opts := gothrottle.Options{MaxConcurrent: 2, MinTime: 4 * time.Second, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		for i := 0; i < 2; i++ {
			time.Sleep(300 * time.Millisecond)
			if err := store.Renew(ctx, lease); err != nil {
				t.Fatalf("Renew %d failed: %v", i+1, err)
			}
		}

		// Renewal stops here. 2 x LeaseTTL from the last renewal elapses at
		// ~2.6s; 2 x MinTime from the start would not until 8s.
		time.Sleep(2400 * time.Millisecond)

		// MaxConcurrent is 2, so capacity is available; only spacing may refuse.
		next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil {
			t.Fatal("renewing a lease shortened the spacing window it started")
		}
		if retryAfter <= 0 || retryAfter > 2*time.Second {
			t.Fatalf("retryAfter = %v, want roughly the 1s left of a 4s window", retryAfter)
		}
	})
}

// TestSpacing_RetryDurationTracksRemainingWindow checks the returned wait is a
// real countdown rather than a constant, which is what lets the scheduler sleep
// exactly as long as the window has left.
func TestSpacing_RetryDurationTracksRemainingWindow(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("spacing-countdown")
		const minTime = 2 * time.Second
		opts := gothrottle.Options{MinTime: minTime, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}
		if err := store.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		_, first, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if first <= 0 || first > minTime {
			t.Fatalf("first retryAfter = %v, want in (0, %v]", first, minTime)
		}

		time.Sleep(600 * time.Millisecond)

		_, second, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if second <= 0 {
			t.Fatalf("second retryAfter = %v, want still positive inside the window", second)
		}
		if second >= first {
			t.Fatalf("retryAfter did not shrink: %v then %v", first, second)
		}
		// The countdown must track elapsed time, not merely decrease.
		if elapsed := first - second; elapsed < 400*time.Millisecond || elapsed > 900*time.Millisecond {
			t.Fatalf("retryAfter fell by %v over a 600ms sleep, want roughly 600ms", elapsed)
		}

		time.Sleep(second + 200*time.Millisecond)

		next, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || next == nil {
			t.Fatalf("Acquire after the window = (%v, %v), want a lease", next, err)
		}
	})
}

// TestSpacing_RedisLastStartIsIndependentOfLeases asserts on the Redis state
// directly. A long MinTime with a short LeaseTTL is where the coupling used to
// show: release refreshed the start record with the lease's TTL, so a 45s window
// was protected by a ~2s key. The point is provable from the TTLs alone, without
// waiting 45 seconds.
func TestSpacing_RedisLastStartIsIndependentOfLeases(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("spacing-redis-keys")
	const minTime = 45 * time.Second
	opts := gothrottle.Options{MaxConcurrent: 2, MinTime: minTime, LeaseTTL: time.Second}
	keys := gothrottle.RedisKeys(id)

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}

	afterAcquire := pttl(t, client, keys.LastStart)
	if afterAcquire <= minTime {
		t.Fatalf("last-start TTL after Acquire = %v, want longer than MinTime %v", afterAcquire, minTime)
	}

	// Renewal must not touch it. It knows only the 1s lease TTL, so a refresh
	// here would leave ~2s protecting a 45s window.
	if err := store.Renew(ctx, lease); err != nil {
		t.Fatalf("Renew failed: %v", err)
	}
	afterRenew := pttl(t, client, keys.LastStart)
	if afterRenew <= minTime {
		t.Fatalf("last-start TTL after Renew = %v, want longer than MinTime %v", afterRenew, minTime)
	}

	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
	afterRelease := pttl(t, client, keys.LastStart)
	if afterRelease <= minTime {
		t.Fatalf("last-start TTL after Release = %v, want longer than MinTime %v", afterRelease, minTime)
	}

	// The start record must also survive the reclamation path, which purges
	// reservation state for expired leases.
	orphan, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if orphan != nil {
		// A 45s window is still open, so this acquisition must be refused; if it
		// were granted the state below would describe the wrong lease.
		t.Fatal("MinTime did not refuse a second acquisition inside the window")
	}

	// Force the reclaim branch to run with an expired token present.
	stale, err := seedExpiredLease(ctx, client, keys)
	if err != nil {
		t.Fatalf("seeding an expired lease failed: %v", err)
	}
	if _, _, err := store.Acquire(ctx, id, 1, opts); err != nil {
		t.Fatal(err)
	}
	if exists, err := client.HExists(ctx, keys.Leases, stale).Result(); err != nil {
		t.Fatalf("HEXISTS failed: %v", err)
	} else if exists {
		t.Fatal("an expired lease was not reclaimed")
	}
	if got := pttl(t, client, keys.LastStart); got <= minTime {
		t.Fatalf("last-start TTL after reclamation = %v, want longer than MinTime %v", got, minTime)
	}

	// And it must still hold a value: reclamation removes reservations only.
	if err := client.Get(ctx, keys.LastStart).Err(); err != nil {
		t.Fatalf("last-start was deleted by lease reclamation: %v", err)
	}
}

// TestSpacing_RedisStartHistoryStaysBounded checks the spacing record is a
// single value rather than a set that grows with traffic. The previous
// implementation kept a per-token ZSET entry, and released tokens were never
// removed.
func TestSpacing_RedisStartHistoryStaysBounded(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("spacing-bounded")
	// A tiny MinTime keeps the loop fast while still exercising the write path.
	opts := gothrottle.Options{MaxConcurrent: 4, MinTime: time.Microsecond, LeaseTTL: time.Second}
	keys := gothrottle.RedisKeys(id)

	const cycles = 300
	for i := 0; i < cycles; i++ {
		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		if lease == nil {
			continue
		}
		if err := store.Release(ctx, lease); err != nil {
			t.Fatalf("Release %d failed: %v", i, err)
		}
	}

	// One string, one value — not a collection with an entry per job.
	kind, err := client.Type(ctx, keys.LastStart).Result()
	if err != nil {
		t.Fatalf("TYPE failed: %v", err)
	}
	if kind != "string" {
		t.Fatalf("last-start key type = %q, want \"string\" so it cannot grow with traffic", kind)
	}
	if size, err := client.MemoryUsage(ctx, keys.LastStart).Result(); err != nil {
		t.Logf("MEMORY USAGE unavailable: %v", err)
	} else if size > 4096 {
		t.Fatalf("last-start uses %d bytes after %d cycles; it is accumulating history", size, cycles)
	}

	// Reservation state is bounded by MaxConcurrent, not by throughput.
	live, err := client.HLen(ctx, keys.Leases).Result()
	if err != nil {
		t.Fatalf("HLEN failed: %v", err)
	}
	if live > int64(opts.MaxConcurrent) {
		t.Fatalf("lease hash holds %d entries after %d released cycles, want at most MaxConcurrent %d", live, cycles, opts.MaxConcurrent)
	}
	expirations, err := client.ZCard(ctx, keys.Expirations).Result()
	if err != nil {
		t.Fatalf("ZCARD failed: %v", err)
	}
	if expirations > int64(opts.MaxConcurrent) {
		t.Fatalf("expiry set holds %d entries after %d released cycles, want at most MaxConcurrent %d", expirations, cycles, opts.MaxConcurrent)
	}
}

// TestSpacing_RedisReclaimsLargeExpiredSet covers the batching in the reclaim
// loop. A single ZRANGEBYSCORE without a limit, fed to unpack(), risks blowing
// Lua's argument limit once a lot of lease state has accumulated — for example
// after a process died holding many leases.
func TestSpacing_RedisReclaimsLargeExpiredSet(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("spacing-large-reclaim")
	opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}
	keys := gothrottle.RedisKeys(id)

	// Register the configuration first so the seeded state is not rejected as a
	// mismatch, then seed far more expired tokens than one batch holds.
	first, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || first == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", first, err)
	}
	if err := store.Release(ctx, first); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	const orphans = 5000
	pipe := client.Pipeline()
	for i := 0; i < orphans; i++ {
		token := fmt.Sprintf("orphan-%d", i)
		pipe.HSet(ctx, keys.Leases, token, 1)
		pipe.ZAdd(ctx, keys.Expirations, &redis.Z{Score: 1, Member: token})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("seeding expired leases failed: %v", err)
	}

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatalf("Acquire with %d expired leases failed: %v", orphans, err)
	}
	if lease == nil {
		t.Fatalf("%d expired leases blocked admission; they should all have been reclaimed", orphans)
	}
	if remaining, err := client.ZCard(ctx, keys.Expirations).Result(); err != nil {
		t.Fatalf("ZCARD failed: %v", err)
	} else if remaining != 1 {
		t.Fatalf("expiry set holds %d entries, want only the new lease", remaining)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestSpacing_ZeroMinTimeStoresNoHistory checks the spacing record is not
// written at all when there is no window to enforce.
func TestSpacing_ZeroMinTimeStoresNoHistory(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("spacing-zero")
	opts := gothrottle.Options{MaxConcurrent: 2, LeaseTTL: time.Second}
	keys := gothrottle.RedisKeys(id)

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}
	defer func() { _ = store.Release(ctx, lease) }()

	exists, err := client.Exists(ctx, keys.LastStart).Result()
	if err != nil {
		t.Fatalf("EXISTS failed: %v", err)
	}
	if exists != 0 {
		t.Fatal("a last-start record was written with MinTime unset; there is no window to enforce")
	}
}

// TestSpacing_LimiterEnforcesWindowAcrossJobCompletion is the end-to-end
// version: two jobs through one limiter, where the first finishes well inside
// the window. Completing a job must not release the next one early.
func TestSpacing_LimiterEnforcesWindowAcrossJobCompletion(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		id := uniqueLimiterID("spacing-limiter")
		const minTime = 1500 * time.Millisecond

		limiter, err := gothrottle.NewLimiter(gothrottle.Options{
			ID:            id,
			MaxConcurrent: 1,
			MinTime:       minTime,
			LeaseTTL:      time.Second,
			Datastore:     store,
			RetryInterval: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Stop() }()

		starts := make(chan time.Time, 2)
		task := func() (interface{}, error) {
			starts <- time.Now()
			return nil, nil
		}

		if _, err := limiter.Schedule(task); err != nil {
			t.Fatalf("first Schedule failed: %v", err)
		}
		if _, err := limiter.Schedule(task); err != nil {
			t.Fatalf("second Schedule failed: %v", err)
		}

		firstStart := <-starts
		secondStart := <-starts
		// The first job returned immediately, so without independent spacing
		// history the second would start at once.
		if gap := secondStart.Sub(firstStart); gap < minTime-100*time.Millisecond {
			t.Fatalf("jobs started %v apart, want at least %v", gap, minTime)
		}
	})
}

// seedExpiredLease writes a reservation that is already past its expiry, so the
// next acquisition has to run its reclamation path. It returns the token.
func seedExpiredLease(ctx context.Context, client *redis.Client, keys gothrottle.RedisKeyLayout) (string, error) {
	const token = "expired-holder"
	if err := client.HSet(ctx, keys.Leases, token, 1).Err(); err != nil {
		return "", err
	}
	if err := client.ZAdd(ctx, keys.Expirations, &redis.Z{Score: 1, Member: token}).Err(); err != nil {
		return "", err
	}
	return token, nil
}

// TestSpacing_LocalStoreKeepsStartAfterExpiry is the LocalStore counterpart of
// the Redis state assertions: the in-memory purge must remove reservations only.
func TestSpacing_LocalStoreKeepsStartAfterExpiry(t *testing.T) {
	store := gothrottle.NewLocalStore()
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("spacing-local-purge")
	opts := gothrottle.Options{MaxConcurrent: 1, MinTime: 3 * time.Second, LeaseTTL: time.Second}

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}

	// No renewal, no release: the lease lapses and the purge runs on the next
	// acquisition.
	time.Sleep(1200 * time.Millisecond)

	next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatal("purging an expired lease erased the spacing window")
	}
	if retryAfter <= 0 || retryAfter >= 3*time.Second {
		t.Fatalf("retryAfter = %v, want the remainder of a 3s window", retryAfter)
	}

	if err := store.Renew(ctx, lease); !errors.Is(err, gothrottle.ErrLeaseLost) {
		t.Fatalf("Renew of an expired lease = %v, want ErrLeaseLost", err)
	}
}
