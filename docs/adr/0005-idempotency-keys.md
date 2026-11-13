# 0005. Idempotency keys

Status: accepted

## Context

Networks fail after the server has done the work. A store that retries "capture" or "refund" must not charge or refund twice.

## Decision

- Every `POST` needs an `Idempotency-Key` header (1 to 255 characters). Without it: `400`.
- Table `idempotency_keys` with primary key `(merchant_id, key)`, a fingerprint of the request, a status, and the stored response. The fingerprint is HMAC-SHA256 of method, path and body, keyed with the caller's API key. A plain SHA-256 would be unsafe here: the body of `POST /v1/card-tokens` holds a card number and a CVV, which have little entropy, so a plain hash in a database copy could be brute-forced. The API key is never stored, so the database alone cannot do that (tested).
- Claim: `INSERT ... ON CONFLICT DO NOTHING`. The primary key makes the claim atomic, so two concurrent duplicates cannot both run.
- Same key, same request, finished: replay the stored status and body with `Idempotent-Replayed: true`.
- Same key, still running: `409 idempotency_request_in_progress`.
- Same key, different request: `422 idempotency_key_reused`.
- 2xx and 4xx responses are stored. 5xx and 409 responses release the key, because they mean "nothing changed in the gateway, try again". A `503 provider_unavailable` after a timeout is the weak spot: the gateway did not change, but with a real provider the outcome of the timed-out call would be unknown, and a retry runs a new operation with a new provider key. Closing that gap needs reconciliation (see "What is not built" in the README).
- Keys expire after 24 hours. Claims stuck in progress (crash) are removed after 5 minutes.

## Consequences

- A test sends the same request from two goroutines at once, 20 times, against PostgreSQL, and checks that exactly one payment exists per key.
- The response is buffered in memory before it is stored. Responses here are small.
- The provider side is idempotent too, within one operation: each provider call carries the operation id as its key, so the decorator's retry loop cannot duplicate a capture at the provider.
