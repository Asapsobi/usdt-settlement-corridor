-- signing_requests is S1.3's own queue: one row per RequestSignature
-- call, idempotent on idempotency_key. status starts PENDING and moves
-- to SIGNED or REJECTED exactly once (S1.4 adds the one-way-transition
-- trigger once REJECTED exists as a real path; S1.3 alone only ever
-- produces PENDING -> SIGNED, which the application layer already
-- controls tightly enough -- see that chunk's own migration for the
-- trigger).
--
-- signing_audit_log is invariant 4's own append-only record: every real
-- KMS Sign call, ever. REVOKEd UPDATE/DELETE from s1_writer specifically,
-- overriding migration 0001's own default grant for every other table --
-- the same "append-only by revoking the privilege, not by a separate
-- role" note s1-key-custody-architecture.md's own build doc flags.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE signing_requests (
    id              bigserial primary key,
    idempotency_key text not null unique,
    slot_id         smallint not null,
    digest          bytea not null CHECK (octet_length(digest) = 32),
    estimated_usd   numeric not null,
    status          text not null CHECK (status IN ('PENDING', 'SIGNED', 'REJECTED')),
    signed_tx       bytea CHECK (signed_tx IS NULL OR octet_length(signed_tx) = 65),
    created_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE signing_audit_log (
    id                  bigserial primary key,
    signing_request_id  bigint not null references signing_requests(id),
    slot_id             smallint not null,
    digest              bytea not null CHECK (octet_length(digest) = 32),
    approvers           text[],
    signed_at           timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
REVOKE UPDATE, DELETE ON signing_audit_log FROM s1_writer;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE signing_audit_log;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE signing_requests;
-- +goose StatementEnd
