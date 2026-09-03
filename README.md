# GoThrottle

<p align="center">
  <img src="assets/logo-256.png" alt="GoThrottle Logo" width="200"/>
</p>

<p align="center">
  <a href="https://golang.org/"><img src="https://img.shields.io/github/go-mod/go-version/AFZidan/gothrottle" alt="Go Version"></a>
  <a href="https://github.com/AFZidan/gothrottle/actions"><img src="https://github.com/AFZidan/gothrottle/workflows/CI/CD%20Pipeline/badge.svg" alt="Build Status"></a>
  <a href="https://goreportcard.com/report/github.com/AFZidan/gothrottle"><img src="https://goreportcard.com/badge/github.com/AFZidan/gothrottle" alt="Go Report Card"></a>
  <a href="https://codecov.io/gh/AFZidan/gothrottle"><img src="https://codecov.io/gh/AFZidan/gothrottle/branch/main/graph/badge.svg" alt="Coverage Status"></a>
  <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://godoc.org/github.com/AFZidan/gothrottle"><img src="https://godoc.org/github.com/AFZidan/gothrottle?status.svg" alt="GoDoc"></a>
</p>

<p align="center">
  <strong>A Go package for request throttling and rate limiting, heavily inspired by the Node.js <a href="https://www.npmjs.com/package/bottleneck">bottleneck</a> package.</strong>
</p>

## Features

- **Local and Distributed Rate Limiting**: Supports both in-memory (LocalStore) and Redis-based (RedisStore) backends
- **Configurable Limits**: Set maximum concurrent jobs and minimum time between jobs
- **Priority Queue**: Jobs are executed by priority, FIFO within a priority
- **Event-Driven Scheduler**: Dispatches as capacity frees up, with no idle polling
- **Atomic Operations**: Redis operations use Lua scripts, with the clock read from Redis so instances need not agree on time
- **Renewable Leases**: Capacity is held by tokenized leases, so a long job keeps its slot and a crashed process releases it
- **Independent Rate Spacing**: `MinTime` is measured from a job's start and outlives the lease that started it, so a crash cannot grant the next job a free start
- **Configuration Agreement**: Instances sharing a limiter ID must agree on the distributed limits, or the disagreement is reported rather than silently resolved
- **Easy Integration**: Simple API for wrapping existing functions

## Unreleased

### Fixed

- **Direct datastore calls now enforce `Options.Validate`**. `NewLimiter`
  validated its options, but `LocalStore.Request`, `LocalStore.Acquire`,
  `RedisStore.Request` and `RedisStore.Acquire` did not, so a caller using a store
  directly could pass a negative `MaxConcurrent`, `MinTime` or `LeaseTTL` and get
  the old silent behavior: the value failed the `> 0` guard in the admission path
  and every request was admitted. A limiter configured with a miscalculated limit
  enforced nothing, with no error to say so. All four now run the same validator
  the limiter does, before any token is generated, any Redis command is issued,
  any local state is created and any TTL or spacing record is touched.
- **`SchedBestFit` accounts for capacity held in other processes**. The policy
  chose a candidate from the limiter's *local* running weight alone. With a shared
  `MaxConcurrent` of 3, process A holding weight 2, and process B queueing a
  high-priority weight-2 job ahead of a weight-1 job, B's local weight of 0 said
  the head job fit; Redis refused it; the scheduler requeued it and ended the
  pass. The weight-1 job that fitted the real distributed capacity was never
  attempted, so throughput collapsed to the head job's retry rate. A capacity
  refusal is now read as information about the store — that weight does not fit,
  so nothing at least as heavy does either — which lowers a per-pass ceiling and
  lets strictly lighter queued jobs be considered in the same pass. Refused jobs
  keep their sequence numbers, so priority and FIFO order are unchanged; a
  `MinTime` refusal still ends the pass, because spacing gates the limiter rather
  than one job. `SchedStrict` is untouched.
- **Redis orphan reconciliation compares membership, not cardinality**. The
  acquire script only reconciled when `HLEN(leases) ~= ZCARD(expirations)`, which
  misses equal-cardinality corruption: a lease hash holding token A and an expiry
  ZSET holding token B are both size 1, so nothing was repaired and token A
  consumed capacity forever with no expiry entry that could ever release it. The
  running weight is now summed by walking the expiry ZSET and reading each token's
  weight, so a weight with no expiry entry is never counted, and expiry entries
  with no weight are removed. Both collections are walked and deleted in batches,
  so no `unpack()` can approach Lua's argument limit however much corruption has
  accumulated. `last-start`, live leases and the configuration record are not
  touched. See "Orphan reconciliation" in the README for what is and is not
  guaranteed for an unlimited limiter.
- **The legacy Redis spacing path clamps a backwards clock**. The lease script
  already capped a computed wait at `MinTime`; `Request` did not, so a Redis
  server whose clock stepped backwards reported a wait far longer than the window
  itself and stalled the caller past the spacing it actually owed.
- **Arithmetic on state windows and running weight cannot overflow**. `2 *
  MinTime` — the legacy state TTL and the spacing window — wrapped negative past
  half the `Duration` range, which compared as "shorter than the default" and left
  the window unprotected, the opposite of what the doubling is for. Weight sums
  now saturate instead of wrapping negative, since a negative total reads as free
  capacity, and capacity comparisons are written as subtraction against the limit
  so no sum is formed that could exceed `int` on a 32-bit build. Microsecond
  values arriving from Redis are clamped on conversion, so a nonsense reply cannot
  become a negative wait that the scheduler reads as "no deadline".
- **The release workflow gates publication instead of reporting on it**. It was
  tag-triggered: by the time it ran, the tag existed and the module was already
  resolvable, so a failing test could not stop a release. It also called
  `gh release create` unconditionally, which failed with "already exists" when the
  release had been created by hand — and took the dependent Go proxy warm-up down
  with it. That is why the historical `v1.1.0` Release run is red. See "Releasing"
  in the README for the replacement.

### Added

- `ErrValueOutOfRange`, returned when `MaxConcurrent`, a job weight, or `MinTime`
  or `LeaseTTL` as a microsecond count exceeds 2^53-1. Redis decides admission
  inside Lua, whose numbers are IEEE-754 doubles, so above that boundary two
  distinct limits can compare equal and a limit can shift as it crosses it —
  the limit being enforced would not be the limit that was configured. Rejecting
  the value is preferred to enforcing it approximately.
- An `actionlint` job in CI, and `make lint-workflows`, so a mistake in the
  release workflow surfaces on every push rather than when someone next tries to
  cut a release.
- Regression tests for each of the above: `tests/store_validation_test.go`
  (direct-call validation, numeric boundaries), `tests/bestfit_test.go`
  (distributed best fit, including two limiters over two Redis stores) and
  `tests/redis_orphan_test.go` (every shape of orphan corruption, bounded repair,
  and a command-count assertion that a healthy limiter does not scan the lease
  hash).

### Changed

