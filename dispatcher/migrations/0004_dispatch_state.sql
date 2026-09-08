-- dispatch_state is C5.3's own local record of "this order has entered
-- dispatching" -- deliberately NOT the dispatch_attempts table C5.5's own
-- spec names (the same judgment call C1.5 made deferring that table
-- until the broadcast layer actually needs it; this is a smaller, earlier
-- concern). One row per order, written once when the E2 conversion entry
-- lands, read back by C5.7's failure path to learn which slot and which
-- conversion entry a `dispatching` order is tied to.
--
-- order_id is the primary key, not attempt-scoped: entering dispatching
-- happens at most once per order, ever -- invariant 3 keeps E2 and E3 as
-- two separate, sequential C1 calls, and this row's own idempotency key
-- is order_id alone, with no <attempt> segment, matching §A's literal
-- Idempotency-Key example for this call
-- ("dispatcher:enter_dispatching:<order_id>"), not invariant 6's more
-- general per-attempt template (which governs the broadcast-layer calls
-- C5.5 onward actually retries as distinct attempts).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE dispatch_state (
    order_id                bigint primary key,
    slot_id                 smallint not null,
    conversion_entry_key    text not null,
    status                  text not null CHECK (status IN ('DISPATCHING', 'SETTLED', 'HELD')),
    entered_dispatching_at  timestamptz not null,
    updated_at              timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_dispatch_state_slot ON dispatch_state (slot_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE dispatch_state;
-- +goose StatementEnd
