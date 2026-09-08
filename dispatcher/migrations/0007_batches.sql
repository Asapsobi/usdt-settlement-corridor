-- Sweep-tier batching (C5.8), deliberately its OWN, separate broadcast
-- tracking rather than retrofitting dispatch_attempts (0005): a batch
-- covers MULTIPLE orders sharing one broadcast, whereas dispatch_attempts
-- is fundamentally one-attempt-per-order. Keeping them apart means C5.5's
-- and C5.6's already-proven single-order machinery (Broadcast,
-- ConfirmFinality) stays untouched.
--
-- batch_queue is Sweep-tier's own waiting room: a screened order that has
-- already entered `dispatching` (E2 posted, same as every other tier)
-- but whose actual on-chain payout is accumulated into a batch rather
-- than dispatched immediately. QUEUED -> BATCHED when a batch is cut;
-- a failed recipient goes back to QUEUED for the next window, never
-- straight back to BATCHED.
--
-- batches is one row per cut multisend transaction -- CUT (built, energy
-- reserved) -> SIGNED -> BROADCAST -> CONFIRMED, or FAILED. No
-- destination-specific columns: batches.id is what batch_queue.batch_id
-- points at, and settlement itself is still reported per-order, straight
-- to C1, per §B's own "the batch is purely a C5-internal concept,
-- invisible to the ledger."
--
-- unsigned_tx (the actual BuildMultisend output, hex-encoded) is stored
-- alongside unsigned_tx_hash for the same reason dispatch_attempts.signed_tx
-- exists (0005): a hash alone cannot be turned back into the bytes a
-- resumed BroadcastBatch actually needs to sign or broadcast.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE batch_queue (
    id                 bigserial primary key,
    order_id           bigint not null unique,
    external_id        text not null,
    customer_id        text not null,
    recipient_address  text not null,
    amount_out         bigint not null,
    status             text not null CHECK (status IN ('QUEUED', 'BATCHED')),
    batch_id           bigint,
    enqueued_at        timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_batch_queue_queued ON batch_queue (enqueued_at) WHERE status = 'QUEUED';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE batches (
    id                bigserial primary key,
    slot_id           smallint not null,
    recipient_count   int not null,
    unsigned_tx       text,
    unsigned_tx_hash  text unique,
    signed_tx         text,
    signed_tx_hash    text,
    tron_txid         text,
    status            text not null CHECK (status IN ('CUT', 'SIGNED', 'BROADCAST', 'CONFIRMED', 'FAILED')),
    cut_at            timestamptz not null default now(),
    broadcast_at      timestamptz
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE batches;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE batch_queue;
-- +goose StatementEnd
