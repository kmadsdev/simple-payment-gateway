# Error codes

Every error is an RFC 7807 problem (`Content-Type: application/problem+json`):

```json
{
  "type": "https://github.com/kmadsdev/simple-payment-gateway/blob/main/docs/errors.md#invalid_state_transition",
  "title": "Operation not allowed in the current state",
  "status": 409,
  "detail": "capture is not allowed when payment is created",
  "instance": "/v1/payments/pay_e6a34b0457c29458068e/capture",
  "code": "invalid_state_transition",
  "request_id": "req_4540dabefa1893630015"
}
```

`code` is stable and safe to branch on. `type` links to the section below. `request_id` matches the `X-Request-Id` response header and the logs.

## malformed_request

400. The body is not valid JSON for the endpoint: unknown field, wrong type (for example a decimal amount), or trailing data. The detail never repeats the body.

## idempotency_key_required

400. A `POST` without an `Idempotency-Key` header, or a key longer than 255 characters.

## unauthenticated

401. Missing, malformed or unknown API key. Send `Authorization: Bearer sk_test_...`.

## not_found

404. The resource does not exist or belongs to another merchant. Also returned for simulator routes when the simulator is off.

## invalid_state_transition

409. The operation is not allowed in the current state, for example capture before authorize, or void after capture. See the state machine in the README.

## method_not_supported

409. The operation does not apply to the payment method, for example capture on a Pix payment.

## operation_in_progress

409. Another provider operation on the same payment is still running. Retry after it finishes. Not stored under the idempotency key.

## concurrent_update

409. The payment changed during the request. Read it again and retry.

## idempotency_request_in_progress

409. A request with the same `Idempotency-Key` is still running. Retry in a moment.

## body_too_large

413. The body is above 64 KiB.

## validation_error

422. A field is not valid. `field` names it. The detail never repeats a card number or CVV.

## invalid_amount

422. The amount is zero or negative.

## amount_too_high

422. A capture above the authorized amount, or a refund above what is still refundable.

## idempotency_key_reused

422. The `Idempotency-Key` was used before with a different method, path or body. Use a new key for a new operation.

## rate_limited

429. Too many requests for this merchant. Wait for the seconds in `Retry-After`.

## internal_error

500. Unexpected error. The `request_id` finds it in the logs.

## provider_unavailable

503. The (simulated) provider did not answer after the retry budget, or the circuit breaker is open. The payment did not change in the gateway. Retry after `Retry-After`. With a real provider, the outcome of a call that timed out would be unknown until reconciled (see ADR 0005).

## not_ready

503. Readiness probe only: the database is not reachable or the process is shutting down.
