//go:build integration

// Requires a real, reachable Postgres 16 instance (LEDGER_TEST_DATABASE_URL)
// AND a sibling checkout of the ledger module at ../../../ledger -- the
// same usdt-settlement-corridor layout this whole project already uses.
// Uses internal/testledger to build and run the REAL ledgerd binary as a
// subprocess and drive it purely over HTTP -- the C3.3 build spec's own
// final acceptance criterion: "a live integration test against a real
// C1 instance confirms a genuinely funded order appears within one poll
// interval and its sender_address round-trips correctly from what C2
// originally observed." Run via `make test-integration` (build tag
// "integration").
package ledgerclient_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"screening/internal/ledgerclient"
	"screening/internal/testledger"
	"screening/internal/verdict"
)

const (
	ledgerListenAddr = ":18436"
	ledgerAPIToken   = "c33-integration-test-token"
	ledgerActor      = "screening"
)

// TestPollFundedOrdersAndGetSenderAddress_AgainstRealC1 is C3.3's own
// final acceptance criterion: a genuinely funded order appears via
// PollFundedOrders, and GetSenderAddress returns exactly the address
// that was reported at funding time -- against a real, running C1, not
// a fake standing in for one.
func TestPollFundedOrdersAndGetSenderAddress_AgainstRealC1(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c33-discovery-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c33-discovery-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F0"

	order := ll.CreateOrder(externalID, customerID)
	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)

	// Before funding: not visible under state=funded at all.
	refsBefore, _, err := client.PollFundedOrders(context.Background(), "")
	if err != nil {
		t.Fatalf("PollFundedOrders (before funding): %v", err)
	}
	for _, ref := range refsBefore {
		if ref.ExternalID == externalID {
			t.Fatalf("order %s appeared in PollFundedOrders before it was ever funded", externalID)
		}
	}

	ll.FundOrder(order, customerID, sender)

	// Within one poll (no interval to wait on -- PollFundedOrders is a
	// single synchronous call, not the loop): the order must now appear.
	refs, _, err := client.PollFundedOrders(context.Background(), "")
	if err != nil {
		t.Fatalf("PollFundedOrders (after funding): %v", err)
	}
	var found bool
	for _, ref := range refs {
		if ref.ExternalID == externalID {
			found = true
			if ref.OrderID != order.ID {
				t.Fatalf("PollFundedOrders returned order_id %d for %s, want %d", ref.OrderID, externalID, order.ID)
			}
		}
	}
	if !found {
		t.Fatalf("order %s did not appear in PollFundedOrders after being funded", externalID)
	}

	got, err := client.GetSenderAddress(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetSenderAddress: %v", err)
	}
	if got != sender {
		t.Fatalf("GetSenderAddress = %q, want %q (the address reported at funding time)", got, sender)
	}
}

// TestPollFundedOrders_CursorNeverReturnsAnAlreadySeenOrder proves the
// cursor this client threads through is genuinely forward-only against a
// real C1: paging across several funded orders never repeats one.
func TestPollFundedOrders_CursorNeverReturnsAnAlreadySeenOrder(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	customerID := "c33-cursor-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F1"

	var externalIDs []string
	for i := 0; i < 3; i++ {
		externalID := fmt.Sprintf("c33-cursor-%d-%d", time.Now().UnixNano(), i)
		order := ll.CreateOrder(externalID, customerID)
		ll.FundOrder(order, customerID, sender)
		externalIDs = append(externalIDs, externalID)
		time.Sleep(2 * time.Millisecond)
	}

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		refs, newCursor, err := client.PollFundedOrders(context.Background(), cursor)
		if err != nil {
			t.Fatalf("PollFundedOrders: %v", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			if seen[ref.ExternalID] {
				t.Fatalf("order %s returned twice across pages", ref.ExternalID)
			}
			seen[ref.ExternalID] = true
		}
		cursor = newCursor
	}

	for _, externalID := range externalIDs {
		if !seen[externalID] {
			t.Fatalf("order %s (funded) was never returned by any page", externalID)
		}
	}
}

