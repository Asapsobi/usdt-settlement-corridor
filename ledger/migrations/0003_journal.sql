-- The append-only, multi-asset, balanced journal -- the heart of C1.
--
-- SIGN CONVENTION, never deviate: in journal_lines.amount_units, positive =
-- debit, negative = credit. For every entry, and for every asset within
-- that entry independently, SUM(amount_units) = 0.
--
-- Balance enforcement is layered on top of the Go-side check in
-- internal/journal, not instead of it:
--
--   1. Go (internal/journal.Post) validates before ever issuing the INSERT.
--   2. journal_balance_check, a DEFERRABLE INITIALLY DEFERRED constraint
--      trigger, recomputes the per-asset sum for the touched entry at
--      COMMIT time (after every line of the entry has been inserted, which
--      is exactly why it must be deferred rather than immediate) and
--      raises if any asset's sum is nonzero. This is what makes the
--      invariant hold even against raw SQL that never goes through Go.
--   3. journal_forbid_modification blocks UPDATE and DELETE on both
--      tables unconditionally -- including for the table owner, who would
--      otherwise bypass the role grants from 0001 by virtue of owning the
--      table. Role grants and triggers are deliberately redundant here.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE journal_entries (
    id               bigserial primary key,
    idempotency_key  text not null unique,
    payload_hash     bytea not null,
    entry_type       text not null,
    order_id         bigint null, -- FK added in C1.5
    actor            text not null,
    occurred_at      timestamptz not null,
    recorded_at      timestamptz not null default now(),
    reversal_of      bigint null references journal_entries(id),
    metadata         jsonb not null default '{}'
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE journal_lines (
    entry_id      bigint not null references journal_entries(id),
    seq           smallint not null,
    account_id    bigint not null references accounts(id),
    asset         asset_code not null,
    amount_units  numeric(38,0) not null,
    primary key (entry_id, seq)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION journal_check_balance() RETURNS trigger AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT asset, SUM(amount_units) AS total
        FROM journal_lines
        WHERE entry_id = NEW.entry_id
        GROUP BY asset
        HAVING SUM(amount_units) <> 0
    LOOP
        RAISE EXCEPTION 'journal entry % does not balance for asset %: sum = %',
            NEW.entry_id, r.asset, r.total;
    END LOOP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE CONSTRAINT TRIGGER journal_lines_balance_check
    AFTER INSERT ON journal_lines
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION journal_check_balance();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION journal_forbid_modification() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER journal_entries_append_only
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION journal_forbid_modification();
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER journal_lines_append_only
    BEFORE UPDATE OR DELETE ON journal_lines
    FOR EACH ROW EXECUTE FUNCTION journal_forbid_modification();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER journal_lines_append_only ON journal_lines;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER journal_entries_append_only ON journal_entries;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION journal_forbid_modification();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER journal_lines_balance_check ON journal_lines;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION journal_check_balance();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE journal_lines;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE journal_entries;
-- +goose StatementEnd
