-- C6.6: webhook delivery.
--
-- webhook_url: where this customer's webhooks get POSTed. Nullable --
-- a customer with none configured simply never gets a
-- webhook_deliveries row enqueued for them (the trigger loop skips
-- them outright); C6.5's own pull-based status endpoint is always
-- their backstop regardless.
--
-- webhook_cursors: one row per polled C1 order state (settled, held,
-- refunded), each independently advancing -- same shape as
-- screening's own discovery_cursor, one column widened to one row per
-- state since this loop polls three states, not one.
--
-- webhook_deliveries: one row per (external_id, event_type) state
-- transition this gateway has observed and owes a webhook for.
-- Unique on that pair so the trigger loop's own re-poll of a cursor
-- window it already advanced past is a harmless no-op, never a
-- duplicate delivery.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers ADD COLUMN webhook_url text;

CREATE TABLE webhook_cursors (
    state       text primary key,
    cursor      text not null default '',
    updated_at  timestamptz not null default now()
);
INSERT INTO webhook_cursors (state) VALUES ('settled'), ('held'), ('refunded');

CREATE TABLE webhook_deliveries (
    id               bigint generated always as identity primary key,
    customer_id      bigint not null references customers(id),
    external_id      text not null,
    event_type       text not null,
    payload          jsonb not null,
    created_at       timestamptz not null default now(),
    delivered_at     timestamptz,
    attempt_count    int not null default 0,
    next_attempt_at  timestamptz not null default now(),
    last_error       text,
    UNIQUE (external_id, event_type)
);

-- The delivery loop's own claim query: every not-yet-delivered row due
-- now. Partial index -- a delivered row (the overwhelming majority
-- over time) drops out of it entirely.
CREATE INDEX idx_webhook_deliveries_due ON webhook_deliveries(next_attempt_at) WHERE delivered_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE webhook_deliveries;
DROP TABLE webhook_cursors;
ALTER TABLE customers DROP COLUMN webhook_url;
-- +goose StatementEnd
