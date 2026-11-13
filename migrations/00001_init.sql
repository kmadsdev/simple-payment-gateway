-- +goose Up
-- Money columns are bigint minor units (centavos). No numeric, real or
-- double precision column exists: internal/archtest checks this file.

CREATE TABLE merchants (
    id              text PRIMARY KEY,
    name            text NOT NULL,
    api_key_prefix  text NOT NULL UNIQUE,
    api_key_hash    bytea NOT NULL,           -- SHA-256 of the full key
    webhook_url     text,
    webhook_secret  text NOT NULL,            -- needed in clear to sign; see ADR 0008
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Only the token, a brand guess, the last four digits and the expiry are
-- kept. The full card number and the security code are never stored.
CREATE TABLE card_tokens (
    token        text PRIMARY KEY,
    merchant_id  text NOT NULL REFERENCES merchants(id),
    brand        text NOT NULL,
    last4        char(4) NOT NULL,
    exp_month    smallint NOT NULL CHECK (exp_month BETWEEN 1 AND 12),
    exp_year     smallint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payments (
    id                        text PRIMARY KEY,
    merchant_id               text NOT NULL REFERENCES merchants(id),
    amount                    bigint NOT NULL CHECK (amount > 0),
    currency                  char(3) NOT NULL CHECK (currency = 'BRL'),
    method                    text NOT NULL CHECK (method IN ('card', 'pix')),
    status                    text NOT NULL CHECK (status IN ('created', 'pending', 'authorized', 'captured',
                                  'partially_refunded', 'refunded', 'voided', 'expired', 'declined')),
    authorized_amount         bigint NOT NULL DEFAULT 0,
    captured_amount           bigint NOT NULL DEFAULT 0,
    refunded_amount           bigint NOT NULL DEFAULT 0,
    card_token                text REFERENCES card_tokens(token),
    card_brand                text,
    card_last4                char(4),
    pix_payload               text,
    pending_expires_at        timestamptz,          -- pix charge expiry or delayed-approval timeout
    authorization_expires_at  timestamptz,
    provider_ref              text,
    decline_code              text,
    description               text,
    reference                 text,
    version                   integer NOT NULL DEFAULT 1,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT amounts_ordered CHECK (
        0 <= refunded_amount AND refunded_amount <= captured_amount
        AND captured_amount <= authorized_amount AND authorized_amount <= amount),
    CONSTRAINT card_needs_token CHECK (method <> 'card' OR card_token IS NOT NULL)
);
CREATE INDEX payments_merchant_page ON payments (merchant_id, created_at DESC, id DESC);
CREATE INDEX payments_sweep ON payments (status) WHERE status IN ('pending', 'authorized');

-- One row per call to the provider. The partial unique index allows only
-- one in-flight operation per payment, so concurrent captures or refunds
-- on the same payment cannot race at the provider.
CREATE TABLE payment_operations (
    id            text PRIMARY KEY,
    payment_id    text NOT NULL REFERENCES payments(id),
    kind          text NOT NULL CHECK (kind IN ('authorize', 'capture', 'void', 'refund')),
    amount        bigint NOT NULL CHECK (amount >= 0),
    status        text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    failure_code  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_operations_one_in_flight ON payment_operations (payment_id) WHERE status = 'pending';

-- Append-only history of every state change.
CREATE TABLE payment_events (
    id           bigserial PRIMARY KEY,
    payment_id   text NOT NULL REFERENCES payments(id),
    op           text NOT NULL,
    from_status  text,
    to_status    text NOT NULL,
    amount       bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_events_payment ON payment_events (payment_id, id);

-- Append-only double-entry ledger. Debit is positive, credit negative.
CREATE TABLE ledger_entries (
    id          bigserial PRIMARY KEY,
    payment_id  text NOT NULL REFERENCES payments(id),
    journal_id  text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('authorize', 'capture', 'release', 'refund')),
    account     text NOT NULL CHECK (account IN ('customer', 'hold', 'merchant')),
    amount      bigint NOT NULL CHECK (amount <> 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_payment ON ledger_entries (payment_id, id);
CREATE INDEX ledger_entries_journal ON ledger_entries (journal_id);

-- +goose StatementBegin
CREATE FUNCTION forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER payment_events_append_only BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Every journal must sum to zero. Deferred to commit time so all lines of
-- a journal can be inserted first.
-- +goose StatementBegin
CREATE FUNCTION check_journal_balanced() RETURNS trigger AS $$
BEGIN
    IF (SELECT sum(amount) FROM ledger_entries WHERE journal_id = NEW.journal_id) <> 0 THEN
        RAISE EXCEPTION 'journal % is not balanced', NEW.journal_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER ledger_journal_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_journal_balanced();

CREATE TABLE idempotency_keys (
    merchant_id    text NOT NULL REFERENCES merchants(id),
    key            text NOT NULL,
    request_hash   bytea NOT NULL,           -- HMAC-SHA256(api key, method, path, body)
    status         text NOT NULL CHECK (status IN ('in_progress', 'completed')),
    response_code  integer,
    response_body  bytea,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, key)
);
CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);

-- Transactional outbox: written in the same transaction as the state
-- change, delivered later by the webhook worker.
CREATE TABLE outbox (
    id               text PRIMARY KEY,
    merchant_id      text NOT NULL REFERENCES merchants(id),
    payment_id       text NOT NULL REFERENCES payments(id),
    event_type       text NOT NULL,
    payload          jsonb NOT NULL,
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed', 'skipped')),
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_error       text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    delivered_at     timestamptz
);
CREATE INDEX outbox_due ON outbox (next_attempt_at) WHERE status = 'pending';

CREATE TABLE webhook_deliveries (
    id           bigserial PRIMARY KEY,
    outbox_id    text NOT NULL REFERENCES outbox(id),
    attempt      integer NOT NULL,
    status_code  integer,
    error        text,
    duration_ms  bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_outbox ON webhook_deliveries (outbox_id, id);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE outbox;
DROP TABLE idempotency_keys;
DROP TABLE ledger_entries;
DROP TABLE payment_events;
DROP FUNCTION check_journal_balanced();
DROP FUNCTION forbid_mutation();
DROP TABLE payment_operations;
DROP TABLE payments;
DROP TABLE card_tokens;
DROP TABLE merchants;
