-- Two additions for C1.6:
--
-- 1. A UNIQUE constraint on journal_entries.reversal_of: an entry can be
--    reversed at most once. This is defense-in-depth alongside the Go
--    layer's own check (internal/journal.Reverse rejects a second attempt
--    before ever issuing the INSERT) -- Postgres treats each NULL in a
--    UNIQUE column as distinct from every other NULL, so ordinary entries
--    (reversal_of IS NULL) are entirely unaffected; only a second row
--    naming the same non-null reversal_of is rejected.
--
-- 2. system_state: the minimal halt primitive C1.6 needs for reorg
--    scenario B ("set the system halt with reason POST_SETTLEMENT_REORG").
--    This is deliberately NOT the full C1.7 reconciler -- no halt_log,
--    no clearing, no reconciliation snapshots, no self-check ticker. Only
--    what this chunk's own acceptance criteria require: a place to record
--    that the system is halted, and why. C1.7 owns the rest: ClearHalt,
--    halt_log, and everything that actually enforces the halt against
--    halt-blocked transitions (C1.5's Transition does not consult this
--    table at all yet).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_reversal_of_key UNIQUE (reversal_of);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE system_state (
    id          smallint primary key default 1 check (id = 1),
    halted      boolean not null default false,
    halt_reason text null,
    halt_detail jsonb null,
    halted_at   timestamptz null,
    halted_by   text null,
    cleared_at  timestamptz null,
    cleared_by  text null
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO system_state (id, halted) VALUES (1, false);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE system_state;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_reversal_of_key;
-- +goose StatementEnd
