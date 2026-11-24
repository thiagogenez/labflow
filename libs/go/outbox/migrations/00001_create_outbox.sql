-- +goose Up
-- The transactional outbox in the Debezium Outbox Event Router shape (D48).
-- order-api created this table in its own first migration before this library
-- existed, and applies that migration first, so the table is created only
-- where missing. Its indexes come next, built concurrently.
CREATE TABLE IF NOT EXISTS outbox (
    id            uuid        PRIMARY KEY,
    aggregatetype text        NOT NULL,
    aggregateid   text        NOT NULL,
    type          text        NOT NULL,
    payload       jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz
);
