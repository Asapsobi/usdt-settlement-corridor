package halt

import (
	"context"
	"errors"
	"testing"
)

// Clear validates before touching its Executor at all, so a nil Executor
// is safe for these two cases: no method on it is ever called.

func TestClearRejectsEmptyActor(t *testing.T) {
	err := Clear(context.Background(), nil, ClearParams{Actor: "", Note: "looks fine now"})
	if !errors.Is(err, ErrClearRequiresOperator) {
		t.Fatalf("got %v, want ErrClearRequiresOperator", err)
	}
}

func TestClearRejectsEmptyNote(t *testing.T) {
	err := Clear(context.Background(), nil, ClearParams{Actor: "ops:alice", Note: ""})
	if !errors.Is(err, ErrClearRequiresOperator) {
		t.Fatalf("got %v, want ErrClearRequiresOperator", err)
	}
}
