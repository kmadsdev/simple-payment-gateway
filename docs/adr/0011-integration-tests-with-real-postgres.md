# 0011. Integration tests against a real PostgreSQL

Status: accepted

## Context

The interesting bugs here live in the database: lock behavior, unique-index races, deferred triggers, `SKIP LOCKED`. Mocks of the store would not catch them. Options: Testcontainers-go, or a database URL given by the environment.

## Decision

- Integration tests read `DATABASE_URL`. Each test creates its own database, runs the migrations, and drops it at the end, so tests are isolated and can run in parallel packages.
- CI provides PostgreSQL 17 as a GitHub Actions service container and sets `REQUIRE_DB=1`, which turns "skipped because no database" into a failure. CI cannot go green by skipping.
- Locally, `scripts/test-in-docker.sh` (`make test-docker`) starts a throwaway PostgreSQL and a Go container on a private network, runs the suite with `-race`, and removes everything on exit. Only Docker is needed.
- Without `DATABASE_URL`, `go test ./...` runs the unit tests and skips the integration tests.

## Consequences

- No Testcontainers dependency and no Docker socket inside the tests.
- A developer with a local PostgreSQL can point `DATABASE_URL` at it and run `make test`.
