package journal

import (
	"context"
	"errors"
	"testing"

	"ledger/internal/money"
)

// These two cases return before validate ever touches the database (the
// line-count check runs first), so a nil Queryer is safe: no method on it
// is ever called.

func TestValidateRejectsZeroLines(t *testing.T) {
	_, err := validate(context.Background(), nil, EntryRequest{})
	if !errors.Is(err, ErrTooFewLines) {
		t.Fatalf("got %v, want ErrTooFewLines", err)
	}
}

func TestValidateRejectsSingleLine(t *testing.T) {
	req := EntryRequest{
		Lines: []Line{
			{AccountCode: "revenue:fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: 100}},
		},
	}
	_, err := validate(context.Background(), nil, req)
	if !errors.Is(err, ErrTooFewLines) {
		t.Fatalf("got %v, want ErrTooFewLines", err)
	}
}
