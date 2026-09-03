// FILENAME: redis_orphan_test.go
package gothrottle_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
	"github.com/go-redis/redis/v8"
)

// Orphan reconciliation
//
// Two Redis keys describe one reservation: a weight in the lease hash and an
// expiry score in the ZSET. This package always writes both together, so a
// mismatch is state it did not produce — a partial restore, a manual HDEL, an
// AOF truncated between the two commands. The acquire script has to survive that
// state, because a weight the expiry path can never reclaim would hold capacity
// until somebody noticed by hand.
//
// The original check compared cardinalities only, which misses the case where
// both collections hold one member and the members differ. These tests cover
// each shape of corruption and then confirm a legitimate lease is untouched.

// orphanFixture is a limiter whose configuration is already registered, so
// seeded state is not rejected as a configuration mismatch, plus the keys and a
// cleanup.
type orphanFixture struct {
	store  *gothrottle.RedisStore
	client *redis.Client
	ctx    context.Context
	id     string
	keys   gothrottle.RedisKeyLayout
	opts   gothrottle.Options
}

func newOrphanFixture(t *testing.T, name string, opts gothrottle.Options) orphanFixture {
	t.Helper()

	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Disconnect() })

	ctx := context.Background()
	id := uniqueLimiterID(name)
	keys := gothrottle.RedisKeys(id)
	t.Cleanup(func() {
		_ = client.Del(context.Background(), keys.Leases, keys.Expirations, keys.LastStart, keys.Config).Err()
	})

	// Register the configuration and release immediately, so the seeded state
	// below is evaluated rather than refused as a mismatch.
	warmup, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || warmup == nil {
		t.Fatalf("warmup Acquire = (%v, %v), want a lease", warmup, err)
	}
	if err := store.Release(ctx, warmup); err != nil {
		t.Fatalf("warmup Release failed: %v", err)
	}

	return orphanFixture{store: store, client: client, ctx: ctx, id: id, keys: keys, opts: opts}
}

// seedHashOrphan writes a lease weight with no expiry entry.
func (f orphanFixture) seedHashOrphan(t *testing.T, token string, weight int) {
	t.Helper()

	if err := f.client.HSet(f.ctx, f.keys.Leases, token, weight).Err(); err != nil {
		t.Fatalf("HSET %s %s failed: %v", f.keys.Leases, token, err)
	}
}

// seedZSetOrphan writes a live expiry entry with no lease weight.
func (f orphanFixture) seedZSetOrphan(t *testing.T, token string) {
	t.Helper()

	if err := f.client.ZAdd(f.ctx, f.keys.Expirations, &redis.Z{
		Score:  f.futureScore(t),
		Member: token,
	}).Err(); err != nil {
		t.Fatalf("ZADD %s %s failed: %v", f.keys.Expirations, token, err)
	}
}

// futureScore is an expiry far enough ahead that the expired-lease reclaim loop
// leaves it alone: these tests are about membership reconciliation, not expiry.
func (f orphanFixture) futureScore(t *testing.T) float64 {
	t.Helper()

	now, err := f.client.Time(f.ctx).Result()
	if err != nil {
		t.Fatalf("TIME failed: %v", err)
	}
	return float64(now.Add(time.Hour).UnixNano() / int64(time.Microsecond))
}

func (f orphanFixture) hlen(t *testing.T) int64 {
	t.Helper()

	n, err := f.client.HLen(f.ctx, f.keys.Leases).Result()
	if err != nil {
		t.Fatalf("HLEN failed: %v", err)
	}
	return n
}

func (f orphanFixture) zcard(t *testing.T) int64 {
	t.Helper()

	n, err := f.client.ZCard(f.ctx, f.keys.Expirations).Result()
	if err != nil {
		t.Fatalf("ZCARD failed: %v", err)
	}
	return n
}

func (f orphanFixture) hasLeaseField(t *testing.T, token string) bool {
	t.Helper()

	exists, err := f.client.HExists(f.ctx, f.keys.Leases, token).Result()
	if err != nil {
		t.Fatalf("HEXISTS failed: %v", err)
	}
	return exists
}

