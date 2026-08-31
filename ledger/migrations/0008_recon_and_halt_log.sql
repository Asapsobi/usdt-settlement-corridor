-- Two append-only tables for C1.7, both reusing journal_forbid_modification
-- (defined in 0003) for the same UPDATE/DELETE-blocking-even-for-the-owner
-- treatment already applied to order_transitions -- neither has anything
-- table-specific about the protection it needs.
--
-- halt_log: the full audit history of every halt set and clear. system_state
-- (0007) only ever holds the CURRENT halt's reason/detail; halt_log is what
-- preserves earlier ones once a later event overwrites system_state.
--
-- recon_snapshots: what C2/C5 report the chain actually holds for an
-- account they own, at a stated chain reference, against the ledger
-- watermark they believe corresponds to it. C1 never fetches anything
-- itself and does not know what a block is -- this table only ever
-- records what it's told.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE halt_log (
    id          bigserial primary key,
    action      text not null,   -- 'set' or 'clear'
    reason      text null,       -- halt_reason, for 'set' actions
    detail      jsonb null,
    note        text null,       -- operator's free-text note, for 'clear' actions
    actor       text not null,
    occurred_at timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER halt_log_append_only
    BEFORE UPDATE OR DELETE ON halt_log
    FOR EACH ROW EXECUTE FUNCTION journal_forbid_modification();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE recon_snapshots (
    id             bigserial primary key,
    account_id     bigint not null references accounts(id),
    asset          asset_code not null,
    observed_units numeric(38,0) not null,
    chain_ref      text not null,
    ledger_units   numeric(38,0) not null,
    watermark      bigint not null,
    drift_units    numeric(38,0) not null,
    observed_at    timestamptz not null,
    recorded_at    timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER recon_snapshots_append_only
    BEFORE UPDATE OR DELETE ON recon_snapshots
    FOR EACH ROW EXECUTE FUNCTION journal_forbid_modification();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER recon_snapshots_append_only ON recon_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE recon_snapshots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER halt_log_append_only ON halt_log;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE halt_log;
-- +goose StatementEnd