- **Direct datastore calls reject invalid options rather than proceeding**. This
  is the intended fix above, but it is a behavior change for code that called
  `Request` or `Acquire` directly with options a limiter would have rejected: such
  a call now returns the corresponding sentinel instead of admitting the job.
  Every sentinel and `errors.Is` relationship is unchanged.

  The validator is `Options.Validate` in full, including the scheduler-only fields
  (`SchedPolicy`, `RetryInterval`, `MaxQueueSize`) that no store reads. A negative
  `RetryInterval` is a configuration mistake wherever it appears, and one
  validator with one behavior is worth more than a store-specific subset where
  which mistakes are caught depends on the entry point used.
- **The release workflow is `workflow_dispatch` only**. Pushing a tag no longer
  triggers a release. See "Releasing" in the README.

## Installation

```bash
go get github.com/AFZidan/gothrottle
```

## Quick Start

### Local Rate Limiting

```go
package main

import (
    "fmt"
    "time"
    "github.com/AFZidan/gothrottle"
)

func main() {
    // Create a limiter with local storage
    limiter, err := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 2,                    // Max 2 concurrent jobs
        MinTime:       100 * time.Millisecond, // 100ms between jobs
    })
    if err != nil {
        panic(err)
    }
    defer limiter.Stop()

    // Schedule a job
    result, err := limiter.Schedule(func() (interface{}, error) {
        // Your work here
        return "Hello, World!", nil
    })
    
    fmt.Println(result) // "Hello, World!"
}
```

### Distributed Rate Limiting with Redis

```go
package main

import (
    "time"
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

func main() {
    // Create Redis client
    rdb := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })
    defer rdb.Close() // the client is yours; the limiter never closes it

    // Create Redis store
    store, err := gothrottle.NewRedisStore(rdb)
    if err != nil {
        panic(err)
    }
    defer store.Disconnect()

    // Create limiter with Redis backend
    limiter, err := gothrottle.NewLimiter(gothrottle.Options{
        ID:            "my-distributed-limiter", // Required for Redis
        MaxConcurrent: 5,
        MinTime:       200 * time.Millisecond,
        Datastore:     store,
    })
    if err != nil {
        panic(err)
    }
    defer limiter.Stop()

    // This limiter will now coordinate with other instances
    // using the same Redis store and limiter ID
}
```

## API Reference

### Options

```go
type Options struct {
    ID            string        // Unique ID for the limiter (required for Redis)
    MaxConcurrent int           // Maximum concurrent jobs (0 = unlimited)
    MinTime       time.Duration // Minimum time between jobs
    Datastore     Datastore     // Storage backend (nil = LocalStore)

    // Let Stop() disconnect an injected Datastore (default false)
    CloseDatastoreOnStop bool

    // How weighted jobs compete for capacity (default SchedStrict)
    SchedPolicy SchedPolicy

    // How often to re-check a distributed store while blocked (default 10ms)
    RetryInterval time.Duration

    // Cap on queued jobs; further submissions get ErrQueueFull (0 = unbounded)
    MaxQueueSize int

    // How long a capacity reservation survives without renewal (default 30s)
    LeaseTTL time.Duration

    // Receives errors that have no caller to return them to
    OnError func(error)
}
```

Negative values for `MaxConcurrent`, `MinTime`, `MaxQueueSize`, `RetryInterval`
or `LeaseTTL` are rejected rather than being treated as "unlimited" or "use the
default" — a miscalculated limit fails loudly instead of silently switching
throttling off. Zero keeps its meaning of "no limit" or "use the default". You
can call `opts.Validate()` yourself, and every admission path runs it: see
[Validation applies to direct datastore calls](#validation-applies-to-direct-datastore-calls).

`MaxConcurrent` and job weights are also capped at 2^53-1, and `MinTime` and
`LeaseTTL` at 2^53-1 microseconds (about 285 years). See [Numeric
limits](#numeric-limits).

### Scheduling

The scheduler is event-driven: it wakes when a job is enqueued, when a running
job releases capacity, or when a `MinTime` window expires. An idle limiter does
not wake at all, and a burst of jobs fills the available concurrency window
immediately instead of starting one job per tick.

Jobs are ordered by priority (higher first), and equal-priority jobs run in
submission order (FIFO).

`SchedPolicy` decides what happens when the highest-priority job is too heavy
for the free capacity:

- `SchedStrict` (default) — the heavy job holds the queue. Priority is never
  inverted, but capacity can sit idle while it waits.
- `SchedBestFit` — lighter, lower-priority jobs may use capacity the heavy job
  cannot fill yet. Better throughput, at the cost of letting light work overtake
  a heavy high-priority job.

`RetryInterval` only applies to distributed setups: when a shared store refuses
capacity, the release happens in another process and produces no local event, so
the scheduler re-checks on this interval.

#### `SchedBestFit` with a shared store

Best fit works against the capacity the *store* has, not just what this limiter
is running. That distinction matters because a limiter cannot see another
process's reservations: its own running weight can be zero while the shared limit
is fully committed elsewhere.

So when the store refuses a job for capacity, `SchedBestFit` treats the refusal
as information — that weight does not fit, therefore nothing at least as heavy
does either — and looks for a strictly lighter queued job in the same dispatch
pass. With a shared `MaxConcurrent` of 3, one process holding weight 2, and a
queue holding a high-priority weight-2 job ahead of a weight-1 job, the weight-1
job runs; the weight-2 job waits for capacity and runs when it frees up.

Three properties bound the behavior:

- Each refusal lowers the ceiling strictly, so every attempt in a pass is for a
  lighter job than the last. A pass cannot retry the same job or spin.
- A refused job keeps its sequence number, so priority and FIFO order among
  equal-priority jobs are unaffected by having been held back.
- A `MinTime` refusal ends the pass. Spacing gates the limiter rather than one
  job, so no lighter job could start either, and asking would only spend
  datastore round trips.

Under `SchedStrict` a refusal still ends the pass immediately: the head job holds
the queue until the store admits it.

### Limiter Methods

#### `NewLimiter(opts Options) (*Limiter, error)`

Creates a new limiter instance.

#### `Schedule(task func() (interface{}, error)) (interface{}, error)`

Schedules a job with default priority (5) and weight (1). Blocks until completion.

#### `ScheduleWithOptions(task func() (interface{}, error), priority, weight int) (interface{}, error)`

Schedules a job with custom priority and weight. Higher priority jobs run first.

Returns `ErrNilTask` for nil tasks, `ErrInvalidWeight` for non-positive weights, and `ErrWeightExceedsMax` when a weighted job cannot fit within the configured `MaxConcurrent` limit.

#### `Wrap(fn func() (interface{}, error)) func() (interface{}, error)`

Returns a wrapped version of the function that applies rate limiting.

#### `ScheduleContext(ctx context.Context, task func() (interface{}, error)) (interface{}, error)`

Like `Schedule`, but bounded by a context. If `ctx` ends while the job is still
queued, the job is removed from the queue and `ctx.Err()` is returned. A job that
has already started runs to completion — the limiter cannot interrupt a task
function — and its real result is returned rather than a cancellation that did
not happen.

`ScheduleWithOptionsContext` is the same with a custom priority and weight.

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()

result, err := limiter.ScheduleContext(ctx, fetchPage)
if errors.Is(err, context.DeadlineExceeded) {
    // gave up waiting in the queue
}
```

#### `QueueLen() int` and `Running() int`

Point-in-time queue depth and the total weight currently executing, for
monitoring against `MaxQueueSize` and `MaxConcurrent`.

#### `Stop() error`

Stops the limiter and cleans up resources.

`Stop` cancels queued jobs with `ErrStoreClosed`, waits for running jobs to
finish, and guarantees no task starts after shutdown begins — including a task
whose capacity request was already in flight. It is safe to call concurrently
and repeatedly; every caller blocks until shutdown completes and receives the
same error. Do not call `Stop` from inside a scheduled task: it waits for that
task to finish.

#### Cancellation

Two contexts are in play, and they answer different questions.

The context you pass to `ScheduleContext` bounds *your* wait. Cancel it while the
job is queued and the job is removed and `ctx.Err()` returned; cancel it after the
job has started and the limiter waits for the real result, because a `func()`
cannot be interrupted.

The limiter also owns a context, cancelled when `Stop` begins, that it passes to
`LeaseDatastore` calls. A store blocked in `Acquire` or `Renew` — on a slow
network, a lock, a queue of its own — is therefore released by shutdown rather
than holding it open. If shutdown cancels an acquisition, the queued caller
receives `ErrStoreClosed`: the job never ran, so the terminal shutdown error is
the honest answer, not the internal cancellation.

`Release` is deliberately exempt. Handing capacity back has to succeed *because*
the process is going away, so it runs under a deadline of its own —
`max(LeaseTTL, 5s)`, covering every retry. Past that the store reclaims the lease
anyway, so a wedged store is reported through `OnError` and abandoned instead of
blocking `Stop` indefinitely.

These guarantees need a `LeaseDatastore`. The legacy `Datastore` interface takes
no context, so `Request` and `RegisterDone` can only be waited out — one more
reason the lease path is the one the limiter prefers.

#### Error reporting

Some failures have no caller to return them to. The most important is a failure
to hand capacity back to the datastore after a job finishes: the store keeps that
capacity reserved for work that has already completed. The limiter retries, then
reports through `OnError`:

```go
limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
    ID:            "api",
    MaxConcurrent: 10,
    Datastore:     store,
    OnError: func(err error) {
        log.Printf("gothrottle: %v", err)
    },
})
```

`OnError` is called from limiter goroutines, so it must be safe for concurrent
use and must not block or call back into the limiter.

Task panics are recovered and returned as a `*PanicError` that matches
`errors.Is(err, ErrTaskPanic)` and carries the stack trace:

```go
var panicErr *gothrottle.PanicError
if errors.As(err, &panicErr) {
    log.Printf("task panicked: %v\n%s", panicErr.Value, panicErr.Stack)
}
```

#### Datastore ownership

A datastore you pass in stays yours. `Stop` only disconnects the `LocalStore`
the limiter creates for itself, so stopping one limiter cannot break other
limiters sharing the same store, or other parts of your application sharing the
same Redis client:

```go
store, _ := gothrottle.NewRedisStore(rdb)

