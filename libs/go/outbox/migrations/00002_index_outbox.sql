-- +goose NO TRANSACTION
-- +goose Up
-- The relay selects unpublished events and deletes old ones by age (D64). The
-- indexes are built concurrently, so a service writing events during an
-- upgrade is never blocked; even CREATE INDEX IF NOT EXISTS would lock the
-- table first. Each is dropped first, to replace an index left invalid by an
-- interrupted build, which goose then runs again.
DROP INDEX CONCURRENTLY IF EXISTS outbox_unpublished;
CREATE INDEX CONCURRENTLY outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;
DROP INDEX CONCURRENTLY IF EXISTS outbox_published;
CREATE INDEX CONCURRENTLY outbox_published ON outbox (created_at) WHERE published_at IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS outbox_published;
