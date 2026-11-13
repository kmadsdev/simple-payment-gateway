# 0009. Webhooks through a transactional outbox

Status: accepted

## Context

After a state change, the merchant needs a notification. Sending the HTTP call inside the database transaction couples the transaction to the merchant's endpoint. Sending it after the commit loses the event if the process dies in between.

## Decision

- The transaction that changes a payment also inserts one `outbox` row per transition, with the full event payload.
- A dispatcher claims due rows with `FOR UPDATE SKIP LOCKED` and pushes `next_attempt_at` forward by a lease, then commits. No lock is held during the HTTP call. Several dispatchers can run side by side.
- Each attempt is written to `webhook_deliveries` (status code, error, duration).
- A non-2xx answer or a network error schedules a retry with exponential backoff and full jitter. After `WEBHOOK_MAX_ATTEMPTS` the message is `failed`.
- Headers: `Webhook-Id` (the event id) and `Webhook-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "t.body")>`. The receiver checks the HMAC in constant time and rejects timestamps older than 5 minutes (replay window). Several `v1` values are accepted, which allows secret rotation.
- `examples/webhook-receiver` is a standard-library receiver that verifies signatures and drops duplicate event ids. Its test cross-checks it against the gateway's signer.

## Consequences

- Delivery is at least once. A merchant can receive the same event twice and must de-duplicate by `Webhook-Id`.
- Order is best effort (oldest first per batch). After a retry, a newer event can arrive before an older one. The payload carries the full payment, so the merchant can always act on the latest state.