func TestReportVerdict_PassTransitionsToScreened(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c34-pass-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-pass-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.CreateOrder(externalID, customerID)
	ll.FundOrder(order, customerID, "0xSenderPass0000000000000000000001")

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	decision := verdict.Decision{Classification: verdict.Pass, ReasonCode: verdict.ReasonPass, ScreeningResultID: 1}
	if err := client.ReportVerdict(context.Background(), externalID, decision); err != nil {
		t.Fatalf("ReportVerdict: %v", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state after a Pass verdict = %q, want screened", after.State)
	}
}

func TestReportVerdict_HoldTransitionsToHeld(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c34-hold-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-hold-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.CreateOrder(externalID, customerID)
	ll.FundOrder(order, customerID, "0xSenderHold0000000000000000000002")

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	decision := verdict.Decision{Classification: verdict.Hold, ReasonCode: verdict.ReasonHoldFlagged, ScreeningResultID: 2}
	if err := client.ReportVerdict(context.Background(), externalID, decision); err != nil {
		t.Fatalf("ReportVerdict: %v", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "held" {
		t.Fatalf("order state after a Hold verdict = %q, want held", after.State)
	}
}

// TestReportVerdict_ReplaySurfacesIllegalTransitionSafely is C3.4's own
// acceptance criterion ("replaying the same queue row... does not
// attempt a second, conflicting transition"), verified against what a
// real C1 actually does -- which is NOT the idempotent no-op invariant
// 3 assumes.
//
// A real finding from this test: orders.Transition's own replay
// detection (replayIfAlreadyPosted, ledger/internal/orders/store.go)
// only fires "if toState == current.State && p.Entry != nil" -- it is
// keyed off journal.GetEntryByIdempotencyKey, so it only ever applies to
// transitions that post an entry. funded->screened and funded->held
// (C1.5's own table) are BOTH RequiresEntry: false -- so there is no
// entry for C1 to recognize a repeat by, and the Idempotency-Key header
// this client sends is required by C1.8's middleware but never actually
// consulted for these two transitions. A second identical ReportVerdict
// call does not replay: it fails with illegal_transition, because the
// order already left funded after the first call succeeded.
//
// This is still safe in practice, just for a different reason than
// invariant 3 describes: illegal_transition is exactly the signal
// internal/pipeline already treats as "done, no retry" for the
// legitimate-race case (a customer cancel landing first) -- a replay
// after a completed report is indistinguishable from that case at this
// layer, and is absorbed the same safe way. But it is not the
// idempotent dedup invariant 3 promises, and C1 has no mechanism today
// to give it for an entry-less transition -- worth flagging to whoever
// owns C1, not something this client can manufacture on its own.
func TestReportVerdict_ReplaySurfacesIllegalTransitionSafely(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c34-replay-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-replay-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.CreateOrder(externalID, customerID)
	ll.FundOrder(order, customerID, "0xSenderReplay000000000000000003")

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	decision := verdict.Decision{Classification: verdict.Pass, ReasonCode: verdict.ReasonPass, ScreeningResultID: 3}

	if err := client.ReportVerdict(context.Background(), externalID, decision); err != nil {
		t.Fatalf("ReportVerdict (first): %v", err)
	}
	replayErr := client.ReportVerdict(context.Background(), externalID, decision)
	if !errors.Is(replayErr, ledgerclient.ErrIllegalTransition) {
		t.Fatalf("ReportVerdict (replay) = %v, want an error wrapping ledgerclient.ErrIllegalTransition (see this test's own doc comment)", replayErr)
	}

	// The important safety property: the replay did not corrupt or
	// double-apply anything -- the order is exactly where the first,
	// successful call left it.
	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state after a rejected replay = %q, want screened (unchanged from the first call)", after.State)
	}
}

// TestReportVerdict_IllegalTransitionIsNotRetried proves the order
// already having left funded (a legitimate race, e.g. cancelled first)
// surfaces as ledgerclient.ErrIllegalTransition, distinguishable from
// every other failure so the pipeline can mark the row DONE without
// retry-storming.
func TestReportVerdict_IllegalTransitionIsNotRetried(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c34-illegal-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-illegal-cust-" + fmt.Sprint(time.Now().UnixNano())
	// Deliberately never funded -- still `quoted`, so funded->screened
	// is illegal from here.
	ll.CreateOrder(externalID, customerID)

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	decision := verdict.Decision{Classification: verdict.Pass, ReasonCode: verdict.ReasonPass, ScreeningResultID: 4}
	err := client.ReportVerdict(context.Background(), externalID, decision)
	if !errors.Is(err, ledgerclient.ErrIllegalTransition) {
		t.Fatalf("ReportVerdict = %v, want an error wrapping ledgerclient.ErrIllegalTransition", err)
	}
}

// TestReportVerdict_SucceedsWhileHalted confirms, against a real C1,
// what the C3.4 build spec itself predicted: funded->screened and
// funded->held are NOT halt-blocked pairs in C1.5's transition table, so
// halting the ledger has no effect on ReportVerdict at all -- it
// succeeds normally. ErrUnexpectedHalt exists in this client as a
// defensive belt-and-suspenders for a case that cannot currently be
// triggered (there is no way to force C1 to return system_halted for
// either of these transitions as the transition table stands today);
// this test proves the actual, reachable behavior instead of asserting
// something the real system structurally cannot produce.
func TestReportVerdict_SucceedsWhileHalted(t *testing.T) {
	ll := testledger.Start(t, ledgerListenAddr, ledgerAPIToken, ledgerActor)
	externalID := "c34-halted-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-halted-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.CreateOrder(externalID, customerID)
	ll.FundOrder(order, customerID, "0xSenderHalted00000000000000000004")

	ll.SetHalt(true, "c34 test: confirming funded->screened ignores halt")
	t.Cleanup(func() { ll.SetHalt(false, "") })

	client := ledgerclient.New(ll.BaseURL(), ledgerAPIToken)
	decision := verdict.Decision{Classification: verdict.Pass, ReasonCode: verdict.ReasonPass, ScreeningResultID: 5}

	if err := client.ReportVerdict(context.Background(), externalID, decision); err != nil {
		t.Fatalf("ReportVerdict while halted: %v (want success -- funded->screened is not halt-blocked)", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state after ReportVerdict while halted = %q, want screened", after.State)
	}
}
