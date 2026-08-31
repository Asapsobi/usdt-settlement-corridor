-- The account model. Accounts only -- no journal entries yet (that's C1.2).
--
-- normal_side is derived from type, not chosen freely: ASSET/EXPENSE accounts
-- are debit-normal (+1), LIABILITY/REVENUE/EQUITY are credit-normal (-1),
-- POSITION is neither (0). The CHECK constraint below is what makes an
-- inconsistent (type, normal_side) pair impossible to insert even via raw
-- SQL that bypasses the Go layer entirely.
--
-- ledger_writer/ledger_reader need no explicit GRANT on this table: the
-- ALTER DEFAULT PRIVILEGES set up in 0001 already covers every table the
-- migration-running role creates from here on.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE account_type AS ENUM ('ASSET', 'LIABILITY', 'REVENUE', 'EXPENSE', 'EQUITY', 'POSITION');
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TYPE asset_code AS ENUM ('USDT_BEP20', 'USDT_TRC20', 'TRX', 'BNB');
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE accounts (
    id            bigserial primary key,
    code          text not null unique,
    type          account_type not null,
    asset         asset_code not null,
    normal_side   smallint not null,
    is_active     boolean not null default true,
    opened_at     timestamptz not null default now(),
    metadata      jsonb not null default '{}',
    CONSTRAINT normal_side_matches_type CHECK (
        (type IN ('ASSET', 'EXPENSE') AND normal_side = 1) OR
        (type IN ('LIABILITY', 'REVENUE', 'EQUITY') AND normal_side = -1) OR
        (type = 'POSITION' AND normal_side = 0)
    )
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE accounts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE asset_code;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE account_type;
-- +goose StatementEnd
