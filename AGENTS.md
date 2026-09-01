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
