-- screening_results is append-only, same audit instinct as C1's journal:
-- a fresh check after expiry (or after an explicit invalidation) is
-- always a NEW row, never an UPDATE to an old one. There is deliberately
-- no unique constraint on (provider_name, sender_address) -- one would
-- force exactly the overwrite this table exists to avoid, and would make
-- "show this address's full verdict history" impossible to answer later.
--
-- screening_result_invalidations is the append-only companion that makes
-- Invalidate (internal/cache) work without ever mutating or deleting a
-- screening_results row: invalidating a cache key records "nothing
-- checked at or before this instant, for this key, should be trusted",
-- and internal/cache.Get filters out any screening_results row whose
-- checked_at falls at or before the latest invalidation for that same
-- key. A fresh Put after an invalidation naturally has a later
-- checked_at, so it is trusted again with no row to un-invalidate.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE screening_results (
    id              bigserial primary key,
    provider_name   text not null,
    sender_address  text not null,
    risk_score      double precision not null
        CHECK (risk_score >= 0 AND risk_score <= 1),
    flagged         boolean not null,
    reason_codes    text[] not null default '{}',
    raw_response    jsonb not null,
    checked_at      timestamptz not null,
    expires_at      timestamptz not null,
    created_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves internal/cache.Get's "freshest unexpired row for this key"
-- lookup directly off the index, without a table scan.
CREATE INDEX idx_screening_results_lookup
    ON screening_results (provider_name, sender_address, checked_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE screening_result_invalidations (
    id              bigserial primary key,
    provider_name   text not null,
    sender_address  text not null,
    invalidated_at  timestamptz not null default now(),
    reason          text not null CHECK (reason <> ''),
    actor           text not null CHECK (actor <> ''),
    created_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_screening_result_invalidations_lookup
    ON screening_result_invalidations (provider_name, sender_address, invalidated_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE screening_result_invalidations;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE screening_results;
-- +goose StatementEnd
