-- C2.8: a deposit that finalized on-chain for an order C1 no longer
-- considers open (the order expired, or was otherwise closed, before
-- C2's report reached it). This table is strictly a capture-and-surface
-- mechanism -- gap #3's business policy for what to actually DO about
-- one (refund, credit a new order, write off) is a separate, unbuilt
-- decision; until it exists, the safe default is "record it, alert a
-- human, touch nothing automatically." resolution stays null until that
-- decision (or a human acting on it) sets it -- no code in this service
-- writes to that column.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE orphaned_deposits (
    id                       bigserial primary key,
    order_id                 bigint not null,
    external_id              text not null,
    tx_hash                  text not null,
    log_index                int not null,
    amount                   numeric(38,0) not null,
    detected_at              timestamptz not null default now(),
    order_state_at_detection text not null,
    resolution               text null,
    resolved_at              timestamptz null,
    unique (tx_hash, log_index)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE orphaned_deposits;
-- +goose StatementEnd
