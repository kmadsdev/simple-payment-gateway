# 0008. Card data and secrets

Status: accepted

## Context

Card numbers (PAN) and security codes (CVV) are the most sensitive data a gateway touches. API keys and webhook secrets give access to the merchant's account.

## Decision

Cards:

- `POST /v1/card-tokens` validates the number (digits only after removing spaces and dashes, 12 to 19 digits, Luhn), the CVV (3 digits, 4 for Amex) and the expiry.
- The gateway keeps a random token, a brand guess from the number prefix, the last four digits and the expiry. The number and the CVV are never written to the database and never logged. The idempotency fingerprint of a tokenization request covers them, so it is a keyed HMAC, not a plain hash (see ADR 0005).
- Validation errors name the field, never the value. JSON decode errors return a fixed message, never the body.
- Handlers never log request bodies. The logger also redacts attribute names like `number`, `cvv` and `authorization` as a second line of defense.
- A test sends valid and invalid cards through the full HTTP stack with debug logs on, then searches the logs, the responses and every row of every table for the number and the CVV.
- Because the PAN is not kept, the simulator decides card outcomes by last four digits.

API keys:

- Format `sk_test_` plus 64 hex characters (256 random bits). The first 20 characters are a lookup prefix.
- Only the SHA-256 of the full key is stored, compared with `crypto/subtle.ConstantTimeCompare`. A slow hash (bcrypt) is not needed for a random 256-bit key, and it would add cost to every request.
- The key is printed once by `create-merchant` and never logged.

Webhook secrets:

- Stored in clear, because the gateway needs them to sign. A production system would encrypt them with a KMS key.

## Consequences

- The API still receives the PAN in a request body, so a real deployment would be in PCI DSS scope. This is listed under "what is not built".
