-- The candidate pipeline (internal/candidates, wired into cmd/watcherd)
-- needs its own persisted "how far have I scanned for Transfer logs"
-- cursor, separate from ingestion_cursor.last_scanned (C2.3's own
-- header/pre-final-reorg bookkeeping). Nullable: NULL means "never run
-- yet" -- watcherd's own wiring starts a fresh cursor from the CURRENT
-- chain tip rather than block 0, so a first boot doesn't try to
-- candidate-scan the entire chain's history.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE ingestion_cursor ADD COLUMN last_candidate_scanned bigint null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE ingestion_cursor DROP COLUMN last_candidate_scanned;
-- +goose StatementEnd
