-- The order state machine. orders is the second (and last) deliberately
-- mutable table in this schema, alongside account_balances -- its state,
-- version, and updated_at change via Transition's optimistic-concurrency
-- UPDATE, which is exactly why it needs the same explicit UPDATE grant
-- account_balances needed in 0004.
--
-- order_transitions is append-only, same enforcement as the journal: it
-- reuses journal_forbid_modification (defined in 0003), which is purely
-- generic despite its name -- it already keys off TG_TABLE_NAME/TG_OP
-- rather than any journal-specific logic, so there is nothing
-- journal-specific to duplicate for a second table.
--
-- journal_entries.order_id has been a bare nullable bigint with no FK
-- since 0003, by design: "FK added in C1.5" was written into that
-- migration's comment because orders didn't exist yet. It exists now.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE order_state AS ENUM
    ('quoted', 'funded', 'screened', 'dispatching', 'settled', 'held', 'refunded', 'expired');
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TYPE order_tier AS ENUM ('DIRECT', 'STANDARD', 'SWEEP');
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE orders (
    id                bigserial primary key,
    external_id       text not null unique,
    customer_id       text not null,
    tier              order_tier not null,
    state             order_state not null,
    amount_in         numeric(38,0) not null,
    amount_out        numeric(38,0) not null,
    fee_units         numeric(38,0) not null,
    network_fee_units numeric(38,0) not null,
    recipient_address text not null,
    quoted_at         timestamptz not null,
    quote_expires_at  timestamptz not null,
    version           integer not null default 0,
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
GRANT UPDATE ON orders TO ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE order_transitions (
    id           bigserial primary key,
    order_id     bigint not null references orders(id),
    from_state   order_state not null,
    to_state     order_state not null,
    entry_id     bigint null references journal_entries(id),
    actor        text not null,
    reason       text not null,
    occurred_at  timestamptz not null,
    recorded_at  timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER order_transitions_append_only
    BEFORE UPDATE OR DELETE ON order_transitions
    FOR EACH ROW EXECUTE FUNCTION journal_forbid_modification();
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE journal_entries
    ADD CONSTRAINT journal_entries_order_id_fkey
    FOREIGN KEY (order_id) REFERENCES orders(id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_order_id_fkey;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER order_transitions_append_only ON order_transitions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE order_transitions;
-- +goose StatementEnd
-- +goose StatementBegin
REVOKE UPDATE ON orders FROM ledger_writer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE orders;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE order_tier;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE order_state;
-- +goose StatementEnd
