# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

This release addresses an external audit of `v1.0.0`. Redis mode was not safe as
a strict concurrency limiter, shutdown had races, and the scheduler imposed an
undocumented throughput ceiling. `v1.0.0` also predates the panic recovery,
nil-task rejection and overweight-job rejection that were on `main`, so anyone
who installed `@latest` is missing those too.

Existing code continues to compile and run. Two behaviors change deliberately
(datastore ownership and negative-limit validation); both are listed under
Changed.

### Fixed

- **Rate spacing no longer dies with the lease that set it.** Start times were
  stored per lease token in a ZSET, and the reclamation path deleted a lease's
  start record along with its reservation — so a crashed holder's expiry handed
  the next job a free start, bypassing whatever `MinTime` was left. Worse, `Renew`
  and `Release` refreshed that ZSET with the *lease* TTL, knowing nothing about
  `MinTime`: with `LeaseTTL: 1s` and `MinTime: 45s`, a release left roughly two
  seconds protecting a forty-five second window, and once it expired spacing
  silently stopped applying. Released tokens were also never removed, so the ZSET
  grew with traffic.

  Spacing is now a single `last-start` value per limiter, written only on a
  successful admission, never touched by renewal, release or reclamation, with a
  lifetime derived from `MinTime` alone. Reclaiming an expired lease purges
  reservation state only. See "Spacing outlives reservations" in the README.
- **`RegisterDone` no longer shortens the legacy state TTL.** `Request` sized the
  TTL to outlast the spacing window; `RegisterDone` reset it to a flat 30 seconds.
  A `MinTime` of 40 seconds got an ~80 second TTL on admission and a 30 second one
  on completion, so the state expired with the window still open. Both scripts now
  extend an existing expiry and never reduce it, and `last_start_us` is preserved.
- **Lease keys share one Redis Cluster hash slot.** The multi-key Lua scripts
  addressed keys built from three separate prefixes, which is a cross-slot error on
  a Cluster client. All four keys for a limiter now carry one hash tag derived from
  a SHA-256 of the ID — derived, not the raw ID, because an ID containing braces
  would otherwise select its own tag and could be pointed at another limiter's
  slot.
- **`Renew` cannot revive a reclaimed lease.** It checked only that the token
  still existed in the lease hash, so a lease whose expiry had passed but whose
  purge had not yet run could be renewed back to life after another holder had
  taken the capacity. It now verifies the expiry score is still in the future.
- **Lease reclamation handles large expired sets, and reconciles orphans.**
  `ZRANGEBYSCORE` fed every expired token straight into `unpack()`, which fails
  once the set is large enough to exceed Lua's argument limit — reachable after a
  process dies holding many leases. Reclamation now runs in bounded batches, and
  a lease weight with no expiry entry (state this package did not write, such as
  a partial restore) is reconciled away instead of holding capacity forever.
- **A blocking datastore can no longer prevent shutdown.** The limiter called
  `Acquire`, `Renew` and `Release` with `context.Background()`, so a store that
  blocked on a network round trip held `Stop` open indefinitely. Capacity
  acquisition and renewal now use a limiter-owned context that `Stop` cancels, and
  `stopRenewal` cancels an in-flight renewal before waiting for its goroutine.
- **Work can no longer start after `Stop`.** The scheduler launched a worker as
  soon as the datastore granted capacity, without rechecking whether `Stop` had
  been called while that request was in flight. It now rechecks and releases the
  reservation instead of starting the task.
- **Concurrent `Stop` calls no longer return early.** A second caller saw
  `running=false` and returned immediately while the first was still waiting for
  workers. `Stop` is now a `sync.Once` state machine; every caller blocks until
  shutdown has completed and receives the same error.
- **Stopping one limiter no longer breaks others.** See Changed below.
- **Redis state no longer expires while a job is running.** Both Lua scripts set
  a fixed `PEXPIRE 30000` on a shared counter. With `MaxConcurrent: 1` and a job
  running 40 seconds, the key expired at 30s, a second job saw `running=0` and
  started over the limit, and the first job's late decrement then zeroed the
  second job's state so a third could start. The same expiry silently discarded
  any `MinTime` longer than 30 seconds. Capacity is now held by renewable
  tokenized leases (see Added).
