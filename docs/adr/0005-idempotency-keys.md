# 0005. Idempotency keys

Status: accepted

## Context

Networks fail after the server has done the work. A store that retries "capture" or "refund" must not charge or refund twice.

## Decision

- Every `POST` needs an `Idempotency-Key` header (1 to 255 characters). Without it: `400`.
- Table `idempotency_keys` with primary key `(merchant_id, key)`, the SHA-256 of method, path and body, a status, and the stored response.
- Claim: `INSERT ... ON CONFLICT DO NOTHING`. The primary key makes the claim atomic, so two concurrent duplicates cannot both run.
- Same key, same request, finished: replay the stored status and body with `Idempotent-Replayed: true`.
- Same key, still running: `409 idempotency_request_in_progress`.
- Same key, different request: `422 idempotency_key_reused`.
- 2xx and 4xx responses are stored. 5xx and 409 responses release the key, because they mean "nothing changed, try again".
- Keys expire after 24 hours. Claims stuck in progress (crash) are removed after 5 minutes.

## Consequences

- A test sends the same request from two goroutines at once, 20 times, against PostgreSQL, and checks that exactly one payment exists per key.
- The response is buffered in memory before it is stored. Responses here are small.
- The provider side is idempotent too: each provider call carries the operation id as its key, so the retry loop cannot duplicate a capture at the provider.
