-- C3.8's POST /v1/rescreen-flags/{id}/resolve accepts an actor in its
-- request body, but C3.7's own rescreen_flags table (migration 0005)
-- had nowhere to record who -- an oversight caught while wiring the
-- HTTP endpoint, the same category of gap as holds.resolved_by, added
-- here the same way: additive, nullable, no existing row affected.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE rescreen_flags ADD COLUMN resolved_by text NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE rescreen_flags DROP COLUMN resolved_by;
-- +goose StatementEnd
