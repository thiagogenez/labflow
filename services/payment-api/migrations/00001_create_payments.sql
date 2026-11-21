-- +goose Up
-- One payment per order. A pending payment without psp_charge_id is one whose
-- outcome at the provider is unknown (a timeout, for example); the resolver
-- settles it by repeating the idempotent charge.
CREATE TABLE payments (
    order_id       uuid        PRIMARY KEY,
    token          text        NOT NULL,
    amount         bigint      NOT NULL CHECK (amount > 0 AND amount <= 9007199254740991),
    currency       char(3)     NOT NULL,
    status         text        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    psp_charge_id  text        UNIQUE,
    decline_code   text,
    attempts       integer     NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payments_pending ON payments (updated_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE payments;
