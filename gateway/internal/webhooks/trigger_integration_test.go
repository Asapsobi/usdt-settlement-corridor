//go:build integration

package webhooks

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/customers"
	"gateway/internal/money"
)

// fakePoller stands in for c1client.Client's own ListOrdersByState.
// gateway_test is a real, persistent database shared across test runs
// (see C6.2's own "no reset between tests" note), so webhook_cursors'
// own rows may already be well past "" by the time a given test runs
// -- this fake deliberately does NOT gate its own byState results on
// the cursor it's called with (always returns the same fixed page,
// with a freshly incrementing cursor each call), so a test's
// assertions rely on Enqueue's own (external_id, event_type)
// uniqueness for dedup, not on cursor state this fake doesn't control.
type fakePoller struct {
	byState map[string][]c1client.Order
	calls   map[string]int
}

func newFakePoller() *fakePoller {
	return &fakePoller{byState: map[string][]c1client.Order{}, calls: map[string]int{}}
}

func (f *fakePoller) ListOrdersByState(ctx context.Context, state, cursor string) ([]c1client.Order, string, error) {
	f.calls[state]++
	return f.byState[state], fmt.Sprintf("cursor-%s-%d", state, f.calls[state]), nil
}

func TestTrigger_EnqueuesOncePerTransitionAndSkipsNoURLCustomers(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	custStore := customers.NewStore(pool)

	withURLCust, _, err := custStore.Create(t.Context(), "trigger-withurl-co")
	if err != nil {
		t.Fatalf("Create withURLCust: %v", err)
	}
	if err := custStore.SetWebhookURL(t.Context(), withURLCust.ID, "https://example.test/webhook"); err != nil {
		t.Fatalf("SetWebhookURL: %v", err)
	}
	noURLCust, _, err := custStore.Create(t.Context(), "trigger-nourl-co")
	if err != nil {
		t.Fatalf("Create noURLCust: %v", err)
	}

	poller := newFakePoller()
	extWithURL := fmt.Sprintf("ext-trigger-withurl-%d", time.Now().UnixNano())
	extNoURL := fmt.Sprintf("ext-trigger-nourl-%d", time.Now().UnixNano())
	poller.byState["settled"] = []c1client.Order{
		{ExternalID: extWithURL, CustomerID: strconv.FormatInt(withURLCust.ID, 10), Tier: "STANDARD", AmountIn: money.Amount(3000_000000), AmountOut: money.Amount(2990_700000), State: "settled"},
		{ExternalID: extNoURL, CustomerID: strconv.FormatInt(noURLCust.ID, 10), Tier: "STANDARD", AmountIn: money.Amount(500_000000), AmountOut: money.Amount(497_500000), State: "settled"},
	}

	trigger := NewTrigger(pool, poller, store, custStore)

	if err := trigger.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	withURLRow, err := findByExternalID(t, pool, extWithURL, "settled")
	if err != nil {
		t.Fatalf("expected a delivery enqueued for the customer with a webhook_url: %v", err)
	}
	if withURLRow.CustomerID != withURLCust.ID {
		t.Errorf("CustomerID = %d, want %d", withURLRow.CustomerID, withURLCust.ID)
	}

	if _, err := findByExternalID(t, pool, extNoURL, "settled"); err == nil {
		t.Errorf("a delivery was enqueued for a customer with no webhook_url configured, want none")
	}

	// A second tick, C1 (this fake) still reporting the very same order
	// settled -- must not enqueue a duplicate row (Enqueue's own
	// (external_id, event_type) uniqueness, ON CONFLICT DO NOTHING) or
	// surface the conflict as an error.
	if err := trigger.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if poller.calls["settled"] < 2 {
		t.Errorf("poller.calls[settled] = %d, want at least 2 (both ticks polled)", poller.calls["settled"])
	}
	stillOneRow, err := findByExternalID(t, pool, extWithURL, "settled")
	if err != nil {
		t.Fatalf("findByExternalID after second tick: %v", err)
	}
	if stillOneRow.ID != withURLRow.ID {
		t.Errorf("a second tick produced a different delivery row (id %d vs %d) -- duplicate enqueued", stillOneRow.ID, withURLRow.ID)
	}
}

func TestTrigger_PollsAllThreeWatchedStatesIndependently(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	custStore := customers.NewStore(pool)

	poller := newFakePoller()
	trigger := NewTrigger(pool, poller, store, custStore)

	if err := trigger.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	for _, state := range []string{"settled", "held", "refunded"} {
		if poller.calls[state] != 1 {
			t.Errorf("poller.calls[%s] = %d, want 1", state, poller.calls[state])
		}
	}
}
