-- s1_slot_keys is S1's own key registry: which KMS key backs which TRON
-- payout slot. Distinct from, and never imported by, C5's own slot
-- IDENTITY registry (slots.id/tron_address/caps/rotation-status) --
-- see docs/03-build/s1-key-management-build-prompts.md's own §0
-- ("S1 owns slot CUSTODY... C5 owns slot IDENTITY"). tron_address is
-- carried here too (derived once, at Register time, from the KMS
-- public key) purely so SlotAddress can answer without a live KMS
-- round-trip on every call -- C5's own copy of the same address is
-- what it was told when this row was created, never independently
-- re-derived, so the two registries cannot silently disagree (see
-- s1-key-custody-architecture.md's own key-generation step 4).
--
-- The ACTIVE -> RETIRED transition is enforced one-way at the DB level,
-- same trigger-based defense-in-depth instinct as depositwatcher's own
-- watched_addresses_check_transition (migrations/0002_watched_addresses.sql).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE s1_slot_keys (
    slot_id       smallint primary key,
    kms_key_id    text not null unique,
    tron_address  text not null unique,
    public_key    bytea not null,
    status        text not null CHECK (status IN ('ACTIVE', 'RETIRED')),
    created_at    timestamptz not null default now(),
    retired_at    timestamptz
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION s1_slot_keys_check_transition() RETURNS trigger AS $$
BEGIN
    IF (OLD.status, NEW.status) NOT IN (
        ('ACTIVE', 'RETIRED')
    ) THEN
        RAISE EXCEPTION 's1_slot_keys: illegal status transition % -> % for slot %',
            OLD.status, NEW.status, OLD.slot_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER s1_slot_keys_transition_check
    BEFORE UPDATE OF status ON s1_slot_keys
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION s1_slot_keys_check_transition();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER s1_slot_keys_transition_check ON s1_slot_keys;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION s1_slot_keys_check_transition();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE s1_slot_keys;
-- +goose StatementEnd
