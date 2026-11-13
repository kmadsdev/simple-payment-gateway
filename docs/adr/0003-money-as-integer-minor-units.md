# 0003. Money as integer minor units

Status: accepted

## Context

Binary floating point cannot represent most decimal amounts exactly (`0.1 + 0.2 != 0.3`). Rounding errors in a payment system turn into reconciliation differences.

## Decision

- Every amount is `money.Amount`, an `int64` number of centavos. `1050` means R$ 10,50.
- The API takes and returns integers. A decimal like `10.5` in JSON fails to decode into `int64` and returns `400`.
- Database columns are `bigint`.
- `money.Format` and `money.Parse` convert to and from the `"10.50"` form, used only inside the Pix payload. Both are fuzz tested (round trip, no panics, overflow).
- Only BRL is accepted.

## Guard

`internal/archtest` parses every non-test Go file and fails on `float32`, `float64` or a float literal, except in `internal/observability` (the Prometheus client takes seconds as `float64`; no money goes there). A second test fails on `numeric`, `real`, `double precision`, `float` or `money` columns in the migrations.

## Consequences

- No rounding inside the gateway. Splits and fees, if added, need an explicit rounding rule.
- `int64` centavos cover amounts far beyond any single payment. The service caps a payment at R$ 1.000.000,00.