- **Distributed timing no longer depends on each machine's clock.** The Redis
  script reads the clock with `TIME` instead of receiving `time.Now()` from the
  calling process, so a skewed clock cannot admit work early or impose an
  inflated wait on every other instance.
- **Sub-millisecond `MinTime` is honored in Redis.** It went through
  `.Milliseconds()` and truncated to zero, disabling spacing that `LocalStore`
  enforced. Durations are now microseconds end to end.
- **`RegisterDone` failures are no longer discarded.** They were dropped with
  `_ = err`, permanently inflating the store's running count. They are now
  retried and then reported through `OnError`.
- **`LocalStore` rejects an overweight job** with `ErrWeightExceedsMax` instead
  of returning `canRun=false` forever, which spins in the scheduler's requeue
  loop. It also validates limiter IDs, matching `RedisStore`.
- **Equal-priority jobs run FIFO.** `Less` compared only priority, and
  `container/heap` is not stable, so with the default priority of 5 nearly all
  jobs ran in arbitrary order.
- **CI actually tests Redis.** The Redis 6/7 matrix started service containers
  but never set `REDIS_ADDR`, so all three Redis tests skipped themselves while
  the matrix reported green. CI now sets `REDIS_ADDR`/`REQUIRE_REDIS` and fails
  if any Redis test skips.
- **`make docker-test` works.** `docker-compose.test.yml` referenced a
  `Dockerfile.test` that was never committed — the `*.test` gitignore pattern had
  swallowed it.
- **The release workflow's Go proxy warm-up** used `curl -x`, which sets a proxy
  to route through rather than requesting the URL.
- **Cross-platform builds set `CGO_ENABLED=0` explicitly.** Go only defaults it
  to 0 when cross-compiling if the environment has not set it, and the
  sqlite3-backed tests need `CGO_ENABLED=1` — so in the Docker quality gate the
  arm64 build handed an arm64 `runtime/cgo` to the host's x86 assembler and
  failed. The library itself has no cgo dependency.

### Added

- `LeaseDatastore` with `Acquire`/`Renew`/`Release` over individually identified
  reservations. Each lease carries a crypto-random token and its own expiry; the
  limiter renews every `LeaseTTL/3` while a task runs, so a long job keeps its
  capacity while a dead holder's lease lapses and is reclaimed. `Release` names
  one token, so a late release from an expired job cannot corrupt a newer
  holder's state. Both built-in stores implement it, and the limiter uses it
  automatically. The interface embeds `Datastore`, so a custom store implementing
  only `Request`/`RegisterDone` keeps working.
- **Same-ID configuration consistency.** The first acquisition for a limiter ID
  records its `MaxConcurrent`, `MinTime` and `LeaseTTL`; a later acquisition that
  disagrees is refused with `ErrLimiterConfigMismatch` naming both
  configurations. Without it, the effective distributed limit was whichever
  process reached Redis first. Local scheduler settings — `RetryInterval`,
  `MaxQueueSize`, `SchedPolicy`, `OnError` — are not compared, since they shape
  one process's queueing rather than what the store admits. A new configuration
  is accepted once every lease has lapsed and the spacing window has closed.
- `RedisKeys(id)` and `RedisStateKey(id)`, which name the keys a limiter ID
  occupies, for operational inspection or clearing state.
- `Options.LeaseTTL` — how long a reservation survives without renewal.
  Default 30s, 1s floor.
- `ScheduleContext` and `ScheduleWithOptionsContext`. A job cancelled while
  queued is removed from the queue and returns `ctx.Err()`; a job already running
  is awaited, since a task function cannot be interrupted.
- `Options.MaxQueueSize` with `ErrQueueFull`, replacing unbounded queue growth.
- `Options.OnError` for failures with no caller to return them to.
- `Options.SchedPolicy` with `SchedBestFit`, which lets lighter jobs use capacity
  a heavy high-priority job cannot fill yet. `SchedStrict` remains the default.
- `Options.RetryInterval` — how often to re-check a distributed store that
  refused capacity.
- `Options.Validate()` for checking configuration without constructing a limiter.
- `PanicError` carrying the panic value and the stack captured at recovery. It
  unwraps to `ErrTaskPanic`, so `errors.Is` checks keep working.
- `Limiter.QueueLen()` and `Limiter.Running()` for monitoring.
- `RedisStore.Close()`, which disconnects the store *and* closes the client, for
  when the store is the client's sole user.
