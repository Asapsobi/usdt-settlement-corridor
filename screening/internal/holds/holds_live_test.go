//go:build integration

// Requires a real, reachable Postgres 16 instance (both
// SCREENING_TEST_DATABASE_URL and LEDGER_TEST_DATABASE_URL) AND a
// sibling checkout of the ledger module -- see internal/testledger. Run
// via `make test-integration` (build tag "integration").
//
// This is C3.6's own headline acceptance criterion: "Release against a
// real C1 instance: order moves held -> screened, C1's order_transitions
// row records the reviewer as actor (pending the verification flagged
// in 'Read this second' -- if C1 does NOT support a caller-supplied
// actor, this test is the one that discovers it, and this chunk is not
// done until that's resolved one way or the other)."
//
// It already IS resolved, discovered while building C3.4 against this
// exact same real ledgerd: C1's transitions endpoint has no actor field
// in its request DTO and rejects unknown JSON fields outright
// (DisallowUnknownFields). This test reconfirms that fact in Release's
// own code path (a different transition, held->screened, than C3.4's
// funded->screened/held) rather than assuming C3.4's finding
// generalizes without checking, and proves the fallback design the C3
// build spec itself named actually works: the reviewer's identity ends
// up in holds.resolved_by (C3's own database), never on C1's own
// order_transitions.actor.
package holds_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"screening/internal/holds"
	"screening/internal/ledgerclient"
	"screening/internal/testledger"
)

const (
	holdsLedgerListenAddr = ":18438"
	holdsLedgerAPIToken   = "c36-integration-test-token"
	holdsLedgerActor      = "screening"
)

func TestRelease_AgainstRealC1_OrderMovesToScreenedActorIsServiceReviewerIsLocal(t *testing.T) {
	pool := testPool(t)
	ll := testledger.Start(t, holdsLedgerListenAddr, holdsLedgerAPIToken, holdsLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), holdsLedgerAPIToken)

	externalID := "c36-release-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c36-release-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xHoldReleaseSenderLive000000001"

	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, sender)

	// Get the order into `held` first, the same way C3.4 would (a Hold
	// verdict reported to C1) -- Release only makes sense from held.
	heldDecision := map[string]any{
		"to_state": "held", "expected_version": funded.Version,
		"reason": "screening_hold_flagged", "occurred_at": time.Now().UTC(),
	}
	resp, body := ll.Do("POST", fmt.Sprintf("/v1/orders/%s/transitions", externalID),
		"screening:hold:"+fmt.Sprint(funded.ID)+":1", heldDecision)
	if resp.StatusCode != 200 {
		t.Fatalf("moving order to held: status %d: %s", resp.StatusCode, body)
	}

	decision := holdDecision(t, pool)
	h, err := holds.Open(context.Background(), pool, ledgerclient.OrderRef{OrderID: funded.ID, ExternalID: externalID}, decision)
	if err != nil {
		t.Fatalf("holds.Open: %v", err)
	}

	const reviewer = "operator:alice"
	if err := holds.Release(context.Background(), pool, client, h.ID, reviewer, "manual review: looks legitimate"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state = %q, want screened", after.State)
	}

	actor := ll.LatestTransitionActor(funded.ID)
	if actor == reviewer {
		t.Fatalf("C1's order_transitions.actor = %q -- C1 apparently DOES accept a caller-supplied actor now; "+
			"this contradicts C3.4's own finding and ReleaseHold's design should be revisited to actually pass the reviewer through", actor)
	}

	got, err := holds.Get(context.Background(), pool, h.ID)
	if err != nil {
		t.Fatalf("holds.Get: %v", err)
	}
	if got.Status != holds.StatusReleased {
		t.Fatalf("hold status = %v, want StatusReleased", got.Status)
	}
	if got.ResolvedBy == nil || *got.ResolvedBy != reviewer {
		t.Fatalf("hold resolved_by = %v, want %q -- the reviewer's identity must live in C3's own record", got.ResolvedBy, reviewer)
	}
}
