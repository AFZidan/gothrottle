// FILENAME: lease_test.go
package gothrottle_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// leaseStores runs a test body against every LeaseDatastore implementation, so
// local and distributed mode are held to the same contract.
func leaseStores(t *testing.T, body func(t *testing.T, store gothrottle.LeaseDatastore)) {
	t.Helper()

	t.Run("local", func(t *testing.T) {
		store := gothrottle.NewLocalStore()
		defer func() { _ = store.Disconnect() }()
		body(t, store)
	})

	t.Run("redis", func(t *testing.T) {
		client := newTestRedisClient(t)
		store, err := gothrottle.NewRedisStore(client)
		if err != nil {
			t.Fatalf("NewRedisStore failed: %v", err)
		}
		defer func() { _ = store.Disconnect() }()
		body(t, store)
	})
}

func TestLease_AcquireEnforcesMaxConcurrent(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-max")
		opts := gothrottle.Options{MaxConcurrent: 2}

		first, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || first == nil {
			t.Fatalf("first Acquire = (%v, %v), want a lease", first, err)
		}
		second, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || second == nil {
			t.Fatalf("second Acquire = (%v, %v), want a lease", second, err)
		}
		if first.Token == second.Token {
			t.Fatal("two leases share a token; releases would collide")
		}

		third, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if third != nil {
			t.Fatal("third Acquire should be refused at MaxConcurrent 2")
		}

		if err := store.Release(ctx, first); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		fourth, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || fourth == nil {
			t.Fatalf("Acquire after Release = (%v, %v), want a lease", fourth, err)
		}
	})
}

func TestLease_ExpiredLeaseIsReclaimed(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-expiry")
		// The 1s floor keeps the renewal interval sane; this is the shortest
		// window a test can wait out.
		opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		// Simulate the holder dying: no renewal, no release.
		blocked, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if blocked != nil {
			t.Fatal("capacity should still be held before the lease expires")
		}

		time.Sleep(1200 * time.Millisecond)

		reclaimed, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if reclaimed == nil {
			t.Fatal("capacity was not reclaimed after the lease expired; a crashed holder would block the limiter forever")
		}
	})
}

func TestLease_RenewKeepsCapacityBeyondTTL(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-renew")
		opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		// Renew across more than two TTLs. Under the old counter model the key
		// simply expired and a second job could start over the limit.
		deadline := time.Now().Add(2500 * time.Millisecond)
		for time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			if err := store.Renew(ctx, lease); err != nil {
				t.Fatalf("Renew failed: %v", err)
			}

			other, _, err := store.Acquire(ctx, id, 1, opts)
			if err != nil {
				t.Fatal(err)
			}
			if other != nil {
				t.Fatal("capacity was handed out while a renewed lease still held it")
			}
		}
	})
}

func TestLease_RenewAfterExpiryReportsLost(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-renew-lost")
		opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		time.Sleep(1200 * time.Millisecond)

		// Force the store to purge the expired lease.
		successor, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if successor == nil {
			t.Fatal("expired lease was not reclaimed")
		}

		if err := store.Renew(ctx, lease); !errors.Is(err, gothrottle.ErrLeaseLost) {
			t.Fatalf("Renew of an expired lease = %v, want ErrLeaseLost", err)
		}
	})
}

func TestLease_StaleReleaseDoesNotAffectNewLease(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-stale-release")
		opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		stale, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || stale == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", stale, err)
		}

		time.Sleep(1200 * time.Millisecond)

		successor, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || successor == nil {
			t.Fatalf("Acquire after expiry = (%v, %v), want a lease", successor, err)
		}

		// This is the corruption the counter model allowed: the long-running
		// job's late completion decremented the successor's reservation to zero
		// and let a third job start. Releasing by token must be a no-op here.
		if err := store.Release(ctx, stale); err != nil {
			t.Fatalf("stale Release returned an error: %v", err)
		}

		third, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if third != nil {
			t.Fatal("a stale release freed capacity still held by a newer lease")
		}
	})
}

func TestLease_ReleaseIsIdempotent(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-idempotent")
		opts := gothrottle.Options{MaxConcurrent: 1}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		for i := 0; i < 3; i++ {
			if err := store.Release(ctx, lease); err != nil {
				t.Fatalf("Release attempt %d failed: %v", i+1, err)
			}
		}

		// Repeated releases must not free more capacity than the one lease held.
		first, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || first == nil {
			t.Fatalf("Acquire after releases = (%v, %v), want a lease", first, err)
		}
		second, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if second != nil {
			t.Fatal("repeated releases inflated available capacity")
		}
	})
}

func TestLease_MinTimeSurvivesRelease(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-mintime")
		opts := gothrottle.Options{MinTime: 300 * time.Millisecond}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}

		// Spacing is measured from when the previous job started, so finishing
		// it must not reset the window.
		if err := store.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil {
			t.Fatal("MinTime was reset by releasing the previous lease")
		}
		if retryAfter <= 0 || retryAfter > 300*time.Millisecond {
			t.Fatalf("retryAfter = %v, want in (0, 300ms]", retryAfter)
		}

		time.Sleep(retryAfter + 50*time.Millisecond)

		next, _, err = store.Acquire(ctx, id, 1, opts)
		if err != nil || next == nil {
			t.Fatalf("Acquire after the MinTime window = (%v, %v), want a lease", next, err)
		}
	})
}

