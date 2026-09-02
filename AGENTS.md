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
- `bounds.go` — numeric policy: what is rejected, what saturates
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
- **One validator, every path.** `validateAdmission` runs `Options.Validate()` in
  full, so `NewLimiter`, `Request` and `Acquire` reject exactly the same things,
  before any token, Redis command, local state, configuration registration or TTL
  change. A store-specific subset was the previous state, and it let a direct
  caller pass a negative `MaxConcurrent` that the `> 0` guards then read as
  "unlimited".
- **Nothing above 2^53-1 reaches Lua.** `MaxConcurrent`, weights, and
  `MinTime`/`LeaseTTL` as microseconds are rejected with `ErrValueOutOfRange`
  past that boundary, because Lua compares IEEE-754 doubles. Arithmetic that
  survives validation saturates (see `bounds.go`): a wrapped negative reads as
  free capacity, or as a window shorter than the one it was sizing.

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
- **The expiry ZSET is authoritative for liveness**, so the sum walks it and reads
  each token's weight rather than summing the hash. A weight with no expiry entry
  is therefore never counted, and is removed; an expiry entry with no weight is
  removed too. Comparing `HLEN` to `ZCARD` alone was the earlier form, and it
  missed equal-cardinality corruption — hash holding token A, ZSET holding token
  B, both size 1, token A holding capacity forever.
- **Renewal checks the expiry score, not just the hash field.** A lease whose
  expiry has passed must not be revivable, because the capacity may already
  belong to someone else.
- **All four keys per limiter share one hash tag**, derived by hashing the ID —
  never the raw ID, which could contain braces and choose its own tag. Keys come
  from `newLimiterKeys` only.
- **Collections stay bounded**, and every multi-member operation — expiry
  reclamation, the membership walk, orphan deletion — runs in batches so
  `unpack()` cannot exceed Lua's argument limit.
- **An unlimited limiter does not pay for membership reconciliation.** With
  `MaxConcurrent` 0 the lease hash is unbounded and no orphan can consume
  capacity, so reconciliation runs only when the O(1) cardinality check
  disagrees. The README states the narrower guarantee; do not widen the claim
  without widening the work.
- **Configuration agreement is decided before any write**, so a rejected client
  cannot disturb the leases, TTLs or spacing state it disagreed about.

## Release process

Releases run only through `workflow_dispatch` on `.github/workflows/release.yml`.
Verification — commit ancestry, existing-tag agreement, the commit's own CI, the
race suite against Redis with `REQUIRE_REDIS=true`, cross-builds — happens *before*
the tag exists, and the tag and release are created only after it passes. Re-runs
reuse an existing tag at the same commit and inspect an existing release rather
than failing, so the Go proxy warm-up is reached either way.

Two rules that are load-bearing: a tag pointing at a different commit than
requested is an error, never moved; and publication must not be reachable without
verification. The old workflow was tag-triggered, so the tag — and the resolvable
module version — already existed by the time the tests ran. `v1.1.0`'s Release run
is red for this reason and is deliberately left alone.

## Cancellation model

The limiter owns a context cancelled by `Stop`, passed to `Acquire` and `Renew`
so a blocking store cannot hold shutdown open. `Release` deliberately does not
use it — capacity has to come back — and instead has its own
`max(LeaseTTL, 5s)` budget across all retries. A shutdown-cancelled acquisition
surfaces to the caller as `ErrStoreClosed`, not as a datastore error.
