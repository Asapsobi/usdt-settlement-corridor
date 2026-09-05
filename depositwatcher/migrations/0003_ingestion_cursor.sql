-- The block-ingestion loop's own bookkeeping: how far it's walked (a
-- single-row cursor, same pattern as C1's system_state) and a rolling
-- window of recently observed blocks used ONLY for the pre-final
-- advisory reorg check described in C2's build spec §B -- this is NOT
-- the finality decision (that's LatestFinalized, C2.5's job) and nothing
-- here ever triggers a report to C1 on its own.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE ingestion_cursor (
    id            smallint primary key default 1 check (id = 1),
    last_scanned  bigint not null default 0,
    updated_at    timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Seeded here, not left for application code to handle a missing row --
-- same reasoning as C1's system_state seed in migration 0007: the Go side
-- should always be able to assume the single row exists and just UPDATE
-- it, never branch on "does it exist yet".
INSERT INTO ingestion_cursor (id, last_scanned) VALUES (1, 0);
-- +goose StatementEnd

-- +goose StatementBegin
-- One row per height, not a time-windowed table: pruning is "keep the
-- most recent N heights" (see internal/chain.DefaultSeenBlocksWindow),
-- which the primary key on height already supports efficiently via a
-- plain range DELETE -- no separate index on observed_at is needed for
-- that, and observed_at itself is purely informational (when THIS
-- process happened to see the block, not a consensus fact about it).
CREATE TABLE seen_blocks (
    height        bigint not null,
    hash          text not null,
    parent_hash   text not null,
    observed_at   timestamptz not null default now(),
    primary key (height)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE seen_blocks;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE ingestion_cursor;
-- +goose StatementEnd
