# 0007. Provider port, deterministic simulator, retry budget and circuit breaker

Status: accepted

## Context

This project does not talk to any real acquirer or PSP. I still want the code shaped so a real adapter could replace the fake one, and I want to exercise declines, timeouts and delayed approvals on purpose.

## Decision

- `acquirer.Acquirer` is the port: `Authorize`, `Capture`, `Void`, `Refund`, `Status`. Every method takes a context.
- `acquirer.Simulator` is the only adapter. It is in memory and deterministic: the cents part of the amount and the card's last four digits select the outcome (see the README table). It honors idempotency keys.
- `acquirer.Resilient` wraps any adapter:
  - a timeout per attempt, and a total time budget per call;
  - retries only on timeouts or unavailability, never on a decline or a business error, with exponential backoff and full jitter;
  - a circuit breaker that opens after N consecutive failures, fails fast while open, and lets one trial call through after a cooldown.
- Pix: authorize returns `pending` with a reference. The simulator endpoint marks the charge paid, and the gateway reads the new status through `Status`. A maintenance loop also polls pending payments, which covers delayed card approvals.

## Consequences

- Tests trigger every path with an amount, without mocks.
- Simulator state is lost on restart, so a pending Pix charge created before a restart cannot be confirmed. It expires.
- Retries are safe only because the provider call carries an idempotency key. A real adapter must keep that property.