a, _ := gothrottle.NewLimiter(gothrottle.Options{ID: "a", Datastore: store})
b, _ := gothrottle.NewLimiter(gothrottle.Options{ID: "b", Datastore: store})

a.Stop()             // b and rdb are unaffected
b.Stop()
store.Disconnect()   // release the store when you are done with it
rdb.Close()          // you own the client, so you close it
```

Set `CloseDatastoreOnStop: true` to transfer ownership to a single limiter.
`RedisStore.Disconnect()` leaves the client open; `RedisStore.Close()` also
closes the client, for when the store is its sole user.

### Validation Errors

- `ErrMissingID`: returned when a datastore-backed limiter is created without an ID.
- `ErrInvalidID`: returned when a limiter ID is too long or contains control characters.
- `ErrInvalidWeight`: returned when a job or datastore operation uses a non-positive weight.
- `ErrWeightExceedsMax`: returned when a job weight exceeds the configured `MaxConcurrent` limit.
- `ErrNilTask`: returned when scheduling a nil task function.
- `ErrTaskPanic`: matched by the `*PanicError` returned when a scheduled task panics.
- `ErrStoreClosed`: returned when scheduling against a stopped limiter or closed datastore.
- `ErrQueueFull`: returned when the queue has reached `MaxQueueSize`.
- `ErrLimiterConfigMismatch`: returned by `Acquire` when another instance already
  registered this limiter ID with a different `MaxConcurrent`, `MinTime` or
  `LeaseTTL`. See [Same-ID configuration consistency](#same-id-configuration-consistency).
- `ErrInvalidMaxConcurrent`, `ErrInvalidMinTime`, `ErrInvalidMaxQueueSize`,
  `ErrInvalidRetryInterval`, `ErrInvalidLeaseTTL`, `ErrInvalidSchedPolicy`:
  returned by `NewLimiter`, `Options.Validate` and every datastore admission call
  for negative or unknown configuration values.
- `ErrValueOutOfRange`: returned when `MaxConcurrent`, a job weight, or `MinTime`
  or `LeaseTTL` in microseconds exceeds what Redis Lua can compare exactly. See
  [Numeric limits](#numeric-limits).
- `ErrNilClient`: returned by `NewRedisStore(nil)`, including a typed nil such as
  `(*redis.Client)(nil)`. It unwraps to `ErrStoreClosed`, which is what earlier
  versions returned.

#### Validation applies to direct datastore calls

`Datastore` and `LeaseDatastore` are exported, so `Request`, `Acquire`,
`RegisterDone`, `Renew` and `Release` can be called without a limiter. Those
calls enforce the same rules `NewLimiter` does.

`Request` and `Acquire` run `Options.Validate()` in full and additionally reject a
non-positive weight, a weight above a positive `MaxConcurrent`, an out-of-range
weight, and a malformed limiter ID — plus an empty ID for `RedisStore`, where the
ID becomes part of a key. Validation happens before anything is generated or
written: no lease token, no Redis command, no local state, no configuration
registration, and no change to a TTL or the spacing record. A rejected call leaves
the store exactly as it was.

`Options.Validate()` is applied whole, including the three fields no store reads —
`SchedPolicy`, `RetryInterval` and `MaxQueueSize`. They shape how one process
queues its own work, so a store has no use for them, but a negative
`RetryInterval` is a configuration mistake wherever it appears, and one validator
with one behavior is easier to rely on than a store-specific subset where which
mistakes get caught depends on which entry point was used.

`RegisterDone` is deliberately narrower: it carries no `Options`, so only the
limiter ID and weight are checked.

#### Numeric limits

Redis decides admission inside a Lua script, and Lua 5.1 numbers are IEEE-754
doubles: integers are exact only up to 2^53-1. Past that boundary two different
limits can compare equal, and a limit can shift as it crosses it — the limit being
enforced would not be the limit that was configured. Rather than enforce a limit
approximately, values that cannot survive the trip are rejected with
`ErrValueOutOfRange`:

| Value | Maximum |
| --- | --- |
| `MaxConcurrent` | 2^53-1 (9,007,199,254,740,991) |
| Job weight | 2^53-1 |
| `MinTime` | 2^53-1 microseconds (~285 years) |
| `LeaseTTL` | 2^53-1 microseconds |

Everything below those bounds is exact. Arithmetic on values that pass validation
saturates rather than wrapping — the doubled state windows, running-weight sums —
because a wrapped negative would read as a shorter window or as free capacity,
and capacity comparisons are written as subtraction against the limit so no sum
is formed that could exceed `int` on a 32-bit build.

### Storage Backends

#### LocalStore

In-memory storage for single-instance applications. This is the default when no `Datastore` is specified.

```go
store := gothrottle.NewLocalStore()
```

#### RedisStore

Redis-based storage for distributed rate limiting across multiple application instances.

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store, err := gothrottle.NewRedisStore(rdb)
```

