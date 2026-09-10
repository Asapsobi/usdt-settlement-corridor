-- C6.6: webhook_secret is the per-customer HMAC-SHA256 signing key
-- webhook_deliveries signs every payload with. Stored in plaintext
-- (unlike api_key_hash) because this service must itself compute a
-- real HMAC with it at delivery time -- it is a shared secret this
-- gateway is one party to, not a credential only ever verified, the
-- same distinction that makes api_key_hash a one-way hash but this
-- column plain text.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE customers ADD COLUMN webhook_secret text NOT NULL DEFAULT '';
ALTER TABLE customers ALTER COLUMN webhook_secret DROP DEFAULT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE customers DROP COLUMN webhook_secret;
-- +goose StatementEnd
