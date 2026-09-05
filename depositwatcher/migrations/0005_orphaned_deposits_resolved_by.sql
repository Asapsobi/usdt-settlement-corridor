-- C2.9's resolve endpoint records WHO resolved an orphaned deposit, not
-- just that it was -- resolution alone (C2.8's own column) has no audit
-- trail of who acted. Matching this whole system's actor convention
-- (derived server-side from the bearer token, never a caller-supplied
-- body field -- see C1's own §A note and AUTH's design goal), this is
-- set from the resolving request's authenticated actor, never from the
-- request body.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE orphaned_deposits ADD COLUMN resolved_by text null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE orphaned_deposits DROP COLUMN resolved_by;
-- +goose StatementEnd