`Disconnect()` releases the store but leaves `rdb` open, because the client is
yours. Use `Close()` when the store is the only user of the client.

`NewRedisStore` takes go-redis's `redis.UniversalClient`, which `*redis.Client`
satisfies — so the call above is unchanged — as do `*redis.ClusterClient`,
`*redis.Ring` and the Sentinel-backed failover client. They compile and construct;
what the tests actually demonstrate for each is set out under [Cluster, Ring and
Sentinel support status](#redis-key-layout). A typed nil such as
`(*redis.Client)(nil)` is rejected with `ErrNilClient` rather than accepted and
panicked on later.

##### Same-ID configuration consistency

A limiter ID names a shared policy, so every instance using it must agree on the
settings that decide admission:

| Must match across instances | May differ per instance |
| --- | --- |
| `MaxConcurrent`, `MinTime`, `LeaseTTL` | `RetryInterval`, `MaxQueueSize`, `SchedPolicy`, `OnError`, `CloseDatastoreOnStop` |

The first acquisition for an ID records the left-hand column; a later one that
disagrees is refused with an error matching `ErrLimiterConfigMismatch`, naming
both configurations. Without this the effective distributed limit would be
whichever process happened to reach Redis first — a deployment mid-rollout would
enforce two different limits at once and neither reliably. The right-hand column
only shapes how one process queues its own work, so it is not compared.

```go
_, err := limiter.Schedule(work)
if errors.Is(err, gothrottle.ErrLimiterConfigMismatch) {
    // another instance registered this ID with different limits
    log.Fatalf("gothrottle: %v", err)
}
```

The record is not permanent. Once every lease has lapsed and the `MinTime` window
has closed — that is, once the limiter is genuinely idle — the ID can be
registered with new settings, so changing a limit is a matter of rolling out the
new configuration and letting the old one drain. Rolling out the change while
traffic is flowing will make the new instances report a mismatch until the old
ones stop.

##### Redis key layout

Each limiter ID occupies four keys, all sharing one Redis Cluster hash tag:

```text
gothrottle:{<tag>}:leases        HASH    lease token -> reserved weight
gothrottle:{<tag>}:expirations   ZSET    lease token -> expiry (µs, Redis clock)
gothrottle:{<tag>}:last-start    STRING  µs timestamp of the last admission
gothrottle:{<tag>}:config        HASH    MaxConcurrent, MinTime, LeaseTTL, ID
```

`<tag>` is a SHA-256 prefix of the limiter ID, not the ID itself: Redis takes the
slot from the first `{...}`, so an ID containing braces would otherwise choose its
own tag and could be steered onto another limiter's slot. `RedisKeys(id)` returns
the four names if you need to inspect or clear state operationally.

The legacy `Request`/`RegisterDone` path uses a single `gothrottle:<id>` hash,
available as `RedisStateKey(id)`. It needs no tag, being single-key.

##### Orphan reconciliation

Two keys describe one reservation: a weight in the lease hash and an expiry score
in the ZSET. The scripts always write both together, so a mismatch is state this
package did not produce — a partial restore, a manual `HDEL`, an AOF truncated
between the two commands. It has to be survivable, because a weight the expiry
path can never reclaim would hold capacity until someone noticed by hand.

For a limiter with a positive `MaxConcurrent`, every acquisition reconciles by
membership. The expiry ZSET is authoritative for liveness, so the running weight
is summed by walking it and reading each token's weight. That gives three
guarantees:

- A weight in the hash with no expiry entry is never counted, and is removed.
- An expiry entry with no weight is removed, so it cannot accumulate.
- A live lease is untouched: its weight still counts and it stays renewable.

The work is bounded. Members are read and deleted in batches, so no `unpack()`
approaches Lua's argument limit however much corruption has accumulated. The cost
is one `ZRANGE` plus one `HMGET` per 256 live leases — and live leases are bounded
by `MaxConcurrent`, since each weighs at least 1. The lease hash is only scanned
(`HKEYS`) when the counts still disagree after that, which is exactly when it
holds an orphan; a healthy limiter never scans it.

With `MaxConcurrent` at 0 the guarantee is narrower, deliberately. There is no
limit for an orphan to consume, so orphan state costs memory rather than
correctness — and the lease hash is not bounded by anything, so walking it on
every admission would be an unbounded cost paid by the configuration that needs it
least. An unlimited limiter therefore reconciles only when the O(1) cardinality
check shows a disagreement. Equal-cardinality corruption is left in place; it
blocks nothing, and the keys carry a TTL of twice the lease window, so one idle
window clears them.

`last-start` is never touched by reconciliation. Spacing is measured from when a
job started, so that record is not reservation state — see [Spacing outlives
reservations](#spacing-outlives-reservations).

**Cluster, Ring and Sentinel support status.** `NewRedisStore` takes
`redis.UniversalClient`, so a `*redis.ClusterClient`, a `*redis.Ring` and the
Sentinel-backed failover client all compile and construct. What the test suite
demonstrates is narrower than that, and the distinction is worth stating exactly:

| Configuration | Status |
| --- | --- |
| Standalone (Redis 6 and 7) | Covered by every Redis test in CI on both versions. |
| `*redis.Ring` | One test exercises acquire, renew, release, the concurrency limit and the legacy path through a single-node Ring. Multi-node sharding is not tested. |
| `*redis.ClusterClient` | Constructor and typed-nil handling only. No test runs against a real cluster. |
| Sentinel failover client | Not exercised by any test. |

The key layout is designed to be cluster-safe: all four keys for a limiter share
one hash tag, so the multi-key Lua scripts stay in a single slot, and the lease
scripts recover from `NOSCRIPT` with `EVAL`, which caches on whichever node serves
the key. That is a design property, not a tested one. Treat standalone as
supported; treat Cluster, multi-node Ring and Sentinel as untested until a real
multi-node test exists.

## Architecture

The package is built around a `Datastore` interface that allows pluggable storage backends:

```go
type Datastore interface {
    Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error)
    RegisterDone(limiterID string, weight int) error
    Disconnect() error
}
```

### Leases

A shared counter cannot distinguish a slow job from a dead one. Expiring the
counter is the only way to keep a crashed process from holding capacity forever,
but expiring it while a job is still running lets another job start over the
limit — and the finished job's late decrement can then corrupt the newcomer's
state.

`LeaseDatastore` tracks each reservation individually instead:

```go
type LeaseDatastore interface {
    Datastore

    Acquire(ctx context.Context, limiterID string, weight int, opts Options) (*Lease, time.Duration, error)
    Renew(ctx context.Context, lease *Lease) error
    Release(ctx context.Context, lease *Lease) error
}
```

Every lease has a unique token and its own expiry. The limiter renews while a job
runs, so a long job keeps its capacity; if the holder dies, renewal stops and the
capacity is reclaimed within `LeaseTTL`. Because `Release` names one token, a late
release from an expired job cannot disturb a newer lease.

Both `LocalStore` and `RedisStore` implement this, and the limiter uses it
automatically. The interface is additive: a custom `Datastore` that implements
only `Request`/`RegisterDone` still works, on the older counter semantics.

- **LocalStore**: Uses Go mutexes and in-memory state
- **RedisStore**: Uses atomic Lua scripts, with the clock read from Redis `TIME`
  so coordinating instances need not agree on the time

### Spacing outlives reservations

`MinTime` is measured from when a job *started*, so the record of that start has to
outlive the reservation it belongs to. The two are kept as separate state, and the
distinction matters in four cases:

| After | Why the window survives |
| --- | --- |
| Normal release | Release removes one lease token. It never touches the last-start record, so finishing early does not let the next job start early. |
| Renewal | Renewal knows only the lease TTL. It extends the reservation's lifetime and leaves the spacing record's alone, which is derived from `MinTime`. |
| Lease expiry | Reclaiming an expired lease purges reservation state only. The spacing record is not reservation state. |
| Process crash | Same path as expiry: the dead holder's capacity comes back, but the spacing window it opened runs its course. |

The failure mode this prevents is specific. Suppose `MinTime: 45s` with
`LeaseTTL: 1s`. If the spacing record's lifetime were tied to the lease — as it
was when start times were stored per lease token — a release or a renewal would
leave roughly two seconds protecting a forty-five second window. Once it expired,
the next job would start immediately, and nothing in the logs would say the limit
had stopped applying. A crashed holder was worse: purging its lease purged its
start time with it, so the crash itself granted the next job a free start.

So: the last-start value is written only on a successful admission, never deleted
or shortened by renewal, release or reclamation, and its own garbage-collection
window is derived from `MinTime` alone (at least `2 × MinTime`). With `MinTime`
unset there is no window to enforce and no record is written at all.

Key lifetimes are only ever extended, never shortened. A short-lived operation —
a release with a one-second lease TTL, say — cannot cut short a key protecting a
longer window, in either the lease scripts or the legacy `Request`/`RegisterDone`
pair. The same asymmetry used to bite there: `Request` sized the state TTL to
cover `MinTime`, and `RegisterDone` reset it to a flat 30 seconds.

## Project Structure

```text
gothrottle/
├── datastore.go         # Datastore interface definition
├── lease.go            # LeaseDatastore interface and Lease type
├── options.go          # Configuration options and validation
├── job.go             # Job struct and priority queue
├── local_store.go     # In-memory storage implementation
├── local_lease.go     # In-memory lease implementation
├── redis_store.go     # Redis-based storage implementation
├── redis_lease.go     # Redis lease implementation and Lua scripts
├── limiter.go         # Main Limiter struct and logic
├── bounds.go          # Numeric policy: exact ranges and saturating arithmetic
├── errors.go          # Common error definitions and PanicError
├── assets/            # Visual assets and branding
│   ├── logo.svg                 # Vector logo
│   ├── logo-*.png              # PNG logos (64px, 128px, 256px, 512px)
│   ├── social-preview.svg       # Social media preview (vector)
│   ├── social-preview-1280x640.png # GitHub social preview (PNG)
│   └── README.md               # Asset documentation
├── tests/             # Test files
│   ├── examples_test.go         # Basic usage examples
│   ├── limiter_test.go          # Core limiter unit tests
│   ├── scheduler_test.go        # Scheduler throughput and ordering
│   ├── shutdown_test.go         # Shutdown and datastore ownership
│   ├── cancellation_test.go     # Shutdown cancellation of store operations
│   ├── options_test.go          # Configuration validation
│   ├── store_validation_test.go # Direct datastore validation and numeric bounds
│   ├── bestfit_test.go          # SchedBestFit against distributed capacity
│   ├── context_test.go          # Context cancellation and error reporting
│   ├── lease_test.go            # Lease contract, both stores
│   ├── spacing_test.go          # MinTime independence from lease lifecycle
│   ├── legacy_state_test.go     # Request/RegisterDone state and TTL behavior
│   ├── config_consistency_test.go # Same-ID configuration agreement
│   ├── redis_keys_test.go       # Key layout, hash tags, client types
│   ├── redis_orphan_test.go     # Orphan reconciliation and bounded repair
│   ├── adversarial_test.go      # Failure-scenario coverage
│   ├── integration_test.go      # Integration tests and benchmarks
│   ├── redis_helpers_test.go    # Redis test helpers
│   ├── database_test.go         # Database throttling tests
│   └── advanced_database_test.go # Advanced DB operations with weights
├── .github/           # GitHub workflows and templates
│   ├── workflows/
│   │   ├── ci.yml                # CI/CD pipeline
│   │   ├── release.yml           # Gated, idempotent release (workflow_dispatch)
│   │   └── codeql.yml           # Security analysis
│   ├── ISSUE_TEMPLATE/
│   │   ├── bug_report.md
│   │   ├── feature_request.md
│   │   └── documentation.md
│   └── pull_request_template.md
├── Makefile           # Development commands and workflows
├── go.mod             # Go module definition
├── go.sum             # Go module checksums
├── docker-compose.test.yml # Docker testing environment
├── Dockerfile.test    # Docker test container
├── README.md          # This file
├── CONTRIBUTING.md    # Contribution guidelines
├── CHANGELOG.md       # Version history
├── SECURITY.md        # Security policy
└── LICENSE            # MIT License
```

## Examples

See `tests/examples_test.go` for more detailed examples of usage patterns.

## Development

GoThrottle includes a comprehensive Makefile that provides all the common development commands. The Makefile offers a consistent and easy way to build, test, lint, and manage the project.

### Getting Started

```bash
# Show all available commands
make help

# Install development tools (golangci-lint, gosec)
make install-tools

# Quick development workflow (format, vet, test)
make dev
```

### Common Commands

```bash
# Build and Test
make build                 # Build the project
make test                  # Run tests
make test-race            # Run tests with race detector
make test-cover           # Run tests with coverage
make test-bench           # Run benchmarks
make test-all             # Run all tests (race, coverage, benchmarks)

# Code Quality
make fmt                  # Format code
make fmt-check           # Check if code is formatted
make vet                 # Run go vet
make lint                # Run golangci-lint
make security            # Run gosec security scan
make quality             # Run all quality checks

# Coverage
make coverage-html       # Generate HTML coverage report
make coverage-check      # Check coverage meets minimum threshold (60%)

# Dependencies
make deps                # Download dependencies
make verify              # Verify dependencies
make mod-tidy            # Tidy up go.mod and go.sum
make mod-update          # Update dependencies to latest versions

# Cross-platform builds
make cross-build         # Build for multiple platforms (Linux, macOS, Windows)

# CI Simulation
make ci                  # Simulate full CI pipeline locally
make release-check       # Full release readiness check
```

### Quick Development Workflows

```bash
# Quick test cycle during development
make quick-test          # Format → Vet → Test

# Quick build cycle
make quick-build         # Format → Vet → Build

# Full quality gate (before committing)
make quality             # Format check → Vet → Lint → Security scan

# Full CI simulation (before pushing)
make ci                  # Dependencies → Quality → All tests → Cross-build
```

### Coverage Requirements

The project maintains a minimum code coverage of **60%**. You can check if your changes meet this requirement:

```bash
make coverage-check
```

This will run the tests with coverage and verify that the total coverage meets the minimum threshold.

### Docker Testing

For testing with Redis in an isolated environment:

```bash
make docker-test         # Run tests in Docker with Redis
```

### Watch Mode

For continuous testing during development (requires `entr`):

```bash
make watch-test          # Automatically run tests when files change
```

### Manual Testing Commands

If you prefer to run commands manually without the Makefile:

```bash
# Run all tests
go test ./tests/... -v

# Run benchmarks
go test ./tests/... -bench=. -benchmem

# Test with coverage
go test -v -race -coverprofile=coverage.out -coverpkg=./... ./tests/...

# Test a specific function
go test ./tests/... -run TestLimiter_MaxConcurrent -v
```

## Releasing

There is one authoritative release path: the `Release` workflow, triggered
manually. Pushing a tag does not release anything.

```bash
# Release the current tip of main
gh workflow run release.yml -f version=v1.1.1

# Or release an exact commit
gh workflow run release.yml -f version=v1.1.1 -f commit=<sha>

# Verify everything without creating a tag or a release
gh workflow run release.yml -f version=v1.1.1 -f dry_run=true
```

The workflow verifies before it publishes, in this order:

1. The version is a valid `vMAJOR.MINOR.PATCH` semantic version tag.
2. The commit exists and is an ancestor of `main`.
3. If the tag already exists, it points at that same commit — otherwise the run
   fails rather than moving a published tag.
4. Every check run for the commit has completed successfully, and no commit status
   is failing.
5. The module path can carry this major version.
6. `go vet`, the `gofmt -s` check, the full suite under `-race` against Redis with
   `REQUIRE_REDIS=true`, a confirmation that Redis tests actually executed rather
   than skipped, and the cross-platform builds all pass — run against the exact
   commit being released, not against whatever `main` has moved on to.

Only then does the `publish` job create the tag and the release, and the Go proxy
warm-up runs after it.

Re-running is safe. An existing tag at the same commit is reused, never moved or
recreated; an existing release is inspected rather than recreated, so the Go proxy
warm-up still runs instead of being skipped by a failure. Two release runs cannot
overlap: a `concurrency: release` group makes the second wait.

Least privilege applies throughout — the workflow is `contents: read` and only the
`publish` job takes `contents: write`.

### Why the historical v1.1.0 Release run is red

The `v1.1.0` release was created by hand, then the old tag-triggered workflow ran
and called `gh release create` for a release that already existed, which failed
with `Release.tag_name already exists`. The dependent Go proxy job was skipped as
a result. Its Redis race tests had passed; nothing about `v1.1.0`'s contents is in
question.

That run is left as it is. The tag and release are published artifacts that
consumers may already have resolved, and re-tagging to make an old workflow run
green would change what `go get` at that version means. The workflow that produced
it has been replaced, so the failure mode cannot recur.

## License

MIT License - see LICENSE file for details.

## Database Query Throttling

**GoThrottle** is excellent for throttling database operations to prevent overwhelming your database with too many concurrent queries. This is especially useful for:

- **Rate limiting API database calls**
- **Batch processing large datasets**  
- **Preventing database connection pool exhaustion**
- **Distributed rate limiting across multiple application instances**

### Basic Database Throttling

```go
package main

import (
    "database/sql"
    "gothrottle"
    _ "github.com/lib/pq" // PostgreSQL driver
)

// DatabaseThrottler wraps database operations with rate limiting
type DatabaseThrottler struct {
    db      *sql.DB
    limiter *gothrottle.Limiter
}

func NewDatabaseThrottler(db *sql.DB, opts gothrottle.Options) (*DatabaseThrottler, error) {
    limiter, err := gothrottle.NewLimiter(opts)
    if err != nil {
        return nil, err
    }
    
    return &DatabaseThrottler{
        db:      db,
        limiter: limiter,
    }, nil
}

// Query executes a throttled database query
func (dt *DatabaseThrottler) Query(query string, args ...interface{}) (*sql.Rows, error) {
    result, err := dt.limiter.Schedule(func() (interface{}, error) {
        return dt.db.Query(query, args...)
    })
    
    if err != nil {
        return nil, err
    }
    
    return result.(*sql.Rows), nil
}

func main() {
    db, _ := sql.Open("postgres", "connection_string")
    
    // Limit to 5 concurrent queries with 10ms between query starts
    throttledDB, _ := NewDatabaseThrottler(db, gothrottle.Options{
        MaxConcurrent: 5,
        MinTime:       10 * time.Millisecond,
    })
    defer throttledDB.Close()
    
    // Now all queries through throttledDB will be rate limited
    rows, err := throttledDB.Query("SELECT * FROM users WHERE active = ?", true)
    // ... handle results
}
```

### Weighted Database Operations

Different database operations can have different resource costs. You can assign weights:

```go
// Light SELECT queries (weight 1)
rows, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
    return db.Query("SELECT id FROM users")
}, 5, 1) // Priority 5, Weight 1

// Heavy analytical queries (weight 5)  
rows, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
    return db.Query("SELECT COUNT(*) FROM large_table GROUP BY complex_column")
}, 10, 5) // Priority 10, Weight 5

// With MaxConcurrent: 10, you can run either:
// - 10 light queries simultaneously, OR  
// - 2 heavy queries simultaneously, OR
// - Some combination that doesn't exceed 10 total weight
```

### Distributed Database Rate Limiting

For applications with multiple instances sharing the same database:

```go
// Use Redis for distributed coordination
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store, _ := gothrottle.NewRedisStore(rdb)

// All app instances using this same ID will share the rate limits
throttledDB, _ := NewDatabaseThrottler(db, gothrottle.Options{
    ID:            "shared-db-limiter",
    MaxConcurrent: 20, // Total across ALL instances
    MinTime:       5 * time.Millisecond,
    Datastore:     store,
})
```

## Real-World Use Cases & Examples

### 1. API Rate Limiting Middleware

Protect your API endpoints from being overwhelmed:

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

// APIThrottler wraps HTTP handlers with rate limiting
type APIThrottler struct {
    limiter *gothrottle.Limiter
}

func NewAPIThrottler(opts gothrottle.Options) (*APIThrottler, error) {
    limiter, err := gothrottle.NewLimiter(opts)
    if err != nil {
        return nil, err
    }
    return &APIThrottler{limiter: limiter}, nil
}

// ThrottleHandler wraps an HTTP handler with rate limiting
func (at *APIThrottler) ThrottleHandler(handler http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        _, err := at.limiter.Schedule(func() (interface{}, error) {
            handler(w, r)
            return nil, nil
        })
        
        if err != nil {
            http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
            return
        }
    }
}

func main() {
    // Create distributed rate limiter for API
    rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    store, _ := gothrottle.NewRedisStore(rdb)
    
    throttler, _ := NewAPIThrottler(gothrottle.Options{
        ID:            "api-rate-limiter",
        MaxConcurrent: 100,    // Max 100 concurrent API requests
        MinTime:       10 * time.Millisecond, // 10ms between requests
        Datastore:     store,
    })
    
    // Apply throttling to endpoints
    http.HandleFunc("/api/users", throttler.ThrottleHandler(handleUsers))
    http.HandleFunc("/api/orders", throttler.ThrottleHandler(handleOrders))
    
    http.ListenAndServe(":8080", nil)
}

func handleUsers(w http.ResponseWriter, r *http.Request) {
    // Simulate database query
    time.Sleep(50 * time.Millisecond)
    json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func handleOrders(w http.ResponseWriter, r *http.Request) {
    // Simulate heavy database operation
    time.Sleep(200 * time.Millisecond)
    json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}
```

### 2. File Processing Pipeline

Throttle file processing to prevent system overload:

```go
package main

import (
    "fmt"
    "io/ioutil"
    "os"
    "path/filepath"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type FileProcessor struct {
    limiter *gothrottle.Limiter
}

func NewFileProcessor() *FileProcessor {
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 5,     // Process max 5 files concurrently
        MinTime:       100 * time.Millisecond, // 100ms between file processing
    })
    
    return &FileProcessor{limiter: limiter}
}

func (fp *FileProcessor) ProcessFile(filePath string) error {
    _, err := fp.limiter.ScheduleWithOptions(func() (interface{}, error) {
        // Determine file size for weight calculation
        stat, err := os.Stat(filePath)
        if err != nil {
            return nil, err
        }
        
        // Read and process file
        data, err := ioutil.ReadFile(filePath)
        if err != nil {
            return nil, err
        }
        
        // Simulate processing time based on file size
        processingTime := time.Duration(len(data)/1024) * time.Millisecond
        time.Sleep(processingTime)
        
        fmt.Printf("Processed file: %s (%d bytes)\n", filePath, len(data))
        return nil, nil
    }, 5, fp.getFileWeight(filePath)) // Priority 5, weight based on file size
    
    return err
}

func (fp *FileProcessor) getFileWeight(filePath string) int {
    stat, err := os.Stat(filePath)
    if err != nil {
        return 1
    }
    
    // Weight based on file size (MB)
    weight := int(stat.Size() / (1024 * 1024))
    if weight < 1 {
        weight = 1
    }
    if weight > 10 {
        weight = 10 // Cap at weight 10
    }
    
    return weight
}

func (fp *FileProcessor) ProcessDirectory(dirPath string) error {
    return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        if !info.IsDir() {
            return fp.ProcessFile(path)
        }
        
        return nil
    })
}

