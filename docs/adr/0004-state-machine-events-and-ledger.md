# 0004. Explicit state machine, append-only events, double-entry ledger

Status: accepted

## Context

A payment moves through states, and some moves must never happen: capture after void, refund before capture, a second capture. I also need to answer "how did this payment get here?" and "do the amounts add up?" at any time.

## Decision

- `internal/payment` holds a transition table. Every operation checks it and returns a `TransitionError` for an illegal move. The HTTP layer maps it to `409 invalid_state_transition`.
- The domain has no I/O. An operation returns a `Transition` (from, to, amount, ledger journals). The store writes it in one transaction.
- `payment_events`: one row per transition. A trigger blocks `UPDATE` and `DELETE`.
- `ledger_entries`: double-entry lines on three accounts per payment (`customer`, `hold`, `merchant`). Debits are positive, credits negative. Each journal sums to zero, and a deferred constraint trigger rejects an unbalanced journal at commit. A trigger blocks `UPDATE` and `DELETE`.
- Check constraints on `payments`: `0 <= refunded <= captured <= authorized <= amount`.
- One capture per payment. A partial capture releases the rest of the hold in the same transaction.

## Consequences

- The invariants are checked in three places: in the domain tests (random sequences in memory), in the service tests (random sequences in PostgreSQL), and by the database itself.
- A correction is a new journal, never an edit. This is how accounting systems work, and it keeps history auditable.
- The payment row keeps denormalized totals for fast reads. The tests check that they always match the ledger.
