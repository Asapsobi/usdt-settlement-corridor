-- C6.1: customers table. api_key_hash, never the raw key, is what's
-- persisted -- the raw key is shown to the caller exactly once, at
-- creation, and never again (this migration's own doc comment on the
-- Go side enforces that, not this schema). rate_limit_per_minute is
-- nullable: NULL means "use the service-wide default," so most rows
-- never need an explicit override.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE customers (
    id                     bigint generated always as identity primary key,
    name                   text not null,
    api_key_hash           text not null unique,
    status                 text not null default 'active' check (status in ('active', 'suspended')),
    rate_limit_per_minute  integer check (rate_limit_per_minute is null or rate_limit_per_minute > 0),
    created_at             timestamptz not null default now(),
    updated_at             timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE customers;
-- +goose StatementEnd
