-- Idempotency keys follow the convention documented in
-- internal/journal/journal.go: "<producer>:<domain>:<natural-id>", capped
-- at 255 bytes. That cap is enforced in Go (validateIdempotencyKey) before
-- every write; this CHECK constraint is the same defense-in-depth already
-- applied elsewhere in this schema (accounts.normal_side's CHECK, the
-- deferred journal balance trigger) -- it holds even against raw SQL that
-- bypasses Go entirely.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE journal_entries
    ADD CONSTRAINT idempotency_key_max_length
    CHECK (char_length(idempotency_key) <= 255);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE journal_entries DROP CONSTRAINT idempotency_key_max_length;
-- +goose StatementEnd
