-- reservations is C4.4's own C5-facing contract: one row per
-- POST /v1/reservations call, keyed for idempotency by the caller's own
-- Idempotency-Key header, same convention as every other write in this
-- system (C1's entries, C3's transitions).
--
-- vendor and cost_trx stay null until CONFIRMED -- there is no vendor or
-- cost to record for a still-PENDING or eventually-FAILED reservation,
-- and a null here is never confused with a real "zero cost" delegation
-- (Redelegate's own zero-cost retarget still names a real vendor).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE reservations (
    id              bigserial primary key,
    idempotency_key text not null,
    external_id     text not null,
    order_id        bigint not null,
    target_address  text not null CHECK (target_address <> ''),
    energy_units    bigint not null CHECK (energy_units > 0),
    tier            text not null CHECK (tier IN ('DIRECT', 'STANDARD', 'SWEEP')),
    status          text not null CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    vendor          text,
    cost_trx        numeric(38,0),
    confirmed_at    timestamptz,
    deadline        timestamptz not null,
    created_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Enforces and serves Create's own idempotency requirement directly:
-- a concurrent duplicate INSERT for the same key fails this constraint
-- rather than racing to create two reservations for one logical request.
CREATE UNIQUE INDEX idx_reservations_idempotency_key ON reservations (idempotency_key);
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves DemandObserver's own "recent reservation volume" scan
-- (internal/buffer's own TargetLevel, C4.3) directly off the index.
CREATE INDEX idx_reservations_created_at ON reservations (created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE reservations;
-- +goose StatementEnd
