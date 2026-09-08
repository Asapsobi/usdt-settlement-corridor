-- dispatch_attempts is the table C1.5 explicitly declined to build (it's
-- C5's own concern, not C1's): one row per constructed transfer, keyed
-- content-addressably by unsigned_tx_hash so a rebuild of the identical
-- transfer (C5.4's own determinism guarantee) collides on the UNIQUE
-- constraint rather than silently duplicating a broadcast. This is the
-- table invariant 1 ("a broadcast is attempted at most once per (order,
-- dispatch attempt) pair, exactly-once under retry") is proven against.
--
-- Deliberately a different table from dispatch_state (0004): dispatch_state
-- is one row per ORDER, written once when E2 lands; this is one row per
-- (order, attempt_number) -- an order can accumulate more than one row
-- here across retries that build a genuinely new transfer (a fresh
-- unsigned_tx_hash), never across a retry of the SAME one.
--
-- signed_tx (not just signed_tx_hash) stores the actual 65-byte compact
-- signature S1 returned, hex-encoded -- required by C5.5's own acceptance
-- criterion that a resumed Broadcast (crashed between SIGNED and the
-- broadcast call) "never calls Sign again," which a hash alone cannot
-- satisfy (a hash cannot be turned back into the signature the broadcast
-- call actually needs). signed_tx_hash is kept alongside it purely as a
-- quick audit/display field, matching the build doc's own named column.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE dispatch_attempts (
    id                bigserial primary key,
    order_id          bigint not null,
    slot_id           smallint not null,
    attempt_number    int not null,
    unsigned_tx_hash  text not null unique,
    signed_tx         text,
    signed_tx_hash    text,
    broadcast_at      timestamptz,
    tron_txid         text,
    status            text not null CHECK (status IN ('BUILT', 'SIGNED', 'BROADCAST', 'CONFIRMED', 'FAILED')),
    created_at        timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_dispatch_attempts_order_attempt ON dispatch_attempts (order_id, attempt_number);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE dispatch_attempts;
-- +goose StatementEnd
