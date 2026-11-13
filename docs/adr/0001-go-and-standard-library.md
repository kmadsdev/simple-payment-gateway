# 0001. Go and the standard library router

Status: accepted

## Context

A payment gateway is I/O bound: it waits on the database, on the provider and on merchant webhook endpoints. Every wait needs a deadline, and a cancelled request must stop its downstream calls. I also want a small surface of third-party code in a service that handles money.

## Decision

- Go 1.27. `context.Context` flows through every call that does I/O.
- `net/http` with Go 1.22+ method and path patterns (`POST /v1/payments/{id}/capture`). No web framework.
- `log/slog` for logs, `crypto/*` for hashing and signatures.
- Runtime dependencies: `pgx`, `goose`, the Prometheus client. `kin-openapi` only in tests.

## Consequences

- One static binary in a distroless, non-root image.
- Middleware is plain function wrapping. The route table (`httpapi.Routes`) is data, so a test can compare it with the OpenAPI spec.
- I write a few small things myself (rate limiter, circuit breaker, retry). Each is under 100 lines and has its own tests. A larger system could use proven libraries for these.
