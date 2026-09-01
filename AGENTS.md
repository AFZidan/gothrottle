# GoThrottle

A Go rate limiting and request throttling library, inspired by the Node.js
`bottleneck` package. Package root is the library itself; tests live in `tests/`
as an external `gothrottle_test` package.

## Commands

```bash
make dev            # Format, vet, test
make test-redis     # Start Redis via Docker, run the suite against it
make quality        # Format check, vet, lint, security scan
make ci             # Full local CI simulation
make docker-test    # Full quality gate in Docker with Redis
```

Redis-backed tests read `REDIS_ADDR` and skip when it is unset. Set
`REQUIRE_REDIS=true` to turn that skip into a failure — CI does this so the Redis
matrix cannot pass without exercising Redis. Always run the suite against a real
Redis before claiming Redis changes work:

```bash
REDIS_ADDR=localhost:6379 REQUIRE_REDIS=true go test -race ./tests/...
```

## Layout

- `limiter.go` — scheduler, dispatch, shutdown, worker lifecycle
- `datastore.go` / `lease.go` — the two store contracts
- `local_store.go` / `local_lease.go` — in-memory store
- `redis_store.go` / `redis_lease.go` — Redis store and Lua scripts
- `job.go` — job type and priority queue
- `options.go` — configuration and validation
- `errors.go` — sentinels and `PanicError`

## Constraints

- Go 1.19 is the floor (`go.mod`), and CI builds 1.19–1.22. No newer stdlib.
- `go-redis/v8`, not v9.
- Tool versions are pinned in three places that must stay in sync:
  `.github/workflows/ci.yml`, `Makefile`, `Dockerfile.test`.
- `Datastore` and `LeaseDatastore` are exported and user-implementable. Adding a
  method to either is a breaking change for downstream implementations;
  `LeaseDatastore` was added by embedding rather than modifying `Datastore` for
  this reason.
- Timing decisions in Redis mode come from `TIME` inside Lua, never from
  `time.Now()` in the client. Durations cross the boundary as microseconds.

## Redis invariants

These are the rules the Lua scripts exist to enforce. Each one was a real defect
before it was a rule, so changing a script means re-reading this list.

- **Rate spacing is not lease state.** `MinTime` is measured from a job's start,
  so `last-start` is written only on a successful admission and is never deleted
  or refreshed by renewal, release or expired-lease reclamation. Its lifetime
  derives from `MinTime` alone. Coupling the two let a crashed holder's expiry
  grant the next job a free start, and let a 1s lease TTL shorten a 45s window.
- **Reclamation touches reservations only** — the lease hash and the expiry
  ZSET, never `last-start`.
- **TTLs are extended, never shortened.** Every script goes through its
  `ensure_ttl` helper. A release holding a short lease TTL must not cut short a
  key protecting a long `MinTime`; that asymmetry is exactly what broke
  `RegisterDone` on the legacy path.
- **Running weight is summed from live leases**, not tracked in a counter, so a
  late release cannot corrupt the total. It is bounded by `MaxConcurrent`.
- **Renewal checks the expiry score, not just the hash field.** A lease whose
  expiry has passed must not be revivable, because the capacity may already
  belong to someone else.
- **All four keys per limiter share one hash tag**, derived by hashing the ID —
  never the raw ID, which could contain braces and choose its own tag. Keys come
  from `newLimiterKeys` only.
- **Collections stay bounded**, and expired tokens are reclaimed in batches so
  `unpack()` cannot exceed Lua's argument limit.
- **Configuration agreement is decided before any write**, so a rejected client
  cannot disturb the leases, TTLs or spacing state it disagreed about.

## Cancellation model

The limiter owns a context cancelled by `Stop`, passed to `Acquire` and `Renew`
so a blocking store cannot hold shutdown open. `Release` deliberately does not
use it — capacity has to come back — and instead has its own
`max(LeaseTTL, 5s)` budget across all retries. A shutdown-cancelled acquisition
surfaces to the caller as `ErrStoreClosed`, not as a datastore error.
