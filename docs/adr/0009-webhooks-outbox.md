# 0009. Webhooks through a transactional outbox

Status: accepted

## Context

After a state change, the merchant needs a notification. Sending the HTTP call inside the database transaction couples the transaction to the merchant's endpoint. Sending it after the commit loses the event if the process dies in between.

## Decision

- The transaction that changes a payment also inserts one `outbox` row per transition, with the full event payload.
- A dispatcher claims due rows with `FOR UPDATE SKIP LOCKED` and pushes `next_attempt_at` forward by a lease, then commits. No lock is held during the HTTP call. Several dispatchers can run side by side.
- Each attempt is written to `webhook_deliveries` (status code, error, duration).
- A non-2xx answer or a network error schedules a retry with exponential backoff and full jitter. After `WEBHOOK_MAX_ATTEMPTS` the message is `failed`.
- The HTTP client has a 10 second timeout and does not follow redirects: a 3xx is a failed attempt. Following redirects would let an endpoint send the gateway, with a signed payload, to another host (tested).
- Shutdown drains: the delivery in flight finishes and is recorded, and no new one starts. Messages claimed but not tried return to the queue when their lease ends (tested).
- Headers: `Webhook-Id` (the event id) and `Webhook-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "t.body")>`. The receiver checks the HMAC in constant time and rejects timestamps older than 5 minutes (replay window). Several `v1` values are accepted, which allows secret rotation.
- `examples/webhook-receiver` is a standard-library receiver that verifies signatures and drops duplicate event ids. Its test cross-checks it against the gateway's signer.

## Consequences

- Delivery is at least once. A merchant can receive the same event twice and must de-duplicate by the event id. Use the `id` inside the signed body: the `Webhook-Id` header carries the same value but is not covered by the signature.
- SSRF: webhook URLs are set only by the operator (`create-merchant`), never through the API, and they are not checked against private or loopback addresses, because the local demo posts to a container on a private network. A gateway that lets merchants set their URL must resolve the host and refuse private, loopback and link-local ranges at delivery time.
- If a delivery takes longer than the lease (a full batch of slow endpoints), another dispatcher can claim the same message. That is a duplicate, which at-least-once already allows.
- Order is best effort (oldest first per batch). After a retry, a newer event can arrive before an older one. The payload carries the full payment, so the merchant can always act on the latest state.
