-- energy_buffer is C4.3's own standing pool of delegatable capacity.
-- Two columns beyond the build spec's own literal table definition are
-- added here, both structurally necessary for the functions that spec
-- itself asks this chunk to build, not scope creep:
--
--   delegation_id: the provider's own opaque id for the specific
--   on-chain delegation this row came from (internal/provider.Delegation.ID).
--   Without it, VerifyOnChain and Reconcile would have no way to ask
--   "is THIS SPECIFIC delegation still present on-chain" -- only "what's
--   the aggregate total for this address right now", which cannot
--   distinguish which of several rows a vendor's early revocation
--   actually hit. Real TRON delegated-resource state (Stake 2.0) is
--   itself tracked per delegation, so this mirrors the chain's own
--   granularity, not an invented abstraction.
--
--   allocation_id: which buffer_allocations row (below) claimed this
--   energy_buffer row, once Reserve moves it from AVAILABLE to
--   RESERVED. Reserve's own signature (orderID, units) and return type
--   (*Allocation) require persisting that association somewhere; a
--   separate join table plus a nullable FK here is this chunk's own
--   choice of where, mirroring the audit-companion-table pattern C3's
--   screening_result_invalidations already established in this project.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE buffer_allocations (
    id           bigserial primary key,
    order_id     bigint not null,
    units        bigint not null CHECK (units > 0),
    reserved_at  timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE energy_buffer (
    id             bigserial primary key,
    provider_name  text not null CHECK (provider_name <> ''),
    delegation_id  text not null CHECK (delegation_id <> ''),
    units          bigint not null CHECK (units > 0),
    acquired_at    timestamptz not null,
    cost_trx       numeric(38,0) not null,
    expires_at     timestamptz not null,
    status         text not null CHECK (status IN ('AVAILABLE', 'RESERVED', 'SPENT', 'EXPIRED')),
    allocation_id  bigint REFERENCES buffer_allocations(id),
    created_at     timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves Reserve's own "oldest-expiry-first AVAILABLE rows" claim
-- directly off the index.
CREATE INDEX idx_energy_buffer_available
    ON energy_buffer (expires_at ASC)
    WHERE status = 'AVAILABLE';
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves Reconcile's own "AVAILABLE or RESERVED rows nearing expiry" scan.
CREATE INDEX idx_energy_buffer_reconcile
    ON energy_buffer (status, expires_at ASC)
    WHERE status IN ('AVAILABLE', 'RESERVED');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE energy_buffer;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE buffer_allocations;
-- +goose StatementEnd
