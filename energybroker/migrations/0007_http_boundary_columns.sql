-- Two columns C4.8's own HTTP boundary needs that no earlier chunk had
-- a reason to add yet:
--
--   reservations.via_fast_path: null until CONFIRMED (a PENDING or
--   FAILED reservation never resolved to either path), true/false once
--   it does. Needed for both GET /v1/system/invariants' own "fast-path
--   vs slow-path ratio" and the reservations_total{fast_path,slow_path,
--   failed} metric this chunk's own acceptance criteria name -- a
--   reactive Prometheus counter still needs this recorded durably so a
--   restart-recovery or an ops query can reconstruct the same ratio
--   independently of whatever the counter itself currently reads.
--
--   manual_fallback_events.resolved_by: this chunk's own
--   POST /v1/manual-fallback-events/{id}/resolve body is explicitly
--   "resolution, actor" per the build spec -- Router.Resolve (C4.6) took
--   only a resolution string because no caller needed an actor before
--   this chunk exposed the endpoint a human actually calls through.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE reservations ADD COLUMN via_fast_path boolean;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE manual_fallback_events ADD COLUMN resolved_by text;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE manual_fallback_events DROP COLUMN resolved_by;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE reservations DROP COLUMN via_fast_path;
-- +goose StatementEnd