func (f orphanFixture) hasExpiryEntry(t *testing.T, token string) bool {
	t.Helper()

	_, err := f.client.ZScore(f.ctx, f.keys.Expirations, token).Result()
	switch err {
	case nil:
		return true
	case redis.Nil:
		return false
	default:
		t.Fatalf("ZSCORE failed: %v", err)
		return false
	}
}

// TestRedisOrphan_EqualCardinalityDifferentTokens is the defect the cardinality
// check missed. The hash holds token A, the ZSET holds token B; HLEN and ZCARD
// are both 1, so nothing was reconciled and token A's weight consumed the whole
// limit with no expiry entry that could ever release it.
func TestRedisOrphan_EqualCardinalityDifferentTokens(t *testing.T) {
	f := newOrphanFixture(t, "orphan-equal-card", gothrottle.Options{MaxConcurrent: 1, LeaseTTL: 30 * time.Second})

	f.seedHashOrphan(t, "token-a", 1)
	f.seedZSetOrphan(t, "token-b")

	if f.hlen(t) != f.zcard(t) {
		t.Fatalf("fixture is not the equal-cardinality case: HLEN %d, ZCARD %d", f.hlen(t), f.zcard(t))
	}

	lease, retryAfter, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatalf("equal-cardinality orphan state consumed MaxConcurrent 1 permanently (retryAfter %v)", retryAfter)
	}

	if f.hasLeaseField(t, "token-a") {
		t.Fatal("the hash orphan was counted rather than reconciled away")
	}
	if f.hasExpiryEntry(t, "token-b") {
		t.Fatal("the ZSET orphan survived; it would accumulate without bound")
	}
	// Exactly the new lease remains, in both collections.
	if got := f.hlen(t); got != 1 {
		t.Fatalf("lease hash holds %d fields, want only the new lease", got)
	}
	if got := f.zcard(t); got != 1 {
		t.Fatalf("expiry set holds %d entries, want only the new lease", got)
	}

	if err := f.store.Release(f.ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestRedisOrphan_HashOnly covers a weight with no expiry record — the shape the
// cardinality check did catch. It is kept because the reconciliation was rewritten
// around ZSET membership, so this path is now reached differently.
func TestRedisOrphan_HashOnly(t *testing.T) {
	f := newOrphanFixture(t, "orphan-hash-only", gothrottle.Options{MaxConcurrent: 2, LeaseTTL: 30 * time.Second})

	f.seedHashOrphan(t, "ghost", 2)

	lease, _, err := f.store.Acquire(f.ctx, f.id, 2, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("an untracked lease weight permanently consumed MaxConcurrent 2")
	}
	if f.hasLeaseField(t, "ghost") {
		t.Fatal("the untracked weight was counted rather than reconciled away")
	}
	if err := f.store.Release(f.ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestRedisOrphan_ZSetOnly covers an expiry entry with no weight. It never held
// capacity, but left in place it would accumulate forever, and the surviving
// mismatch would make every later acquisition re-scan the hash.
func TestRedisOrphan_ZSetOnly(t *testing.T) {
	f := newOrphanFixture(t, "orphan-zset-only", gothrottle.Options{MaxConcurrent: 1, LeaseTTL: 30 * time.Second})

	f.seedZSetOrphan(t, "weightless")

	lease, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("a weightless expiry entry blocked admission")
	}
	if f.hasExpiryEntry(t, "weightless") {
		t.Fatal("the weightless expiry entry was not reclaimed")
	}
	if got := f.zcard(t); got != 1 {
		t.Fatalf("expiry set holds %d entries, want only the new lease", got)
	}
	if err := f.store.Release(f.ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestRedisOrphan_MixedValidAndOrphaned is the realistic case: a live lease
// alongside both kinds of orphan. The live lease must keep its capacity while
// every orphan is cleared.
func TestRedisOrphan_MixedValidAndOrphaned(t *testing.T) {
	f := newOrphanFixture(t, "orphan-mixed", gothrottle.Options{MaxConcurrent: 3, LeaseTTL: 30 * time.Second})

	live, _, err := f.store.Acquire(f.ctx, f.id, 2, f.opts)
	if err != nil || live == nil {
		t.Fatalf("Acquire of the live lease = (%v, %v), want a lease", live, err)
	}

	f.seedHashOrphan(t, "hash-orphan-1", 1)
	f.seedHashOrphan(t, "hash-orphan-2", 3)
	f.seedZSetOrphan(t, "zset-orphan-1")
	f.seedZSetOrphan(t, "zset-orphan-2")

	// The live lease holds 2 of 3, so exactly 1 must remain available. If the
	// orphans were counted, nothing would fit; if the live lease were reconciled
	// away, a weight-3 job would fit, which it must not.
	granted, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if granted == nil {
		t.Fatal("orphans consumed the capacity that was actually free")
	}

	overCapacity, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if overCapacity != nil {
		t.Fatal("reconciliation freed capacity that live leases hold")
	}

	for _, token := range []string{"hash-orphan-1", "hash-orphan-2"} {
		if f.hasLeaseField(t, token) {
			t.Fatalf("hash orphan %s survived reconciliation", token)
		}
	}
	for _, token := range []string{"zset-orphan-1", "zset-orphan-2"} {
		if f.hasExpiryEntry(t, token) {
			t.Fatalf("ZSET orphan %s survived reconciliation", token)
		}
	}

	// The live lease is still renewable, which is the strongest statement that
	// reconciliation left it entirely alone.
	if err := f.store.Renew(f.ctx, live); err != nil {
		t.Fatalf("Renew of the live lease failed after reconciliation: %v", err)
	}

	if err := f.store.Release(f.ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Release(f.ctx, granted); err != nil {
		t.Fatal(err)
	}
}

// TestRedisOrphan_ValidLeaseSurvivesAndStillBlocks states the safety half on its
// own: reconciliation must never mistake a live lease for corruption.
func TestRedisOrphan_ValidLeaseSurvivesAndStillBlocks(t *testing.T) {
	f := newOrphanFixture(t, "orphan-valid-survives", gothrottle.Options{MaxConcurrent: 1, LeaseTTL: 30 * time.Second})

	live, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil || live == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", live, err)
	}

	// Trigger the reconciliation path without disturbing the live lease.
	f.seedZSetOrphan(t, "decoy")

	blocked, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatal("reconciliation released a live lease's capacity")
	}
	if !f.hasLeaseField(t, live.Token) {
		t.Fatal("the live lease's weight was reconciled away")
	}
	if !f.hasExpiryEntry(t, live.Token) {
		t.Fatal("the live lease's expiry entry was removed")
	}
	if err := f.store.Renew(f.ctx, live); err != nil {
		t.Fatalf("the live lease is no longer renewable: %v", err)
	}

	if err := f.store.Release(f.ctx, live); err != nil {
		t.Fatal(err)
	}
}

// TestRedisOrphan_SpacingRecordIsNeverTouched pins the invariant orphan repair
// must respect: MinTime is measured from a job's start, so the last-start record
// is not reservation state and reconciliation has no business clearing it.
func TestRedisOrphan_SpacingRecordIsNeverTouched(t *testing.T) {
	f := newOrphanFixture(t, "orphan-spacing", gothrottle.Options{
		MaxConcurrent: 2,
		MinTime:       400 * time.Millisecond,
		LeaseTTL:      30 * time.Second,
	})

	before, err := f.client.Get(f.ctx, f.keys.LastStart).Result()
	if err != nil {
		t.Fatalf("GET %s failed: %v", f.keys.LastStart, err)
	}

	f.seedHashOrphan(t, "ghost", 1)
	f.seedZSetOrphan(t, "phantom")

	// Refused on spacing, which proves the window is still in force: the warmup
	// acquisition set it moments ago.
	lease, retryAfter, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if lease != nil {
		t.Fatal("the MinTime window was bypassed while orphans were reconciled")
	}
	if retryAfter <= 0 || retryAfter > f.opts.MinTime {
		t.Fatalf("retryAfter = %v, want within (0, %v]", retryAfter, f.opts.MinTime)
	}

	after, err := f.client.Get(f.ctx, f.keys.LastStart).Result()
	if err != nil {
		t.Fatalf("GET %s failed: %v", f.keys.LastStart, err)
	}
	if after != before {
		t.Fatalf("last-start changed from %s to %s; a refused acquisition must not move the spacing window", before, after)
	}
}

// TestRedisOrphan_UnlimitedConfigurationIsReconciled covers MaxConcurrent 0,
// where the documented guarantee is deliberately narrower.
//
// With no limit there is no capacity for an orphan to hold, so orphan state costs
// memory rather than correctness — and the lease hash is not bounded by
// MaxConcurrent either, so walking it on every admission would be an unbounded
// cost paid by the configuration that needs it least. The script therefore
// reconciles an unlimited limiter only when the O(1) cardinality check says the
// collections disagree, which covers every orphan this package could produce.
func TestRedisOrphan_UnlimitedConfigurationIsReconciled(t *testing.T) {
	f := newOrphanFixture(t, "orphan-unlimited", gothrottle.Options{LeaseTTL: 30 * time.Second})

	f.seedHashOrphan(t, "ghost", 4)

	lease, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("an unlimited limiter refused admission")
	}
	if f.hasLeaseField(t, "ghost") {
		t.Fatal("a hash orphan accumulated under an unlimited configuration")
	}
	if err := f.store.Release(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
}

// TestRedisOrphan_UnlimitedEqualCardinalityHoldsNoCapacity states the limit of
// that narrower guarantee, so the behavior is pinned rather than assumed.
//
// Equal-cardinality corruption under an unlimited configuration is not repaired
// on the acquisition path. It cannot block anything — there is no limit to
// consume — and the records cannot persist indefinitely either: the keys carry a
// TTL of twice the lease window, so one idle window removes them wholesale.
func TestRedisOrphan_UnlimitedEqualCardinalityHoldsNoCapacity(t *testing.T) {
	f := newOrphanFixture(t, "orphan-unlimited-equal", gothrottle.Options{LeaseTTL: 30 * time.Second})

	f.seedHashOrphan(t, "token-a", 4)
	f.seedZSetOrphan(t, "token-b")
	if f.hlen(t) != f.zcard(t) {
		t.Fatalf("fixture is not the equal-cardinality case: HLEN %d, ZCARD %d", f.hlen(t), f.zcard(t))
	}

	// Admission is unaffected, which is the whole correctness claim here.
	for i := 0; i < 3; i++ {
		lease, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i, err)
		}
		if lease == nil {
			t.Fatalf("Acquire %d was refused by an unlimited limiter", i)
		}
		if err := f.store.Release(f.ctx, lease); err != nil {
			t.Fatal(err)
		}
	}

	// And the state is bounded by key expiry rather than left to grow forever.
	for _, key := range []string{f.keys.Leases, f.keys.Expirations} {
		if got := pttl(t, f.client, key); got > 2*f.opts.LeaseTTL+time.Second {
			t.Fatalf("PTTL(%s) = %v, want at most twice the lease window", key, got)
		}
	}
}

// TestRedisOrphan_LargeCorruptedCollections is the bounded-repair requirement.
// Reconciling by feeding whole collections to unpack() fails once they are large
// enough to exceed Lua's argument limit — reachable after a bad restore — so both
// the membership walk and the deletions run in batches.
func TestRedisOrphan_LargeCorruptedCollections(t *testing.T) {
	f := newOrphanFixture(t, "orphan-large", gothrottle.Options{MaxConcurrent: 4, LeaseTTL: 30 * time.Second})

	// Well past Lua's practical unpack() limit (~8000 arguments) in both
	// collections, with disjoint token sets so the cardinalities match while
	// nothing corresponds.
	const orphans = 12000
	future := f.futureScore(t)

	pipe := f.client.Pipeline()
	for i := 0; i < orphans; i++ {
		pipe.HSet(f.ctx, f.keys.Leases, fmt.Sprintf("hash-%d", i), 1)
		pipe.ZAdd(f.ctx, f.keys.Expirations, &redis.Z{Score: future, Member: fmt.Sprintf("zset-%d", i)})
	}
	if _, err := pipe.Exec(f.ctx); err != nil {
		t.Fatalf("seeding corrupted collections failed: %v", err)
	}
	if f.hlen(t) != f.zcard(t) {
		t.Fatalf("fixture is not the equal-cardinality case: HLEN %d, ZCARD %d", f.hlen(t), f.zcard(t))
	}

	lease, _, err := f.store.Acquire(f.ctx, f.id, 4, f.opts)
	if err != nil {
		t.Fatalf("Acquire with %d orphans of each kind failed: %v", orphans, err)
	}
	if lease == nil {
		t.Fatalf("%d orphans of each kind blocked admission", orphans)
	}
	if got := f.hlen(t); got != 1 {
		t.Fatalf("lease hash holds %d fields, want only the new lease", got)
	}
	if got := f.zcard(t); got != 1 {
		t.Fatalf("expiry set holds %d entries, want only the new lease", got)
	}
	if err := f.store.Release(f.ctx, lease); err != nil {
		t.Fatal(err)
	}
}

// TestRedisOrphan_CapacityBecomesAvailableAfterCleanup states the outcome in the
// terms an operator cares about: a limiter wedged by orphan state starts admitting
// work again on the next acquisition, with no manual intervention.
func TestRedisOrphan_CapacityBecomesAvailableAfterCleanup(t *testing.T) {
	f := newOrphanFixture(t, "orphan-recovers", gothrottle.Options{MaxConcurrent: 2, LeaseTTL: 30 * time.Second})

	// Both units of capacity held by orphaned weights, one of which also has a
	// mismatched expiry entry.
	f.seedHashOrphan(t, "stuck-a", 1)
	f.seedHashOrphan(t, "stuck-b", 1)
	f.seedZSetOrphan(t, "unrelated")

	first, _, err := f.store.Acquire(f.ctx, f.id, 2, f.opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if first == nil {
		t.Fatal("orphaned weights kept the limiter wedged")
	}

	// And the recovered limiter enforces its limit normally from then on.
	if blocked, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts); err != nil {
		t.Fatal(err)
	} else if blocked != nil {
		t.Fatal("MaxConcurrent 2 was exceeded after orphan cleanup")
	}

	if err := f.store.Release(f.ctx, first); err != nil {
		t.Fatal(err)
	}
	second, _, err := f.store.Acquire(f.ctx, f.id, 2, f.opts)
	if err != nil || second == nil {
		t.Fatalf("Acquire after Release = (%v, %v), want a lease", second, err)
	}
	if err := f.store.Release(f.ctx, second); err != nil {
		t.Fatal(err)
	}
}

// TestRedisOrphan_HealthyStateNeedsNoHashScan is the performance claim. On a
// healthy limiter the ZSET is authoritative and its members are read once; the
// hash is only scanned when the counts disagree, which is precisely when it holds
// an orphan. Command counts come from Redis's own INFO commandstats.
func TestRedisOrphan_HealthyStateNeedsNoHashScan(t *testing.T) {
	f := newOrphanFixture(t, "orphan-no-scan", gothrottle.Options{MaxConcurrent: 4, LeaseTTL: 30 * time.Second})

	before := hkeysCalls(t, f.client)

	var leases []*gothrottle.Lease
	for i := 0; i < 4; i++ {
		lease, _, err := f.store.Acquire(f.ctx, f.id, 1, f.opts)
		if err != nil || lease == nil {
			t.Fatalf("Acquire %d = (%v, %v), want a lease", i, lease, err)
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		if err := f.store.Release(f.ctx, lease); err != nil {
			t.Fatal(err)
		}
	}

	if after := hkeysCalls(t, f.client); after != before {
		t.Fatalf("HKEYS calls went from %d to %d; a healthy limiter must not scan the lease hash", before, after)
	}
}

// hkeysCalls reads how many HKEYS commands this Redis has served. HKEYS is the
// membership walk reconciliation needs and nothing else in the package uses, so
// its count is a direct measure of whether the repair path ran.
//
// Redis is shared between tests, so the value is only meaningful as a difference
// measured across an interval.
func hkeysCalls(t *testing.T, client *redis.Client) int64 {
	t.Helper()

	stats, err := client.Info(context.Background(), "commandstats").Result()
	if err != nil {
		t.Fatalf("INFO commandstats failed: %v", err)
	}
	for _, line := range splitLines(stats) {
		const prefix = "cmdstat_hkeys:calls="
		if !hasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		calls := int64(0)
		for i := 0; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				break
			}
			calls = calls*10 + int64(rest[i]-'0')
		}
		return calls
	}
	// HKEYS has never been called on this server.
	return 0
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
