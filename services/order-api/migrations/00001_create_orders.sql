-- +goose Up
-- The catalogue (D68): loaded by `order-api catalogue import`, which retires
-- products missing from its file instead of deleting them, for past orders.
CREATE TABLE products (
    sku              text        PRIMARY KEY CHECK (sku ~ '^[A-Z0-9]+(-[A-Z0-9]+)*$' AND length(sku) <= 32),
    name             text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    unit_price_gross bigint      NOT NULL CHECK (unit_price_gross > 0 AND unit_price_gross <= 9007199254740991),
    tax_rate_bps     integer     NOT NULL CHECK (tax_rate_bps IN (700, 1900)),
    weight_grams     integer     NOT NULL CHECK (weight_grams > 0),
    retired_at       timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- One order per idempotency key (D66): a retry after a lost response finds the
-- order and resumes its charge instead of creating another. request_hash tells
-- that retry from a different request reusing the key. The shipping address is
-- personal data that stays here (D66, D64).
CREATE TABLE orders (
    id                    uuid        PRIMARY KEY,
    idempotency_key       text        NOT NULL UNIQUE,
    request_hash          bytea       NOT NULL,
    status                text        NOT NULL CHECK (status IN ('pending_payment', 'paid', 'payment_failed')),
    email                 text        NOT NULL,
    first_name            text        NOT NULL,
    last_name             text        NOT NULL,
    address_line1         text        NOT NULL,
    address_line2         text        NOT NULL,
    postal_code           text        NOT NULL,
    city                  text        NOT NULL,
    country               char(2)     NOT NULL,
    shipping_zone         text        NOT NULL,
    shipping_speed        text        NOT NULL,
    shipping_rate_source  text        NOT NULL,
    shipping_net          bigint      NOT NULL,
    shipping_tax          bigint      NOT NULL,
    shipping_gross        bigint      NOT NULL,
    net_total             bigint      NOT NULL,
    tax_total             bigint      NOT NULL,
    gross_total           bigint      NOT NULL CHECK (gross_total > 0 AND gross_total <= 9007199254740991),
    payment_token         text        NOT NULL,
    decline_code          text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX orders_pending ON orders (created_at) WHERE status = 'pending_payment';

CREATE TABLE order_lines (
    order_id         uuid    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    line_number      integer NOT NULL CHECK (line_number BETWEEN 1 AND 50),
    sku              text    NOT NULL,
    name             text    NOT NULL,
    quantity         integer NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    unit_price_gross bigint  NOT NULL,
    tax_rate_bps     integer NOT NULL,
    net_amount       bigint  NOT NULL,
    tax_amount       bigint  NOT NULL,
    gross_amount     bigint  NOT NULL,
    PRIMARY KEY (order_id, line_number)
);

-- Shipping VAT follows the goods' rates, one part per rate (D54).
CREATE TABLE order_shipping_taxes (
    order_id     uuid    NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    tax_rate_bps integer NOT NULL,
    net_amount   bigint  NOT NULL,
    tax_amount   bigint  NOT NULL,
    gross_amount bigint  NOT NULL,
    PRIMARY KEY (order_id, tax_rate_bps)
);

-- Transactional outbox in the Debezium Outbox Event Router shape (D48): the
-- relay publishes unpublished rows in order and stamps published_at.
CREATE TABLE outbox (
    id            uuid        PRIMARY KEY,
    aggregatetype text        NOT NULL,
    aggregateid   text        NOT NULL,
    type          text        NOT NULL,
    payload       jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz
);

CREATE INDEX outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
DROP TABLE order_shipping_taxes;
DROP TABLE order_lines;
DROP TABLE orders;
DROP TABLE products;
