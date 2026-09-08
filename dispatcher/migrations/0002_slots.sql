-- slots is C5's own slot-IDENTITY registry (per decision 4: six
-- segregated payout slots) -- distinct from, and never sharing a
-- database or module with, S1's own slot-CUSTODY registry
-- (s1_slot_keys). This table owns which address a slot is, its lifecycle
-- status, and its own running tx_count (never reset on rotation start,
-- per this chunk's own build spec); S1 owns which KMS key can sign for
-- that address. C5 asks S1 for the address (GET /v1/slots/{id}/address)
-- once, at registration time, and stores it here -- the two registries
-- are never allowed to silently disagree because C5 never re-derives an
-- address, only ever receives one from S1.
--
-- ACTIVE -> RETIRING -> RETIRED is the normal cap-exhaustion path,
-- enforced one-way at the DB level, same trigger-based defense-in-depth
-- as every other lifecycle table in this project (depositwatcher's
-- watched_addresses, s1's s1_slot_keys). ACTIVE -> RETIRED directly is
-- also allowed -- C5.9's own mid-flight-freeze handling retires a slot
-- outright the moment a freeze is detected ("not just RETIRING -- a
-- frozen slot is not coming back"), skipping the graceful drain
-- RETIRING exists for, since there is nothing to gracefully finish.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE slots (
    id            smallint primary key,
    tron_address  text not null unique,
    status        text not null CHECK (status IN ('ACTIVE', 'RETIRING', 'RETIRED')),
    activated_at  timestamptz not null,
    retired_at    timestamptz,
    tx_count      bigint not null default 0
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION slots_check_transition() RETURNS trigger AS $$
BEGIN
    IF (OLD.status, NEW.status) NOT IN (
        ('ACTIVE', 'RETIRING'),
        ('RETIRING', 'RETIRED'),
        ('ACTIVE', 'RETIRED')
    ) THEN
        RAISE EXCEPTION 'slots: illegal status transition % -> % for slot %',
            OLD.status, NEW.status, OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER slots_transition_check
    BEFORE UPDATE OF status ON slots
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION slots_check_transition();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER slots_transition_check ON slots;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION slots_check_transition();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE slots;
-- +goose StatementEnd
