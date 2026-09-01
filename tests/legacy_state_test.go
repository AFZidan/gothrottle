// FILENAME: legacy_state_test.go
package gothrottle_test

import (
	"context"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"
)

// The legacy Request/RegisterDone path keeps a single expiring counter, which
// cannot tell a slow holder from a dead one — that is why leases exist. What it
// can do is stop making things worse: Request sizes the state TTL to cover the
// spacing window, and RegisterDone must not undo that. It used to reset the TTL
// to its own 30s fallback, so a 40s MinTime lost its spacing the moment a job
// finished.

// TestLegacyState_RegisterDoneKeepsLongTTL is the direct assertion, made on the
// key's PTTL so it does not have to outwait a 40-second window.
func TestLegacyState_RegisterDoneKeepsLongTTL(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("legacy-ttl")
	const minTime = 40 * time.Second
	opts := gothrottle.Options{MinTime: minTime}
	key := gothrottle.RedisStateKey(id)

	if canRun, _, err := store.Request(id, 1, opts); err != nil || !canRun {
		t.Fatalf("Request = (%v, %v), want (true, nil)", canRun, err)
	}

	afterRequest := pttl(t, client, key)
	if afterRequest <= minTime {
		t.Fatalf("state TTL after Request = %v, want longer than MinTime %v", afterRequest, minTime)
	}

	if err := store.RegisterDone(id, 1); err != nil {
		t.Fatalf("RegisterDone failed: %v", err)
	}

	afterDone := pttl(t, client, key)
	if afterDone <= minTime {
		t.Fatalf("RegisterDone cut the state TTL to %v; the %v spacing window would expire with it", afterDone, minTime)
	}
	// It must not have been extended either: only Request decides the window.
	if afterDone > afterRequest {
		t.Fatalf("state TTL grew from %v to %v across RegisterDone", afterRequest, afterDone)
	}
}

// TestLegacyState_SpacingHoldsAcrossCompletion is the behavioral half: a second
// request stays refused across the window, and the remaining wait keeps counting
// down from the original start rather than restarting.
func TestLegacyState_SpacingHoldsAcrossCompletion(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("legacy-spacing")
	const minTime = 3 * time.Second
	opts := gothrottle.Options{MaxConcurrent: 2, MinTime: minTime}
	key := gothrottle.RedisStateKey(id)

	if canRun, _, err := store.Request(id, 1, opts); err != nil || !canRun {
		t.Fatalf("Request = (%v, %v), want (true, nil)", canRun, err)
	}
	if err := store.RegisterDone(id, 1); err != nil {
		t.Fatalf("RegisterDone failed: %v", err)
	}

	// last_start_us must survive completion; spacing is measured from the start.
	if err := client.HGet(ctx, key, "last_start_us").Err(); err != nil {
		t.Fatalf("RegisterDone lost last_start_us: %v", err)
	}

	deadline := time.Now().Add(1500 * time.Millisecond)
	var lastWait time.Duration
	for time.Now().Before(deadline) {
		canRun, waitTime, err := store.Request(id, 1, opts)
		if err != nil {
			t.Fatalf("Request during the window failed: %v", err)
		}
		if canRun {
			t.Fatal("a request was admitted inside the spacing window after the previous job completed")
		}
		if waitTime <= 0 || waitTime > minTime {
			t.Fatalf("waitTime = %v, want in (0, %v]", waitTime, minTime)
		}
		if lastWait != 0 && waitTime > lastWait {
			t.Fatalf("remaining wait grew from %v to %v; the window restarted", lastWait, waitTime)
		}
		lastWait = waitTime
		time.Sleep(200 * time.Millisecond)
	}

	time.Sleep(lastWait + 200*time.Millisecond)

	canRun, _, err := store.Request(id, 1, opts)
	if err != nil {
		t.Fatalf("Request after the window failed: %v", err)
	}
	if !canRun {
		t.Fatal("a request was still refused after the spacing window closed")
	}
}

