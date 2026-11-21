-- +goose Up
-- The provider keeps the brand and last four digits of a card, never the
-- full number, the expiry or the security code.
CREATE TABLE tokens (
    id          text        PRIMARY KEY,
    brand       text        NOT NULL,
    last4       char(4)     NOT NULL,
    outcome     text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- A token pays for one charge only (UNIQUE token_id). The merchant's
-- idempotency key is stored with the charge, so a retry finds its charge
-- even if the response was never recorded. A pending charge settles to
-- resolves_to once resolve_at has passed.
CREATE TABLE charges (
    id               text        PRIMARY KEY,
    token_id         text        NOT NULL UNIQUE REFERENCES tokens (id),
    idempotency_key  text        NOT NULL UNIQUE,
    amount           bigint      NOT NULL CHECK (amount > 0),
    currency         char(3)     NOT NULL,
    status           text        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    failure_code     text,
    resolves_to      text        CHECK (resolves_to IN ('succeeded', 'failed')),
    resolve_at       timestamptz,
    metadata         jsonb       NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE charges;
DROP TABLE tokens;
