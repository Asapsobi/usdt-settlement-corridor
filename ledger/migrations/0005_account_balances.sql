-- account_balances is the ONLY mutable table in the money path: a cache of
-- each account's running balance, updated in the SAME transaction as the
-- journal entry that changed it, never afterward, never by a background
-- job. Every other table in this schema is append-only by design; this
-- one is the deliberate, singular exception -- which is exactly why it
-- needs an explicit UPDATE grant. The schema-wide default from 0001 only
-- ever gives ledger_writer INSERT+SELECT.
--
-- last_entry_id is a watermark: the highest journal_entries.id whose
-- effect this row reflects. It exists because BalanceAsOf needs to answer
-- "what did this account's balance look like as of entry N," which the
-- cache alone (always current) cannot answer.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE account_balances (
    account_id     bigint primary key references accounts(id),
    asset          asset_code not null,
    balance_units  numeric(38,0) not null default 0,
    last_entry_id  bigint not null,
    updated_at     timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
GRANT UPDATE ON account_balances TO ledger_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
REVOKE UPDATE ON account_balances FROM ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE account_balances;
-- +goose StatementEnd