func (fp *FileProcessor) Close() {
    fp.limiter.Stop()
}
```

### 3. Web Scraping with Rate Limits

Respectful web scraping that doesn't overwhelm target servers:

```go
package main

import (
    "fmt"
    "io/ioutil"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type WebScraper struct {
    limiter *gothrottle.Limiter
    client  *http.Client
}

func NewWebScraper() *WebScraper {
    // Respectful scraping limits
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 3,     // Max 3 concurrent requests
        MinTime:       2 * time.Second, // 2 seconds between requests
    })
    
    return &WebScraper{
        limiter: limiter,
        client:  &http.Client{Timeout: 30 * time.Second},
    }
}

func (ws *WebScraper) ScrapeURL(url string) (string, error) {
    result, err := ws.limiter.Schedule(func() (interface{}, error) {
        resp, err := ws.client.Get(url)
        if err != nil {
            return nil, err
        }
        defer resp.Body.Close()
        
        body, err := ioutil.ReadAll(resp.Body)
        if err != nil {
            return nil, err
        }
        
        fmt.Printf("Scraped: %s (%d bytes)\n", url, len(body))
        return string(body), nil
    })
    
    if err != nil {
        return "", err
    }
    
    return result.(string), nil
}

func (ws *WebScraper) ScrapeMultipleURLs(urls []string) []string {
    results := make([]string, len(urls))
    
    for i, url := range urls {
        content, err := ws.ScrapeURL(url)
        if err != nil {
            fmt.Printf("Error scraping %s: %v\n", url, err)
            continue
        }
        results[i] = content
    }
    
    return results
}