- `PriorityQueue.Peek()` and `PriorityQueue.Remove()`.
- New sentinels: `ErrQueueFull`, `ErrLeaseLost`, `ErrNilLease`, `ErrNilClient`,
  `ErrLimiterConfigMismatch`, `ErrInvalidMaxConcurrent`, `ErrInvalidMinTime`,
  `ErrInvalidMaxQueueSize`, `ErrInvalidRetryInterval`, `ErrInvalidLeaseTTL`,
  `ErrInvalidSchedPolicy`.
- Adversarial tests: long jobs outliving the lease TTL, crashed holders, stale
  releases, server-side `MinTime` across two instances, shutdown during
  acquisition, shared stores, queue overflow, and throughput.
- Regression tests for the spacing model (`tests/spacing_test.go`), the legacy
  state TTL (`tests/legacy_state_test.go`), configuration agreement
  (`tests/config_consistency_test.go`), key layout and client types
  (`tests/redis_keys_test.go`) and shutdown cancellation
  (`tests/cancellation_test.go`).
- `make redis-up`, `make redis-down` and `make test-redis` for running the suite
  against a local Redis.

### Changed

- **`NewRedisStore` takes `redis.UniversalClient`.** `*redis.Client` satisfies it,
  so existing call sites compile unchanged; `*redis.ClusterClient`, `*redis.Ring`
  and the Sentinel failover client now work too. A typed nil such as
  `(*redis.Client)(nil)` is rejected with `ErrNilClient` rather than accepted and
  panicked on at first use.

  Cluster support status: the key scheme is cluster-slot ready and the constructor
  accepts a cluster client, but CI exercises standalone Redis 6 and 7 only. Treat
  standalone and Sentinel as supported and Cluster as untested.
- **An injected datastore is no longer closed by `Stop`.** `Stop` called
  `Datastore.Disconnect` unconditionally, and `RedisStore.Disconnect` closed the
  `*redis.Client` the caller had constructed, so stopping one limiter broke every
  other limiter sharing the store and every component sharing the client. `Stop`
  now disconnects only a store the limiter created for itself. Set
  `CloseDatastoreOnStop: true` to transfer ownership, or call `Disconnect()`
  yourself.

  If you relied on `Stop` closing your store or client, close it explicitly.
- **Negative `MaxConcurrent` and `MinTime` are rejected** by `NewLimiter` rather
  than being treated as unlimited. Zero still means "no limit".
- **Capacity release is bounded rather than unbounded.** A release must survive
  shutdown — the point is to hand the reservation back — so it does not use the
  cancellable operation context. Instead all its retries share a deadline of
  `max(LeaseTTL, 5s)`, after which the store reclaims the lease on its own; a
  store that never answers is reported through `OnError` instead of blocking
  `Stop`.
- **Lease scripts recover from `NOSCRIPT` with `EVAL`** rather than `SCRIPT LOAD`
  followed by a retry. `EVAL` caches on the node that serves the key, which is
  what makes recovery correct on a Cluster or Ring client where `SCRIPT LOAD`
  could reach a different node.
- **The scheduler is event-driven.** It woke every 10ms and started at most one
  job per tick, capping starts at roughly 100/second: 1000 trivial jobs took
  ~10s with no limits configured, and every idle limiter burned 100 wakeups per
  second. It now wakes on enqueue, on capacity release, and on real deadlines,
  and dispatches every job that fits. The same 1000 jobs complete in ~20ms and an
  idle limiter issues no datastore requests.
- **A datastore error fails its job instead of requeuing it**, and the scheduler
  backs off before touching the store again, so one outage cannot fail the whole
  queue in a tight loop. A cancellation caused by the limiter's own shutdown is
  reported as `ErrStoreClosed` rather than as a datastore fault.
- `NewRedisStore(nil)` returns `ErrNilClient` instead of `ErrStoreClosed`. It
  unwraps to `ErrStoreClosed` so existing checks still match.
- Linter and scanner versions are pinned (golangci-lint v1.59.1, gosec v2.20.0)
  in CI, the Makefile and the test image, so results no longer depend on when the
  job ran.
- The release workflow runs the suite against Redis before cutting a release, and
  uses `gh release create` in place of the archived `actions/create-release`.
- The README coverage badge points at Codecov, matching what CI uploads.

## [1.0.0] - 2025-07-03

### Initial Release

- Initial public release
