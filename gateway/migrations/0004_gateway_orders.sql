-- C6.3/C6.4: gateway_orders is this gateway's own bookkeeping row per
-- order -- external_id -> {c1_order_created, c2_address_assigned} --
-- what C6.4's reconciliation loop reconciles against. A row is only
-- ever inserted AFTER C1's own POST /v1/orders has already succeeded
-- (c1_order_created is always true the moment this row exists; kept as
-- an explicit column, not implied by row presence, so its own meaning
-- reads clearly next to c2_address_assigned rather than being implicit).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE gateway_orders (
    external_id          text primary key,
    customer_id          bigint not null references customers(id),
    quote_id             bigint not null references quotes(id),
    c1_order_id          bigint not null,
    c1_order_created     boolean not null default true,
    c2_address_assigned  boolean not null default false,
    deposit_address      text,
    created_at           timestamptz not null default now(),
    updated_at           timestamptz not null default now()
);

CREATE INDEX idx_gateway_orders_customer_id ON gateway_orders(customer_id);

-- C6.4's own reconciliation loop query: every row still missing its
-- address, oldest first. Partial index -- once c2_address_assigned
-- flips true a row drops out of this index entirely, keeping it small
-- regardless of total order volume.
CREATE INDEX idx_gateway_orders_pending_address ON gateway_orders(created_at) WHERE c2_address_assigned = false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE gateway_orders;
-- +goose StatementEnd
