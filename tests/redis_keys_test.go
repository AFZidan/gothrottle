// FILENAME: redis_keys_test.go
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

// The lease scripts touch several keys at once, and Redis Cluster only allows
// that when every key resolves to one hash slot. The slot comes from the
// substring inside the first {...}, so all four keys for a limiter share one tag
// derived from the ID — derived, not the ID itself, because an ID containing
// braces would otherwise pick its own tag.

// hashTag returns what Redis Cluster would use to choose a key's slot: the text
// between the first '{' and the next '}' after it, or the whole key when there is
// no such pair.
func hashTag(key string) string {
	open := strings.Index(key, "{")
	if open < 0 {
		return key
	}
	rest := key[open+1:]
	closeIdx := strings.Index(rest, "}")
	if closeIdx <= 0 {
		return key
	}
	return rest[:closeIdx]
}

func TestRedisKeys_SameIDSharesOneHashTag(t *testing.T) {
	for _, id := range []string{
		"orders",
		"payments:eu-west-1",
		"a",
		strings.Repeat("x", 512),
		"unicode-ключ-🔑",
		"spaces and symbols !@#$%^&*()",
	} {
		keys := gothrottle.RedisKeys(id)
		all := []string{keys.Leases, keys.Expirations, keys.LastStart, keys.Config}

		tag := hashTag(all[0])
		if tag == "" {
			t.Fatalf("limiter %q produced key %q with an empty hash tag; Redis would hash the whole key", id, all[0])
		}
		for _, key := range all[1:] {
			if got := hashTag(key); got != tag {
				t.Fatalf("limiter %q: key %q hashes on %q but %q hashes on %q; multi-key Lua would be cross-slot",
					id, all[0], tag, key, got)
			}
		}
		// Distinct keys within the slot, or they would overwrite each other.
		seen := make(map[string]bool, len(all))
		for _, key := range all {
			if seen[key] {
				t.Fatalf("limiter %q reuses key %q for two different purposes", id, key)
			}
			seen[key] = true
		}
	}
}

func TestRedisKeys_DifferentIDsGetDifferentTags(t *testing.T) {
	ids := []string{
		"orders",
		"orders ",
		"Orders",
		"payments",
		"", // the empty ID is rejected before it reaches Redis, but must not alias
		"{orders}",
		"}orders{",
		"a{b}c",
		"{}",
	}

	tags := make(map[string]string, len(ids))
	for _, id := range ids {
		tag := hashTag(gothrottle.RedisKeys(id).Leases)
		if other, clash := tags[tag]; clash {
			t.Fatalf("limiter IDs %q and %q share hash tag %q; they would share capacity", other, id, tag)
		}
		tags[tag] = id
	}
}

// TestRedisKeys_BraceInIDCannotChooseTheTag is the specific hazard: an ID
// containing braces must not be able to steer its own slot or collide with
// another ID's.
func TestRedisKeys_BraceInIDCannotChooseTheTag(t *testing.T) {
	hostile := gothrottle.RedisKeys("{shared}")
	plain := gothrottle.RedisKeys("shared")

	if hashTag(hostile.Leases) == "shared" {
		t.Fatal("an ID's own braces chose the hash tag; a caller could pick another limiter's slot")
	}
	if hashTag(hostile.Leases) == hashTag(plain.Leases) {
		t.Fatal(`limiter "{shared}" collides with limiter "shared"`)
	}

	// No braces may survive into the tag, or the tag itself becomes ambiguous.
	for _, key := range []string{hostile.Leases, hostile.Expirations, hostile.LastStart, hostile.Config} {
		if tag := hashTag(key); strings.ContainsAny(tag, "{}") {
			t.Fatalf("hash tag %q for key %q contains a brace", tag, key)
		}
	}
}

func TestRedisKeys_AreDeterministic(t *testing.T) {
	const id = "deterministic-limiter"

	first := gothrottle.RedisKeys(id)
	for i := 0; i < 5; i++ {
		if got := gothrottle.RedisKeys(id); got != first {
			t.Fatalf("RedisKeys(%q) returned %+v then %+v; instances would not find each other's state", id, first, got)
		}
	}
}

// TestRedisKeys_MatchTheKeysTheStoreWrites ties the exported layout to reality:
// after one acquisition, exactly the advertised keys exist under the package
// prefix.
func TestRedisKeys_MatchTheKeysTheStoreWrites(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("keys-match")
	opts := gothrottle.Options{MaxConcurrent: 1, MinTime: 5 * time.Second, LeaseTTL: 2 * time.Second}
	keys := gothrottle.RedisKeys(id)

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}
	defer func() { _ = store.Release(ctx, lease) }()

	pattern := "gothrottle:{" + hashTag(keys.Leases) + "}:*"
	found, err := client.Keys(ctx, pattern).Result()
	if err != nil {
		t.Fatalf("KEYS failed: %v", err)
	}

	want := map[string]bool{
		keys.Leases:      false,
		keys.Expirations: false,
		keys.LastStart:   false,
		keys.Config:      false,
	}
	for _, key := range found {
		if _, expected := want[key]; !expected {
			t.Fatalf("store wrote undocumented key %q", key)
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("documented key %q was not created", key)
		}
	}
}

