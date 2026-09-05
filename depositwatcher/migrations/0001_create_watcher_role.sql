-- A single watcher_writer role, unlike C1's reader/writer split. C1 needed
-- two roles because its append-only guarantee depends on ledger_writer
-- having no UPDATE/DELETE grant at all -- that split IS the enforcement
-- mechanism. This service's own database is NOT append-only by design
-- (C2.1's address book legitimately updates an address's status field,
-- WATCHING -> FUNDED -> RETIRED), so there is no equivalent invariant a
-- second, more restrictive role would be enforcing here. One role with
-- full DML on its own tables is the right amount of privilege separation
-- for what this chunk actually needs, not a mechanical copy of C1's.
--
-- Role creation is wrapped in an existence check for the same reason as
-- C1's own 0001: Postgres roles are cluster-wide, not per-database, so
-- running this migration against a second database in the same cluster
-- (dev alongside test) must not fail with "role already exists".
--
-- The same cluster-wide-ness has a mirror-image consequence on the way
-- down, exactly as C1's own 0001 notes: DROP ROLE below fails with
-- "cannot be dropped because some objects depend on it" if this migration
-- is still applied to any OTHER database in the same cluster (each such
-- database leaves its own default-ACL entries against the role). That is
-- expected, not a bug -- confirmed directly: rolling back a fresh
-- database while watcher_dev/watcher_test still have 0001 applied fails
-- on exactly this line, and rolling back all of them first (or accepting
-- the role stays until the last one does) resolves it.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'watcher_writer') THEN
        CREATE ROLE watcher_writer NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT watcher_writer TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT USAGE ON SCHEMA public TO watcher_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO watcher_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE ON SEQUENCES TO watcher_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE ON SEQUENCES FROM watcher_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM watcher_writer;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE USAGE ON SCHEMA public FROM watcher_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE watcher_writer;
-- +goose StatementEnd
