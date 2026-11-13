# 0006. No database lock during provider calls

Status: accepted

## Context

The simple way to change a payment is: lock the row, call the provider, write the result, commit. That holds a row lock and a database connection for the whole network call. A slow provider then blocks other requests on the same payment and drains the connection pool.

## Decision

Authorize, capture, void and refund run in three phases:

1. Transaction: lock the payment row, dry-run the domain rule on a copy, insert a `payment_operations` row in status `pending`, commit.
2. No transaction: call the provider with the operation id as the idempotency key. Timeouts, retries and the circuit breaker apply here.
3. Transaction: lock the row again, apply the domain change, write events, ledger and outbox, mark the operation `succeeded` or `failed`, commit.

A partial unique index (`payment_id WHERE status = 'pending'`) allows one in-flight operation per payment. A second concurrent capture or refund gets `409 operation_in_progress`.

Phases 2 and 3 run on a context detached from the client (`context.WithoutCancel` plus a timeout). Once a money-moving call is sent, a client hang-up must not abort it, because the provider may already have done the work. Once the provider answered, the result must be recorded even if the client hung up. Both are tested.

Background jobs (expiry, provider sync) change a payment only when no operation is pending on it. They take the same row lock as phase 1, so an authorization cannot expire under an in-flight capture (tested).

All transactions run at PostgreSQL's default isolation, READ COMMITTED. Correctness comes from the row lock (`SELECT ... FOR UPDATE`), the version check in the `UPDATE`, the partial unique index and the check constraints, not from a stricter isolation level.

## Consequences

- Twelve concurrent refunds on one payment: some succeed, the others get `409` or `422`, and the refunded total never passes the captured amount (tested).
- If the process dies between phase 1 and 3, the operation stays `pending`. A maintenance job marks operations older than 2 minutes as `failed (abandoned)` so the payment is not blocked forever. In a real system the outcome at the provider would be unknown and would need reconciliation. That is listed under "what is not built".
- If the provider call fails after all retries, the operation is `failed`, the payment does not change, and the API returns `503 provider_unavailable` with `Retry-After`.