// TestRedisStore_AcceptsUniversalClients covers the constructor widening. A Ring
// is a UniversalClient that is not *redis.Client, so it proves the store is no
// longer tied to one client type, and it round-trips the scripts through a real
// server.
func TestRedisStore_AcceptsUniversalClients(t *testing.T) {
	addr := redisTestAddr(t)

	ring := redis.NewRing(&redis.RingOptions{Addrs: map[string]string{"one": addr}})
	t.Cleanup(func() { _ = ring.Close() })

	ctx := context.Background()
	if err := ring.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis ring is unreachable: %v", err)
	}

	store, err := gothrottle.NewRedisStore(ring)
	if err != nil {
		t.Fatalf("NewRedisStore(*redis.Ring) failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("universal-client")
	opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: 2 * time.Second}

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire through a Ring = (%v, %v), want a lease", lease, err)
	}
	if err := store.Renew(ctx, lease); err != nil {
		t.Fatalf("Renew through a Ring failed: %v", err)
	}

	blocked, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatal("MaxConcurrent 1 was exceeded through a Ring client")
	}

	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release through a Ring failed: %v", err)
	}

	// The legacy path has to work through the same client.
	if canRun, _, err := store.Request(id+"-legacy", 1, opts); err != nil || !canRun {
		t.Fatalf("Request through a Ring = (%v, %v), want (true, nil)", canRun, err)
	}
	if err := store.RegisterDone(id+"-legacy", 1); err != nil {
		t.Fatalf("RegisterDone through a Ring failed: %v", err)
	}
}

// TestRedisStore_RejectsTypedNilClient covers the interface hazard: a nil
// *redis.Client stored in a non-nil interface. Without an explicit check the
// store would accept it and panic on first use.
func TestRedisStore_RejectsTypedNilClient(t *testing.T) {
	cases := map[string]redis.UniversalClient{
		"nil interface":      nil,
		"nil *redis.Client":  (*redis.Client)(nil),
		"nil *redis.Ring":    (*redis.Ring)(nil),
		"nil *ClusterClient": (*redis.ClusterClient)(nil),
	}

	for name, client := range cases {
		if _, err := gothrottle.NewRedisStore(client); !errors.Is(err, gothrottle.ErrNilClient) {
			t.Fatalf("NewRedisStore(%s) = %v, want ErrNilClient", name, err)
		}
	}
}

// TestRedisStore_ReconcilesOrphanedLeaseWeight covers state this package did not
// write: a lease hash field with no expiry entry, as a partial restore or manual
// surgery could leave. Summing the hash blindly would let it hold capacity
// forever, since nothing would ever expire it.
func TestRedisStore_ReconcilesOrphanedLeaseWeight(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("orphan-weight")
	opts := gothrottle.Options{MaxConcurrent: 1, LeaseTTL: 5 * time.Second}
	keys := gothrottle.RedisKeys(id)

	// Register the configuration so the seeded state is not rejected as a
	// mismatch.
	warmup, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || warmup == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", warmup, err)
	}
	if err := store.Release(ctx, warmup); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// A weight with no expiry entry: unreclaimable by the normal path.
	if err := client.HSet(ctx, keys.Leases, "ghost", 1).Err(); err != nil {
		t.Fatalf("HSET failed: %v", err)
	}

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("an untracked lease weight permanently consumed MaxConcurrent 1")
	}
	if exists, err := client.HExists(ctx, keys.Leases, "ghost").Result(); err != nil {
		t.Fatalf("HEXISTS failed: %v", err)
	} else if exists {
		t.Fatal("the untracked weight was counted rather than reconciled away")
	}

	// The limit still holds for the lease that legitimately exists.
	blocked, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatal("reconciliation freed capacity that a live lease holds")
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}

// TestRedisStore_ReloadsMissingLeaseScript covers NOSCRIPT on the lease path,
// which now falls back to EVAL — the form that also caches on the node serving
// the key, rather than whichever node a SCRIPT LOAD happened to reach.
func TestRedisStore_ReloadsMissingLeaseScript(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("lease-script-reload")
	opts := gothrottle.Options{MaxConcurrent: 1, MinTime: 10 * time.Millisecond, LeaseTTL: 2 * time.Second}

	lease, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil || lease == nil {
		t.Fatalf("Acquire = (%v, %v), want a lease", lease, err)
	}

	if err := client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("ScriptFlush failed: %v", err)
	}
	if err := store.Renew(ctx, lease); err != nil {
		t.Fatalf("Renew after ScriptFlush failed: %v", err)
	}

	if err := client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("ScriptFlush failed: %v", err)
	}
	if err := store.Release(ctx, lease); err != nil {
		t.Fatalf("Release after ScriptFlush failed: %v", err)
	}

	if err := client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("ScriptFlush failed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	next, _, err := store.Acquire(ctx, id, 1, opts)
	if err != nil {
		t.Fatalf("Acquire after ScriptFlush failed: %v", err)
	}
	if next == nil {
		t.Fatal("Acquire after ScriptFlush was refused")
	}
	if err := store.Release(ctx, next); err != nil {
		t.Fatalf("Release failed: %v", err)
	}
}