func (ws *WebScraper) Close() {
    ws.limiter.Stop()
}
```

### 4. Background Job Processing

Throttle background jobs to prevent resource exhaustion:

```go
package main

import (
    "fmt"
    "sync"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type JobType int

const (
    EmailJob JobType = iota
    ReportJob
    DataSyncJob
    ImageProcessingJob
)

type Job struct {
    ID       string
    Type     JobType
    Data     interface{}
    Priority int
}

type JobProcessor struct {
    limiter *gothrottle.Limiter
}

func NewJobProcessor() *JobProcessor {
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 10,    // Max 10 concurrent jobs
        MinTime:       50 * time.Millisecond, // 50ms between job starts
    })
    
    return &JobProcessor{limiter: limiter}
}

func (jp *JobProcessor) ProcessJob(job Job) error {
    priority := job.Priority
    weight := jp.getJobWeight(job.Type)
    
    _, err := jp.limiter.ScheduleWithOptions(func() (interface{}, error) {
        return jp.executeJob(job)
    }, priority, weight)
    
    return err
}

func (jp *JobProcessor) getJobWeight(jobType JobType) int {
    switch jobType {
    case EmailJob:
        return 1 // Light operation
    case ReportJob:
        return 3 // Medium operation
    case DataSyncJob:
        return 5 // Heavy operation
    case ImageProcessingJob:
        return 8 // Very heavy operation
    default:
        return 1
    }
}

