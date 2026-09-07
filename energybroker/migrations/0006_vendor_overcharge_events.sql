-- vendor_overcharge_events is C4.7's own audit trail: every time a
-- provider's ACTUAL charged price (recovered from a Delegation's own
-- CostTRX/EnergyUnits) didn't match what it quoted immediately before
-- the Delegate call, or exceeded the configured ceiling outright. This
-- is the "did we ever overpay" artifact the build spec's own acceptance
-- criteria ask for -- append-only, same audit instinct as every other
-- event table in this system (manual_fallback_events, C3's
-- screening_result_invalidations): a row here is a fact about what a
-- vendor actually did, never edited or deleted after the fact.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE vendor_overcharge_events (
    id                bigserial primary key,
    detected_at       timestamptz not null default now(),
    provider_name     text not null CHECK (provider_name <> ''),
    delegation_id     text not null CHECK (delegation_id <> ''),
    order_id          bigint,
    energy_units      bigint not null CHECK (energy_units > 0),
    quoted_price_sun  double precision not null,
    charged_price_sun double precision not null,
    ceiling_sun       double precision not null,
    over_ceiling      boolean not null
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_vendor_overcharge_events_provider ON vendor_overcharge_events (provider_name, detected_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE vendor_overcharge_events;
-- +goose StatementEnd
