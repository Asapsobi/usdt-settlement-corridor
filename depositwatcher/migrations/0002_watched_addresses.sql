-- The address book: one watch-only BEP20 address per order, its lifecycle
-- status, and the sequence that guarantees a derivation index is never
-- reused -- see internal/addresses/store.go's Assign for how it's spent.
--
-- Unlike C1's tables, this one is legitimately mutable: an address's
-- status moves WATCHING -> FUNDED -> RETIRED (or WATCHING -> RETIRED
-- directly, or FUNDED -> WATCHING on a pre-dispatch reorg, once C2.6
-- drives that transition) over its life. 0001's watcher_writer role
-- already has UPDATE via its default-privilege grant, so no extra GRANT
-- is needed here.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE address_status AS ENUM ('WATCHING', 'FUNDED', 'RETIRED');
-- +goose StatementEnd

-- +goose StatementBegin
-- MAXVALUE is 2^31 - 1: internal/addresses.DeriveAddress rejects any index
-- at or above 2^31 (BIP32's hardened-derivation boundary, which requires a
-- private key this package never holds) -- the sequence is capped here so
-- it can never hand out a value DeriveAddress would then refuse.
CREATE SEQUENCE watched_addresses_derivation_index_seq
    START WITH 0
    MINVALUE 0
    MAXVALUE 2147483647;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE watched_addresses (
    id                bigserial primary key,
    address           text not null unique,
    derivation_index  bigint not null unique
        CHECK (derivation_index >= 0 AND derivation_index < 2147483648),
    order_id          bigint not null unique,   -- C1's internal order id
    external_id       text not null unique,
    customer_id       text not null,
    status            address_status not null default 'WATCHING',
    quoted_at         timestamptz not null,
    quote_expires_at  timestamptz not null,
    assigned_at       timestamptz not null default now(),
    retired_at        timestamptz null,
    retired_reason    text null                 -- settled | refunded | expired | superseded
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Enforces the legal transition set at the database level, independent of
-- whatever calls internal/addresses.go's own Go API -- same defense-in-
-- depth posture as C1's journal_check_balance trigger. Only fires when
-- status actually changes (the WHEN clause below), so an update that
-- touches other columns without changing status is never blocked here.
CREATE FUNCTION watched_addresses_check_transition() RETURNS trigger AS $$
BEGIN
    IF (OLD.status, NEW.status) NOT IN (
        ('WATCHING', 'FUNDED'),
        ('WATCHING', 'RETIRED'),
        ('FUNDED', 'RETIRED'),
        ('FUNDED', 'WATCHING')
    ) THEN
        RAISE EXCEPTION 'watched_addresses: illegal status transition % -> % for order %',
            OLD.status, NEW.status, OLD.order_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER watched_addresses_transition_check
    BEFORE UPDATE OF status ON watched_addresses
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION watched_addresses_check_transition();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER watched_addresses_transition_check ON watched_addresses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION watched_addresses_check_transition();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE watched_addresses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP SEQUENCE watched_addresses_derivation_index_seq;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE address_status;
-- +goose StatementEnd
