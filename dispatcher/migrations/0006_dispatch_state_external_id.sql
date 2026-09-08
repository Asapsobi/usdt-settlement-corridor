-- C5.7's reconciliation job needs to call C1's GET /v1/orders/{external_id}
-- for an order it only knows locally by its numeric order_id (C1 has no
-- GET-by-numeric-id route -- external_id is the only real lookup key).
-- Storing it here, once, at EnterDispatching time, avoids a roundabout
-- GET /v1/orders?state=dispatching list-and-filter just to resolve it.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE dispatch_state ADD COLUMN external_id text NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE dispatch_state ALTER COLUMN external_id DROP DEFAULT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dispatch_state DROP COLUMN external_id;
-- +goose StatementEnd
