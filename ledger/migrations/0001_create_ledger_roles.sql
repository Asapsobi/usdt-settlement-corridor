-- Creates the two roles that enforce append-only at the database level,
-- not merely by application convention: ledger_writer (INSERT + SELECT,
-- never UPDATE or DELETE) and ledger_reader (SELECT only).
--
-- Both are NOLOGIN roles granted to whichever login role runs this
-- migration. The application connects as that login role and issues
-- `SET ROLE ledger_writer` (or ledger_reader) for the duration of a
-- session/transaction, rather than holding separate passworded
-- credentials per role — no secrets belong in a committed migration.
--
-- ALTER DEFAULT PRIVILEGES (omitting `FOR ROLE`) applies to whichever role
-- executes this statement, i.e. the same role that will later create every
-- accounting table via subsequent migrations. That is what makes the grant
-- automatic for tables that do not exist yet: C1.2's journal_entries and
-- journal_lines inherit these grants the moment they are created, with no
-- repeated GRANT statements required in later migrations.

-- Role creation is wrapped in a existence check because Postgres roles are
-- cluster-wide, not per-database, while goose's applied-migrations ledger
-- is per-database. Without this, running this migration against a second
-- database in the same cluster (e.g. a test database alongside dev) would
-- fail with "role already exists" on plain CREATE ROLE, which has no native
-- IF NOT EXISTS form.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'ledger_reader') THEN
        CREATE ROLE ledger_reader NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'ledger_writer') THEN
        CREATE ROLE ledger_writer NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT ledger_reader TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT ledger_writer TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT USAGE ON SCHEMA public TO ledger_reader, ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT ON TABLES TO ledger_reader;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT ON TABLES TO ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE ON SEQUENCES TO ledger_writer;
-- +goose StatementEnd

-- NOTE: because ledger_writer/ledger_reader are cluster-wide roles, DROP
-- ROLE below fails with "cannot be dropped because some objects depend on
-- it" if this migration was also applied to any OTHER database in the same
-- cluster (each such database leaves its own default-ACL entries against
-- these roles). That is expected, not a bug: in the normal case, one
-- database per cluster, the down migration runs cleanly. A shared-cluster
-- multi-database dev setup must roll back every database before this one,
-- or accept that the roles remain until the last database does.

-- +goose Down
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE ON SEQUENCES FROM ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT ON TABLES FROM ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT ON TABLES FROM ledger_reader;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE USAGE ON SCHEMA public FROM ledger_reader, ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE ledger_reader;
-- +goose StatementEnd
