-- A single s1_writer role, same posture as every prior component's own
-- 0001 (e.g. energybroker's broker_writer): this database is not
-- append-only by design at the role-privilege level -- signing_requests
-- legitimately updates in place (status PENDING -> SIGNED/REJECTED) --
-- so there is no second, more-restrictive role this chunk's own
-- invariants would need. The audit table (S1.3) IS append-only, by
-- convention and by revoking UPDATE/DELETE on it specifically once it
-- exists, not by a separate role here.
--
-- Wrapped in an existence check because Postgres roles are cluster-wide,
-- not per-database: this migration must not fail with "role already
-- exists" when applied to a second database (dev alongside test) in the
-- same cluster. Same mirror-image consequence on the way down as every
-- prior component's own 0001: DROP ROLE fails while any other database
-- in the cluster still has this migration applied, because each leaves
-- its own default-ACL entries against the role -- expected, not a bug.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 's1_writer') THEN
        CREATE ROLE s1_writer NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT s1_writer TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT USAGE ON SCHEMA public TO s1_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO s1_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE ON SEQUENCES TO s1_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE ON SEQUENCES FROM s1_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM s1_writer;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE USAGE ON SCHEMA public FROM s1_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE s1_writer;
-- +goose StatementEnd
