-- Lets an operator add/rotate a vendor's own credentials (Tronsell,
-- Netts, CatFee) through the ops console instead of editing env vars and
-- redeploying -- see docs/03-build/ops-console-build-prompts.md's own
-- OC.10. cmd/brokerd's own providersFromEnv() seeds this table from
-- whatever env vars are already set on first boot after this migration
-- (a one-time bootstrap, never overwriting a row that already exists),
-- then reads from here on every subsequent boot -- env vars remain the
-- disaster-recovery path if this table is ever lost, never removed.
--
-- api_secret is nullable: CatFee and Tronsell both need one, Netts does
-- not (X-API-KEY only). Values are stored in plain text, matching this
-- whole project's existing posture on every other credential (env vars
-- in cleartext, no secrets manager, no KMS-at-rest) -- not a new risk
-- this migration introduces, an existing one it inherits.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE provider_credentials (
    provider_name text PRIMARY KEY CHECK (provider_name IN ('tronsell', 'netts', 'catfee')),
    base_url      text,
    api_key       text NOT NULL CHECK (api_key <> ''),
    api_secret    text,
    real_ip       text, -- Netts-only: the whitelisted egress IP it requires as X-Real-IP
    enabled       boolean NOT NULL DEFAULT true,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    text NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE provider_credentials;
-- +goose StatementEnd
