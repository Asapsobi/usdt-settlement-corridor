-- A single screening_writer role, same posture as depositwatcher's own
-- 0001: this database is not append-only by design at the role-privilege
-- level either (screening_results IS append-only by convention -- see
-- 0002 -- but that guarantee lives in this package's own Go code and the
-- migration comments, not in a restrictive role, since screening_result_
-- invalidations is legitimately insert-only-forever too and there is no
-- second, more-restrictive role this chunk's own invariants would need).
--
-- Wrapped in an existence check because Postgres roles are cluster-wide,
-- not per-database: this migration must not fail with "role already
-- exists" when applied to a second database (dev alongside test) in the
-- same cluster. Same mirror-image consequence on the way down as C1's and
-- C2's own 0001: DROP ROLE fails while any other database in the cluster
-- still has this migration applied, because each leaves its own
-- default-ACL entries against the role -- expected, not a bug.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'screening_writer') THEN
        CREATE ROLE screening_writer NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT screening_writer TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT USAGE ON SCHEMA public TO screening_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO screening_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE ON SEQUENCES TO screening_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE ON SEQUENCES FROM screening_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM screening_writer;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE USAGE ON SCHEMA public FROM screening_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE screening_writer;
-- +goose StatementEnd
