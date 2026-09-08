package slots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"dispatcher/internal/money"
)

// BalanceReader is the one call this package needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
// Defined here (the consumer), matching this project's established
// convention.
type BalanceReader interface {
	GetAccountBalance(ctx context.Context, accountCode string) (money.Amount, error)
}

// Caps are the two ceilings decision 4 sets per slot -- config, not a
// constant, so a real cap change never needs a redeploy.
type Caps struct {
	BalanceCeiling money.Amount
	TxCountCeiling int64
}

// ErrNoEligibleSlot is SelectForDispatch's own result when every ACTIVE
// slot is over at least one cap -- the C5-side version of C4's own
// fallback_ladder signal: a real condition to alert on, never a silent
// block-forever.
var ErrNoEligibleSlot = errors.New("slots: no eligible slot under both caps")

// accountCode is the account-code convention C1's chart of accounts
// already defines for a payout slot (c1-ledger-build-prompts.md §B:
// "CR asset:tron:slot:3").
func accountCode(slotID int) string {
	return fmt.Sprintf("asset:tron:slot:%d", slotID)
}

// SelectForDispatch lists every ACTIVE slot, checks each one's REAL,
// current balance (never a locally cached value -- invariant 5) against
// caps.BalanceCeiling, and each one's own tx_count (already current in
// this table) against caps.TxCountCeiling, concurrently (six slots, this
// is cheap). Among slots under BOTH ceilings, picks the least-recently-
// dispatched-from one (NULL LastDispatchAt -- never yet used -- sorts
// first), a deterministic, tested tie-break rather than map-iteration
// order, and records this call's own choice as that slot's new
// LastDispatchAt.
func (s *Store) SelectForDispatch(ctx context.Context, balances BalanceReader, caps Caps) (Slot, error) {
	active, err := s.List(ctx, StatusActive)
	if err != nil {
		return Slot{}, fmt.Errorf("slots: listing ACTIVE slots: %w", err)
	}
	if len(active) == 0 {
		return Slot{}, ErrNoEligibleSlot
	}

	type checked struct {
		slot    Slot
		balance money.Amount
		err     error
	}
	results := make([]checked, len(active))
	var wg sync.WaitGroup
	for i, slot := range active {
		wg.Add(1)
		go func(i int, slot Slot) {
			defer wg.Done()
			balance, err := balances.GetAccountBalance(ctx, accountCode(slot.ID))
			results[i] = checked{slot: slot, balance: balance, err: err}
		}(i, slot)
	}
	wg.Wait()

	var eligible []Slot
	for _, r := range results {
		if r.err != nil {
			return Slot{}, fmt.Errorf("slots: checking balance for slot %d: %w", r.slot.ID, r.err)
		}
		if r.balance >= caps.BalanceCeiling {
			continue
		}
		if r.slot.TxCount >= caps.TxCountCeiling {
			continue
		}
		eligible = append(eligible, r.slot)
	}
	if len(eligible) == 0 {
		return Slot{}, ErrNoEligibleSlot
	}

	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i].LastDispatchAt, eligible[j].LastDispatchAt
		switch {
		case a == nil && b == nil:
			return eligible[i].ID < eligible[j].ID // both never used -- deterministic by id
		case a == nil:
			return true // never-used sorts before any real timestamp
		case b == nil:
			return false
		case !a.Equal(*b):
			return a.Before(*b)
		default:
			return eligible[i].ID < eligible[j].ID
		}
	})
	chosen := eligible[0]

	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `UPDATE slots SET last_dispatch_at = $1 WHERE id = $2`, now, chosen.ID); err != nil {
		return Slot{}, fmt.Errorf("slots: recording dispatch selection for slot %d: %w", chosen.ID, err)
	}
	chosen.LastDispatchAt = &now
	return chosen, nil
}
