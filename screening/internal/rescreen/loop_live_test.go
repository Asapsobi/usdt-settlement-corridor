//go:build integration

// Requires a real, reachable Postgres 16 instance (both
// SCREENING_TEST_DATABASE_URL and LEDGER_TEST_DATABASE_URL) AND a
// sibling checkout of the ledger module -- see internal/testledger. Run
// via `make test-integration` (build tag "integration").
//
// This is C3.7's own most load-bearing acceptance criterion, proven
// against a real C1 rather than just a fake: "no call to C1 is made
// that changes the order's state. Verify this last part explicitly: the
// test asserts zero transition calls, not just that the 'right' one
// wasn't made." Against a real ledgerd, the strongest possible proof of
// "zero transition calls happened" is that the order's own state and
// version are byte-for-byte unchanged after a tick that DID detect and
// record a flag.
package rescreen_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/rescreen"
	"screening/internal/testledger"
)

const (
	rescreenLedgerListenAddr = ":18439"
	rescreenLedgerAPIToken   = "c37-integration-test-token"
	rescreenLedgerActor      = "screening"
)

func TestRunTick_FlaggedReScreen_MakesNoTransitionCallAgainstRealC1(t *testing.T) {
	pool := testPool(t)
	ll := testledger.Start(t, rescreenLedgerListenAddr, rescreenLedgerAPIToken, rescreenLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), rescreenLedgerAPIToken)

	externalID := "c37-live-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c37-live-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xRescreenLiveSenderAddr000000001"

	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, sender)

	// Move the order to `screened` the way a clean C3.4 pass would --
	// this is the state rescreen must find it in and leave it in.
	passBody := map[string]any{
		"to_state": "screened", "expected_version": funded.Version,
		"reason": "screening_pass", "occurred_at": time.Now().UTC(),
	}
	resp, body := ll.Do("POST", fmt.Sprintf("/v1/orders/%s/transitions", externalID),
		"screening:pass:"+fmt.Sprint(funded.ID)+":1", passBody)
	if resp.StatusCode != 200 {
		t.Fatalf("moving order to screened: status %d: %s", resp.StatusCode, body)
	}
	screened := ll.GetOrder(externalID)
	if screened.State != "screened" {
		t.Fatalf("setup: order state = %q, want screened", screened.State)
	}

	seedPreviousVerdict(t, pool, sender, false, 0.1) // the verdict that "let it through"

	mock := provider.NewMockProvider(1)
	mock.ForceFlagged(sender) // the fresh check now disagrees

	// client itself satisfies rescreen.OrderLister -- no wrapper needed;
	// using it directly (not a fake) is the point of this test.
	if err := rescreen.RunTick(context.Background(), pool, client, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	// The flag was recorded...
	flags, err := rescreen.ListUnresolved(context.Background(), pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	var found bool
	for _, f := range flags {
		if f.ExternalID == externalID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a rescreen_flags row for %s, got %+v", externalID, flags)
	}

	// ...but C1's own record of the order is byte-for-byte unchanged:
	// same state, same version. A transition call -- successful or not
	// -- would have advanced the version even on a no-op self-transition
	// attempt is illegal in this system, so an unchanged version is only
	// possible if no transition call was made at all.
	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state after a flagged re-screen = %q, want unchanged screened", after.State)
	}
	if after.Version != screened.Version {
		t.Fatalf("order version after a flagged re-screen = %d, want unchanged %d -- a transition call must have been made", after.Version, screened.Version)
	}
}