func TestLease_LongMinTimeIsEnforced(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-long-mintime")
		// A MinTime longer than the lease TTL used to be bypassed once the
		// state key expired.
		opts := gothrottle.Options{MinTime: 45 * time.Second, LeaseTTL: time.Second}

		lease, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
		}
		if err := store.Release(ctx, lease); err != nil {
			t.Fatalf("Release failed: %v", err)
		}

		// Wait past the lease TTL: the spacing window must outlive it.
		time.Sleep(1200 * time.Millisecond)

		next, retryAfter, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if next != nil {
			t.Fatal("a MinTime longer than the lease TTL was bypassed")
		}
		if retryAfter <= 30*time.Second {
			t.Fatalf("retryAfter = %v, want more than 30s", retryAfter)
		}
	})
}

func TestLease_WeightIsRespected(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-weight")
		opts := gothrottle.Options{MaxConcurrent: 5}

		heavy, _, err := store.Acquire(ctx, id, 3, opts)
		if err != nil || heavy == nil {
			t.Fatalf("Acquire(weight 3) = (%v, %v), want a lease", heavy, err)
		}

		fits, _, err := store.Acquire(ctx, id, 2, opts)
		if err != nil || fits == nil {
			t.Fatalf("Acquire(weight 2) = (%v, %v), want a lease", fits, err)
		}

		overflow, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		if overflow != nil {
			t.Fatal("weights 3+2 filled MaxConcurrent 5, but another lease was granted")
		}

		if _, _, err := store.Acquire(ctx, id, 6, opts); !errors.Is(err, gothrottle.ErrWeightExceedsMax) {
			t.Fatalf("Acquire(weight > max) = %v, want ErrWeightExceedsMax", err)
		}
	})
}

func TestLease_ConcurrentAcquireNeverExceedsLimit(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("lease-concurrent")
		const maxConcurrent = 4
		opts := gothrottle.Options{MaxConcurrent: maxConcurrent}

		var mu sync.Mutex
		var held []*gothrottle.Lease
		var wg sync.WaitGroup

		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease, _, err := store.Acquire(ctx, id, 1, opts)
				if err != nil {
					t.Errorf("Acquire failed: %v", err)
					return
				}
				if lease == nil {
					return
				}
				mu.Lock()
				held = append(held, lease)
				mu.Unlock()
			}()
		}
		wg.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(held) > maxConcurrent {
			t.Fatalf("%d leases granted concurrently, want at most %d", len(held), maxConcurrent)
		}
		for _, lease := range held {
			if err := store.Release(ctx, lease); err != nil {
				t.Fatalf("Release failed: %v", err)
			}
		}
	})
}

func TestLease_NilLeaseRejected(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		if err := store.Renew(ctx, nil); !errors.Is(err, gothrottle.ErrNilLease) {
			t.Fatalf("Renew(nil) = %v, want ErrNilLease", err)
		}
		if err := store.Release(ctx, nil); !errors.Is(err, gothrottle.ErrNilLease) {
			t.Fatalf("Release(nil) = %v, want ErrNilLease", err)
		}
	})
}

// TestLimiter_LongJobHoldsCapacityViaRenewal is the end-to-end version of the
// report's core scenario: with MaxConcurrent 1 and a job that runs longer than
// the lease TTL, no second job may start.
func TestLimiter_LongJobHoldsCapacityViaRenewal(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		id := uniqueLimiterID("long-job")

		var overlaps int32
		var running int32

		limiter, err := gothrottle.NewLimiter(gothrottle.Options{
			ID:            id,
			MaxConcurrent: 1,
			Datastore:     store,
			LeaseTTL:      time.Second,
			RetryInterval: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Stop() }()

		task := func() (interface{}, error) {
			if atomic.AddInt32(&running, 1) > 1 {
				atomic.AddInt32(&overlaps, 1)
			}
			// Outlive the lease TTL by more than 2x.
			time.Sleep(2500 * time.Millisecond)
			atomic.AddInt32(&running, -1)
			return nil, nil
		}

		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := limiter.Schedule(task); err != nil {
					t.Errorf("Schedule failed: %v", err)
				}
			}()
		}
		wg.Wait()

		if got := atomic.LoadInt32(&overlaps); got != 0 {
			t.Fatalf("jobs overlapped %d times with MaxConcurrent 1; a long job lost its capacity", got)
		}
	})
}

// TestLimiter_CrashedHolderCapacityIsReclaimed simulates a process dying with a
// lease held: a fresh limiter must be able to acquire once the lease expires.
func TestLimiter_CrashedHolderCapacityIsReclaimed(t *testing.T) {
	leaseStores(t, func(t *testing.T, store gothrottle.LeaseDatastore) {
		ctx := context.Background()
		id := uniqueLimiterID("crashed-holder")
		opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: time.Second}

		// Acquire directly and never release: the holder is gone.
		orphan, _, err := store.Acquire(ctx, id, 1, opts)
		if err != nil || orphan == nil {
			t.Fatalf("Acquire = (%v, %v), want a lease", orphan, err)
		}

		limiter, err := gothrottle.NewLimiter(gothrottle.Options{
			ID:            id,
			MaxConcurrent: 1,
			Datastore:     store,
			LeaseTTL:      time.Second,
			RetryInterval: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = limiter.Stop() }()

		done := make(chan error, 1)
		go func() {
			_, err := limiter.Schedule(func() (interface{}, error) { return "ran", nil })
			done <- err
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("job failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("capacity from a crashed holder was never reclaimed")
		}
	})
}