// TestLegacyState_RegisterDoneOnMissingKeyIsSafe covers the completion of a job
// whose state has already gone — a store that was flushed, or a TTL that lapsed
// while the process was away. It must not resurrect a counter, and must not
// error.
func TestLegacyState_RegisterDoneOnMissingKeyIsSafe(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("legacy-missing")
	key := gothrottle.RedisStateKey(id)

	if err := store.RegisterDone(id, 1); err != nil {
		t.Fatalf("RegisterDone on a missing key failed: %v", err)
	}

	// A zeroed counter is acceptable; a key with no expiry is not, because it
	// would outlive the process that wrote it.
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL failed: %v", err)
	}
	if ttl == -1 {
		t.Fatal("RegisterDone left state with no expiry; it would leak for the lifetime of the Redis instance")
	}

	// And it must not have blocked the next request.
	canRun, _, err := store.Request(id, 1, gothrottle.Options{MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if !canRun {
		t.Fatal("a completion against missing state blocked the next request")
	}
}

// TestLegacyState_RepeatedCompletionCannotCreateCapacity is the underflow guard:
// more completions than admissions must not push the counter negative and let
// extra work through.
func TestLegacyState_RepeatedCompletionCannotCreateCapacity(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	ctx := context.Background()
	id := uniqueLimiterID("legacy-underflow")
	opts := gothrottle.Options{MaxConcurrent: 1}
	key := gothrottle.RedisStateKey(id)

	if canRun, _, err := store.Request(id, 1, opts); err != nil || !canRun {
		t.Fatalf("Request = (%v, %v), want (true, nil)", canRun, err)
	}
	for i := 0; i < 5; i++ {
		if err := store.RegisterDone(id, 1); err != nil {
			t.Fatalf("RegisterDone %d failed: %v", i+1, err)
		}
	}

	running, err := client.HGet(ctx, key, "running").Result()
	if err != nil {
		t.Fatalf("HGET running failed: %v", err)
	}
	if running != "0" {
		t.Fatalf("running = %q after 1 admission and 5 completions, want \"0\"", running)
	}

	// One slot, so exactly one request may be admitted however many completions
	// were recorded.
	if canRun, _, err := store.Request(id, 1, opts); err != nil || !canRun {
		t.Fatalf("Request = (%v, %v), want (true, nil)", canRun, err)
	}
	canRun, _, err := store.Request(id, 1, opts)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if canRun {
		t.Fatal("repeated completions inflated capacity beyond MaxConcurrent 1")
	}
}

// TestLegacyState_RequestDoesNotShortenAnExistingWindow covers the reverse
// direction: a caller passing a shorter MinTime must not shorten the TTL
// protecting a window another caller opened.
func TestLegacyState_RequestDoesNotShortenAnExistingWindow(t *testing.T) {
	client := newTestRedisClient(t)
	store, err := gothrottle.NewRedisStore(client)
	if err != nil {
		t.Fatalf("NewRedisStore failed: %v", err)
	}
	defer func() { _ = store.Disconnect() }()

	id := uniqueLimiterID("legacy-ttl-monotonic")
	key := gothrottle.RedisStateKey(id)

	if canRun, _, err := store.Request(id, 1, gothrottle.Options{MinTime: 60 * time.Second}); err != nil || !canRun {
		t.Fatalf("Request = (%v, %v), want (true, nil)", canRun, err)
	}
	long := pttl(t, client, key)

	// Refused by spacing, but the script still touches the key's TTL on the way
	// out of a successful branch elsewhere; either way it must not shrink.
	if _, _, err := store.Request(id, 1, gothrottle.Options{MinTime: time.Millisecond}); err != nil {
		t.Fatalf("second Request failed: %v", err)
	}
	if err := store.RegisterDone(id, 1); err != nil {
		t.Fatalf("RegisterDone failed: %v", err)
	}

	after := pttl(t, client, key)
	// Allow for the time the test itself took.
	if after < long-5*time.Second {
		t.Fatalf("state TTL fell from %v to %v; a shorter MinTime shortened another caller's window", long, after)
	}
}
