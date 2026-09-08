-- C5.1's own selection tie-break needs a place to record "when was this
-- slot last chosen" -- least-recently-used among eligible slots, the
-- deterministic rule this chunk's own build spec requires ("whichever is
-- chosen must be deterministic and tested, not whichever happens to be
-- first in a map iteration"). NULL (never yet dispatched) sorts first.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE slots ADD COLUMN last_dispatch_at timestamptz;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE slots DROP COLUMN last_dispatch_at;
-- +goose StatementEnd
