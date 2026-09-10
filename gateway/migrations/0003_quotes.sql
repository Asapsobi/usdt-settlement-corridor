-- C6.2: quotes table. A quote is issued once (internal/pricing computes
-- it exactly once, here) and never recomputed at order-creation time --
-- C6.3 reads this row back verbatim. consumed_at/consumed_by_order_external_id
-- are set together, exactly once, the moment a quote is actually used.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE quotes (
    id                             bigint generated always as identity primary key,
    customer_id                    bigint not null references customers(id),
    tier                           text not null check (tier in ('DIRECT', 'STANDARD', 'SWEEP')),
    amount_in                      bigint not null check (amount_in > 0),
    amount_out                     bigint not null check (amount_out > 0),
    fee_units                      bigint not null check (fee_units > 0),
    network_fee_units              bigint not null check (network_fee_units > 0),
    recipient_address              text not null,
    created_at                     timestamptz not null default now(),
    expires_at                     timestamptz not null,
    consumed_at                    timestamptz,
    consumed_by_order_external_id  text,
    constraint quotes_consumed_together
        check ((consumed_at is null) = (consumed_by_order_external_id is null))
);

CREATE INDEX idx_quotes_customer_id ON quotes(customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE quotes;
-- +goose StatementEnd
