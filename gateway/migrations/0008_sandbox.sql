-- C6.7: the sandbox -- a fully separate code path from production,
-- never sharing state, never sharing a customer_id namespace (a
-- sandbox customer's own is_sandbox flag never changes; no
-- conversion between a sandbox and a production customer), and never
-- reaching a real C1/C2/C4/C5 call. sandbox_orders is its own table,
-- entirely separate from gateway_orders -- a production
-- GET /v1/orders/{external_id} only ever reads gateway_orders, so a
-- sandbox external_id is structurally invisible to it, not just
-- conventionally hidden.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers ADD COLUMN is_sandbox boolean NOT NULL DEFAULT false;

CREATE TABLE sandbox_orders (
    external_id        text primary key,
    customer_id        bigint not null references customers(id),
    trigger            text not null,
    tier               text not null,
    amount_in          bigint not null,
    amount_out         bigint not null,
    fee_units          bigint not null,
    network_fee_units  bigint not null,
    recipient_address  text not null,
    deposit_address    text not null,
    state              text not null,
    hold_reason        text,
    webhook_attempts   int not null default 0,
    webhook_exhausted  boolean not null default false,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now()
);
CREATE INDEX idx_sandbox_orders_customer_id ON sandbox_orders(customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE sandbox_orders;
ALTER TABLE customers DROP COLUMN is_sandbox;
-- +goose StatementEnd
