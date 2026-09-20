-- OC.19: the admin key-issuance view needs to show a masked hint of
-- the current key without ever storing (or being able to recover) the
-- raw value -- api_key_hash alone (a full SHA-256 digest) can't serve
-- that, since it isn't a truncation of the plaintext. Nullable: rows
-- created before this migration simply show no last4 until their key
-- is next rotated.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers ADD COLUMN api_key_last4 text;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE customers DROP COLUMN api_key_last4;
-- +goose StatementEnd
