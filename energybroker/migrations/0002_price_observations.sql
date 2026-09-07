-- price_observations is append-only, the same audit instinct as C1's
-- journal and C3's screening_results: every poll tick's result is a new
-- row, never an update to an old one -- "was this vendor actually
-- reliable" analysis (this chunk's own build spec) is only answerable
-- later if every observation this component ever made is still there.
--
-- max_units mirrors internal/provider.Quote.MaxUnitsAvailable exactly --
-- a plain int64 on that struct, with no "unset" representation of its
-- own (some providers cap a single call's size, others report a large
-- enough number that it never binds) -- so this column stores whatever
-- value the provider actually returned, not null: there is nothing for
-- null to mean that a real, very large max_units value doesn't already
-- mean.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE price_observations (
    id             bigserial primary key,
    provider_name  text not null CHECK (provider_name <> ''),
    price_sun      double precision not null CHECK (price_sun >= 0),
    observed_at    timestamptz not null,
    max_units      bigint not null,
    created_at     timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves CurrentPrice's "freshest observation for this provider" lookup
-- directly off the index, without a table scan -- the same shape as
-- screening's own idx_screening_results_lookup.
CREATE INDEX idx_price_observations_lookup
    ON price_observations (provider_name, observed_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE price_observations;
-- +goose StatementEnd
