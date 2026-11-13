# 0010. Per-merchant rate limit (GCRA, in memory)

Status: accepted

## Context

One merchant with a bug (a retry loop, for example) must not slow down the others.

## Decision

- A per-merchant limiter after authentication, based on GCRA (generic cell rate algorithm). It keeps one timestamp per merchant and uses only integer time math. It allows `RATE_LIMIT_BURST` requests at once and `RATE_LIMIT_RPS` on average.
- Over the limit: `429 rate_limited` with `Retry-After` in seconds.

## Consequences

- The state is per process. With N replicas the effective limit is N times higher. A shared limit would need Redis (or similar) with the same algorithm.
- The map grows with the number of merchants, which is small here.
