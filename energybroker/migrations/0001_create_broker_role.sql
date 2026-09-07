-- A single broker_writer role, same posture as depositwatcher's and
-- screening's own 0001: this database is not append-only by design at
-- the role-privilege level either (price_observations IS append-only by
-- convention, but delegations and buffer state (later chunks) legitimately
-- update in place -- e.g. Delegation.ConfirmedAt filling in once
-- on-chain-verified -- so there is no second, more-restrictive role this
-- chunk's own invariants would need).
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
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'broker_writer') THEN
        CREATE ROLE broker_writer NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT broker_writer TO CURRENT_USER;
-- +goose StatementEnd
-- +goose StatementBegin
GRANT USAGE ON SCHEMA public TO broker_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO broker_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE ON SEQUENCES TO broker_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE ON SEQUENCES FROM broker_writer;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM broker_writer;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE USAGE ON SCHEMA public FROM broker_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP ROLE broker_writer;
-- +goose StatementEnd
