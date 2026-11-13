# 0002. Plain pgx and hand-written SQL, goose migrations

Status: accepted

## Context

The important queries are about locking and concurrency: `SELECT ... FOR UPDATE`, `INSERT ... ON CONFLICT DO NOTHING`, `FOR UPDATE SKIP LOCKED`, a version check in `UPDATE ... WHERE version = $n`. I want them visible in the code, not generated or hidden behind an ORM.

Options: an ORM, `sqlc` (type-safe code generated from SQL), or plain `pgx`.

## Decision

Plain `pgx/v5` with hand-written SQL in `internal/store`. Migrations are SQL files run by `goose`, embedded in the binary, with a PostgreSQL session lock so several replicas can start at once.

## Consequences

- Every lock and every transaction boundary is explicit and easy to review.
- No code generation step in the build or in CI.
- Scanning is manual. The column list of a payment lives in one constant (`paymentCols`) with one scan function to keep it in one place.
- With more queries, `sqlc` would pay off: it checks queries against the schema at build time. Switching later only touches `internal/store`.
