-- manual_fallback_events is C4.6's own record of every time routing
-- (C4.2) had nowhere automated left to go: every primary unhealthy, or
-- every primary priced above ceiling. See docs/runbook-energy-fallback.md
-- for what an operator actually does about one.
--
-- The partial unique index is the de-duplication mechanism itself, not
-- just an optimization: this chunk's own acceptance criterion is
-- "exactly one row per triggering condition, not one per failed
-- reservation attempt during the same outage window" -- enforcing that
-- at the database level means it holds even under concurrent triggers
-- (several reservations hitting the same outage at once), the same
-- reasoning C4.4's own reservations.idempotency_key unique index uses.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE manual_fallback_events (
    id           bigserial primary key,
    triggered_at timestamptz not null default now(),
    reason       text not null CHECK (reason IN ('all_unhealthy', 'all_over_ceiling')),
    order_id     bigint,
    resolved_at  timestamptz,
    resolution   text
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_manual_fallback_events_open_per_reason
    ON manual_fallback_events (reason)
    WHERE resolved_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE manual_fallback_events;
-- +goose StatementEnd
