//go:build integration

package slots_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dispatcher/internal/money"
	"dispatcher/internal/slots"
)

type fakeBalanceReader struct {
	mu       sync.Mutex
	balances map[string]money.Amount
}

func newFakeBalanceReader() *fakeBalanceReader {
	return &fakeBalanceReader{balances: make(map[string]money.Amount)}
}

func (f *fakeBalanceReader) set(accountCode string, amt money.Amount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[accountCode] = amt
}

func (f *fakeBalanceReader) GetAccountBalance(ctx context.Context, accountCode string) (money.Amount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	amt, ok := f.balances[accountCode]
	if !ok {
		return 0, errors.New("no balance configured")
	}
	return amt, nil
}

func mustParse(t *testing.T, s string) money.Amount {
	t.Helper()
	amt, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return amt
}

func TestSelectForDispatch_EligibleSetIsExactlyUnderBothCaps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)
	balances := newFakeBalanceReader()

	for i := 1; i <= 3; i++ {
		if _, err := store.Create(ctx, i, "TSlot0000000000000000000000000"+string(rune('0'+i)), time.Now()); err != nil {
			t.Fatalf("Create slot %d: %v", i, err)
		}
	}
	balances.set("asset:tron:slot:1", mustParse(t, "1000.000000"))  // well under
	balances.set("asset:tron:slot:2", mustParse(t, "60000.000000")) // over
	balances.set("asset:tron:slot:3", mustParse(t, "2000.000000"))  // well under

	caps := slots.Caps{BalanceCeiling: mustParse(t, "50000.000000"), TxCountCeiling: 5000}

	seen := map[int]bool{}
	for i := 0; i < 10; i++ {
		chosen, err := store.SelectForDispatch(ctx, balances, caps)
		if err != nil {
			t.Fatalf("SelectForDispatch (%d): %v", i, err)
		}
		seen[chosen.ID] = true
	}
	if seen[2] {
		t.Fatal("slot 2 (over the balance ceiling) was selected")
	}
	if !seen[1] || !seen[3] {
		t.Fatalf("expected both eligible slots (1, 3) to be selected across 10 calls, got %v", seen)
	}
}

func TestSelectForDispatch_SlotAtExactlyTheCeilingIsExcluded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)
	balances := newFakeBalanceReader()

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ceiling := mustParse(t, "50000.000000")
	balances.set("asset:tron:slot:1", ceiling) // exactly at the ceiling

	_, err := store.SelectForDispatch(ctx, balances, slots.Caps{BalanceCeiling: ceiling, TxCountCeiling: 5000})
	if !errors.Is(err, slots.ErrNoEligibleSlot) {
		t.Fatalf("SelectForDispatch (balance exactly at ceiling) error = %v, want ErrNoEligibleSlot", err)
	}
}

func TestSelectForDispatch_AllOverCapReturnsErrNoEligibleSlot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)
	balances := newFakeBalanceReader()

	for i := 1; i <= 2; i++ {
		if _, err := store.Create(ctx, i, "TSlotOver00000000000000000000"+string(rune('0'+i)), time.Now()); err != nil {
			t.Fatalf("Create slot %d: %v", i, err)
		}
		balances.set("asset:tron:slot:"+string(rune('0'+i)), mustParse(t, "99999.000000"))
	}

	_, err := store.SelectForDispatch(ctx, balances, slots.Caps{BalanceCeiling: mustParse(t, "50000.000000"), TxCountCeiling: 5000})
	if !errors.Is(err, slots.ErrNoEligibleSlot) {
		t.Fatalf("SelectForDispatch error = %v, want ErrNoEligibleSlot", err)
	}
}

func TestSelectForDispatch_NeverTrustsAStaleCachedBalance(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)
	balances := newFakeBalanceReader()

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	caps := slots.Caps{BalanceCeiling: mustParse(t, "50000.000000"), TxCountCeiling: 5000}

	balances.set("asset:tron:slot:1", mustParse(t, "1000.000000"))
	if _, err := store.SelectForDispatch(ctx, balances, caps); err != nil {
		t.Fatalf("SelectForDispatch (1st, under cap): %v", err)
	}

	balances.set("asset:tron:slot:1", mustParse(t, "60000.000000"))
	if _, err := store.SelectForDispatch(ctx, balances, caps); !errors.Is(err, slots.ErrNoEligibleSlot) {
		t.Fatalf("SelectForDispatch (2nd, now over cap) error = %v, want ErrNoEligibleSlot -- a cached balance would have wrongly succeeded", err)
	}
}

func TestSelectForDispatch_TxCountCeilingExcludes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)
	balances := newFakeBalanceReader()

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.IncrementTxCount(ctx, 1, 5000); err != nil {
		t.Fatalf("IncrementTxCount: %v", err)
	}
	balances.set("asset:tron:slot:1", mustParse(t, "1.000000"))

	_, err := store.SelectForDispatch(ctx, balances, slots.Caps{BalanceCeiling: mustParse(t, "50000.000000"), TxCountCeiling: 5000})
	if !errors.Is(err, slots.ErrNoEligibleSlot) {
		t.Fatalf("SelectForDispatch (at tx-count ceiling) error = %v, want ErrNoEligibleSlot", err)
	}
}
