-- Reworks internal/buffer around per-payout-slot pools instead of one
-- shared staging address. See internal/provider.EnergyProvider's own doc
-- comment for the full account: building real HTTP clients against
-- Tronsell's, Netts's, and CatFee's actual APIs found that none of them
-- support retargeting an already-issued delegation to a new address --
-- only "buy a new, fixed-receiver order." Design (b) from C4.4's own
-- open question (retarget a shared staging pool's capacity at
-- reservation time via EnergyProvider.Redelegate) is therefore not
-- buildable against real vendor capabilities. This migration adopts
-- design (a) instead: one independently-sized, independently-replenished
-- pool per known payout slot address, each pre-acquired already pointed
-- at its own final destination, so a reservation's fast path becomes a
-- pure database claim -- no vendor call, no retarget, ever.
--
-- slot_address is added NOT NULL with no default: no chunk ever wired a
-- real EnergyProvider into production (cmd/brokerd/main.go's own doc
-- comment), so no real deployment has ever written a row into this
-- table -- there is nothing to backfill.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE energy_buffer ADD COLUMN slot_address text NOT NULL CHECK (slot_address <> '');
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX idx_energy_buffer_available;
-- +goose StatementEnd

-- +goose StatementBegin
-- Serves Reserve's own "oldest-expiry-first AVAILABLE rows already
-- slotted under the requested target address" claim directly off the
-- index.
CREATE INDEX idx_energy_buffer_available
    ON energy_buffer (slot_address, expires_at ASC)
    WHERE status = 'AVAILABLE';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_energy_buffer_available;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_energy_buffer_available
    ON energy_buffer (expires_at ASC)
    WHERE status = 'AVAILABLE';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE energy_buffer DROP COLUMN slot_address;
-- +goose StatementEnd
