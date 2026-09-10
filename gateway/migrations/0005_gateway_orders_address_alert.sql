-- C6.4: tracks whether a pending-address row has already fired its
-- one alert, so a row stuck past the alert threshold raises exactly
-- one alert (not one per loop tick) -- same "alert once, not per
-- retry" discipline C4's own buffer.Reconcile established.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE gateway_orders ADD COLUMN address_pending_alerted_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE gateway_orders DROP COLUMN address_pending_alerted_at;
-- +goose StatementEnd
