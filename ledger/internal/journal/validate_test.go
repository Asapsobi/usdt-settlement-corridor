package journal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ledger/internal/money"
)

// These cases return before validate ever touches the database (the
// idempotency-key and line-count checks both run before any DB call), so
// a nil Queryer is safe: no method on it is ever called.

func TestValidateRejectsZeroLines(t *testing.T) {
	_, err := validate(context.Background(), nil, EntryRequest{IdempotencyKey: "test:zero_lines"})
	if !errors.Is(err, ErrTooFewLines) {
		t.Fatalf("got %v, want ErrTooFewLines", err)
	}
}

func TestValidateRejectsSingleLine(t *testing.T) {
	req := EntryRequest{
		IdempotencyKey: "test:single_line",
		Lines: []Line{
			{AccountCode: "revenue:fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: 100}},
		},
	}
	_, err := validate(context.Background(), nil, req)
	if !errors.Is(err, ErrTooFewLines) {
		t.Fatalf("got %v, want ErrTooFewLines", err)
	}
}

func TestValidateIdempotencyKeyEmpty(t *testing.T) {
	_, err := validate(context.Background(), nil, EntryRequest{IdempotencyKey: ""})
	if !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("got %v, want ErrInvalidIdempotencyKey", err)
	}
}

func TestValidateIdempotencyKeyTooLong(t *testing.T) {
	req := EntryRequest{IdempotencyKey: strings.Repeat("x", maxIdempotencyKeyLen+1)}
	_, err := validate(context.Background(), nil, req)
	if !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("got %v, want ErrInvalidIdempotencyKey", err)
	}
}

func TestValidateIdempotencyKeyMaxLengthAccepted(t *testing.T) {
	// Exactly at the limit must pass the key check itself (it will still
	// fail on line count immediately after, which is fine -- this test
	// only cares that the boundary value isn't rejected as "too long").
	req := EntryRequest{IdempotencyKey: strings.Repeat("x", maxIdempotencyKeyLen)}
	_, err := validate(context.Background(), nil, req)
	if errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("a key of exactly %d bytes was rejected as too long", maxIdempotencyKeyLen)
	}
	if !errors.Is(err, ErrTooFewLines) {
		t.Fatalf("got %v, want ErrTooFewLines (key length itself should have passed)", err)
	}
}