func (jp *JobProcessor) executeJob(job Job) (interface{}, error) {
    start := time.Now()
    
    switch job.Type {
    case EmailJob:
        return jp.processEmail(job)
    case ReportJob:
        return jp.generateReport(job)
    case DataSyncJob:
        return jp.syncData(job)
    case ImageProcessingJob:
        return jp.processImage(job)
    }
    
    fmt.Printf("Job %s completed in %v\n", job.ID, time.Since(start))
    return nil, nil
}

func (jp *JobProcessor) processEmail(job Job) (interface{}, error) {
    time.Sleep(100 * time.Millisecond) // Simulate email sending
    fmt.Printf("Email sent: %s\n", job.ID)
    return "email_sent", nil
}

func (jp *JobProcessor) generateReport(job Job) (interface{}, error) {
    time.Sleep(2 * time.Second) // Simulate report generation
    fmt.Printf("Report generated: %s\n", job.ID)
    return "report_generated", nil
}

func (jp *JobProcessor) syncData(job Job) (interface{}, error) {
    time.Sleep(5 * time.Second) // Simulate data sync
    fmt.Printf("Data synced: %s\n", job.ID)
    return "data_synced", nil
}

func (jp *JobProcessor) processImage(job Job) (interface{}, error) {
    time.Sleep(10 * time.Second) // Simulate image processing
    fmt.Printf("Image processed: %s\n", job.ID)
    return "image_processed", nil
}

