-- Adds the BSC deposit sender address to orders -- C3 (screening) needs
-- it to call a screening provider and has no chain access of its own to
-- derive it; C2 is the only component that ever observes it (parsed on
-- every Transfer log) and, until now, had nowhere to report it. See
-- docs/03-build/c3-screening-build-prompts.md's "Read this first".
--
-- Nullable and additive: existing rows get NULL, no existing caller of
-- orders.Create or the transitions endpoint is required to send it, and
-- nothing here changes the legal transition table. It is set exactly
-- once per funding event by orders.Transition on a quoted->funded
-- transition (see internal/orders/store.go's TransitionParams.SenderAddress)
-- -- no other code path writes this column.

-- The composite index serves GET /v1/orders?state=&updated_after=
-- (orders.ListByStateAfter) directly off the index: keyset pagination
-- ordered by (updated_at, id) within one state, no table scan.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE orders ADD COLUMN sender_address text NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_orders_state_updated_at_id ON orders (state, updated_at, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_orders_state_updated_at_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE orders DROP COLUMN sender_address;
-- +goose StatementEnd
