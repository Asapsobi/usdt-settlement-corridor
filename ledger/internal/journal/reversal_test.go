package journal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These three cases return before Reverse ever touches the database (the
// parameter validation runs first), so a nil pgx.Tx is safe: no method on
// it is ever called.

func TestReverseRejectsEmptyActor(t *testing.T) {
	_, err := Reverse(context.Background(), nil, 1, "", "reason", time.Now())
	if !errors.Is(err, ErrInvalidReverseParams) {
		t.Fatalf("got %v, want ErrInvalidReverseParams", err)
	}
}

func TestReverseRejectsEmptyReason(t *testing.T) {
	_, err := Reverse(context.Background(), nil, 1, "actor", "", time.Now())
	if !errors.Is(err, ErrInvalidReverseParams) {
		t.Fatalf("got %v, want ErrInvalidReverseParams", err)
	}
}

func TestReverseRejectsZeroOccurredAt(t *testing.T) {
	_, err := Reverse(context.Background(), nil, 1, "actor", "reason", time.Time{})
	if !errors.Is(err, ErrInvalidReverseParams) {
		t.Fatalf("got %v, want ErrInvalidReverseParams", err)
	}
}
