-- +goose Up
-- owner identifies the request currently holding the key; a request whose
-- lease expired cannot overwrite the outcome of the one that took over.
CREATE TABLE idempotency_keys (
    key              text        PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 255),
    fingerprint      bytea       NOT NULL,
    owner            text        NOT NULL,
    completed        boolean     NOT NULL DEFAULT false,
    locked_until     timestamptz NOT NULL,
    response_status  integer,
    response_type    text,
    response_body    bytea,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