func (jp *JobProcessor) ProcessJobsConcurrently(jobs []Job) {
    var wg sync.WaitGroup
    
    for _, job := range jobs {
        wg.Add(1)
        go func(j Job) {
            defer wg.Done()
            if err := jp.ProcessJob(j); err != nil {
                fmt.Printf("Job %s failed: %v\n", j.ID, err)
            }
        }(job)
    }
    
    wg.Wait()
}

func (jp *JobProcessor) Close() {
    jp.limiter.Stop()
}
```

### 5. Microservices Communication Throttling

Rate limit calls between microservices:

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

type ServiceClient struct {
    limiter    *gothrottle.Limiter
    baseURL    string
    httpClient *http.Client
}

func NewServiceClient(serviceName, baseURL string) *ServiceClient {
    // Use Redis for distributed rate limiting across service instances
    rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    store, _ := gothrottle.NewRedisStore(rdb)
    
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        ID:            fmt.Sprintf("service-client-%s", serviceName),
        MaxConcurrent: 20,    // Max 20 concurrent calls to this service
        MinTime:       10 * time.Millisecond, // 10ms between calls
        Datastore:     store,
    })
    
    return &ServiceClient{
        limiter:    limiter,
        baseURL:    baseURL,
        httpClient: &http.Client{Timeout: 30 * time.Second},
    }
}

func (sc *ServiceClient) Get(endpoint string) (*http.Response, error) {
    result, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        url := sc.baseURL + endpoint
        return sc.httpClient.Get(url)
    }, 5, 1) // Normal priority, weight 1
    
    if err != nil {
        return nil, err
    }
    
    return result.(*http.Response), nil
}

func (sc *ServiceClient) Post(endpoint string, data interface{}) (*http.Response, error) {
    result, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        jsonData, err := json.Marshal(data)
        if err != nil {
            return nil, err
        }
        
        url := sc.baseURL + endpoint
        return sc.httpClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
    }, 6, 2) // Higher priority, weight 2 (POST is heavier)
    
    if err != nil {
        return nil, err
    }
    
    return result.(*http.Response), nil
}

func (sc *ServiceClient) BulkOperation(endpoint string, items []interface{}) error {
    _, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        // Bulk operations are heavy and should have high priority
        jsonData, err := json.Marshal(items)
        if err != nil {
            return nil, err
        }
        
        url := sc.baseURL + endpoint
        resp, err := sc.httpClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
        if err != nil {
            return nil, err
        }
        defer resp.Body.Close()
        
        return resp, nil
    }, 10, 5) // Highest priority, weight 5 (very heavy operation)
    
    return err
}

func (sc *ServiceClient) Close() {
    sc.limiter.Stop()
}

// Example usage in a microservice
func main() {
    userService := NewServiceClient("user-service", "http://user-service:8080")
    orderService := NewServiceClient("order-service", "http://order-service:8080")
    
    defer userService.Close()
    defer orderService.Close()
    
    // These calls will be rate limited
    userResp, _ := userService.Get("/api/users/123")
    orderResp, _ := orderService.Post("/api/orders", map[string]interface{}{
        "user_id": 123,
        "amount":  99.99,
    })
    
    fmt.Printf("User response status: %d\n", userResp.StatusCode)
    fmt.Printf("Order response status: %d\n", orderResp.StatusCode)
}
```

### 6. ETL Pipeline Rate Limiting

Control data extraction, transformation, and loading processes:

```go
package main

import (
    "database/sql"
    "fmt"
    "time"
    
    "github.com/AFZidan/gothrottle"
    _ "github.com/lib/pq"
)

type ETLPipeline struct {
    extractLimiter   *gothrottle.Limiter
    transformLimiter *gothrottle.Limiter
    loadLimiter      *gothrottle.Limiter
    sourceDB         *sql.DB
    targetDB         *sql.DB
}

func NewETLPipeline(sourceDB, targetDB *sql.DB) *ETLPipeline {
    // Different rate limits for different stages
    extractLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 5,  // Limit source DB queries
        MinTime:       20 * time.Millisecond,
    })
    
    transformLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 10, // CPU-intensive, but can be parallel
        MinTime:       10 * time.Millisecond,
    })
    
    loadLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 3,  // Limit target DB writes
        MinTime:       50 * time.Millisecond,
    })
    
    return &ETLPipeline{
        extractLimiter:   extractLimiter,
        transformLimiter: transformLimiter,
        loadLimiter:      loadLimiter,
        sourceDB:         sourceDB,
        targetDB:         targetDB,
    }
}

func (etl *ETLPipeline) ExtractData(query string) ([]map[string]interface{}, error) {
    result, err := etl.extractLimiter.Schedule(func() (interface{}, error) {
        rows, err := etl.sourceDB.Query(query)
        if err != nil {
            return nil, err
        }
        defer rows.Close()
        
        // Process rows into data structure
        var data []map[string]interface{}
        // ... row processing logic
        
        fmt.Printf("Extracted %d records\n", len(data))
        return data, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    return result.([]map[string]interface{}), nil
}

func (etl *ETLPipeline) TransformData(data []map[string]interface{}) ([]map[string]interface{}, error) {
    result, err := etl.transformLimiter.Schedule(func() (interface{}, error) {
        // Simulate data transformation
        time.Sleep(100 * time.Millisecond)
        
        var transformed []map[string]interface{}
        for _, record := range data {
            // Transform each record
            transformedRecord := make(map[string]interface{})
            for k, v := range record {
                transformedRecord[k+"_transformed"] = v
            }
            transformed = append(transformed, transformedRecord)
        }
        
        fmt.Printf("Transformed %d records\n", len(transformed))
        return transformed, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    return result.([]map[string]interface{}), nil
}

func (etl *ETLPipeline) LoadData(data []map[string]interface{}) error {
    _, err := etl.loadLimiter.Schedule(func() (interface{}, error) {
        tx, err := etl.targetDB.Begin()
        if err != nil {
            return nil, err
        }
        defer tx.Rollback()
        
        for _, record := range data {
            // Insert transformed record
            // ... insert logic
        }
        
        err = tx.Commit()
        if err != nil {
            return nil, err
        }
        
        fmt.Printf("Loaded %d records\n", len(data))
        return nil, nil
    })
    
    return err
}

func (etl *ETLPipeline) ProcessBatch(query string) error {
    // Extract -> Transform -> Load pipeline
    data, err := etl.ExtractData(query)
    if err != nil {
        return err
    }
    
    transformedData, err := etl.TransformData(data)
    if err != nil {
        return err
    }
    
    return etl.LoadData(transformedData)
}

func (etl *ETLPipeline) Close() {
    etl.extractLimiter.Stop()
    etl.transformLimiter.Stop()
    etl.loadLimiter.Stop()
}
```
