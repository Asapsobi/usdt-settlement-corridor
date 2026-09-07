//go:build integration

// Requires LEDGER_TEST_DATABASE_URL pointing at a real, reachable
// Postgres 16 instance, plus a sibling checkout of the ledger module at
// ../../../ledger (the usdt-settlement-corridor layout this whole
// project uses). Builds and runs a REAL ledgerd binary as a subprocess
// via internal/testledger. Run via `make test-integration`.
//
// LEDGER_TEST_DATABASE_URL is shared, persistent state across repeated
// runs of this file (nothing truncates it between invocations, unlike
// this module's own throwaway BROKER_TEST_DATABASE_URL) -- so every
// external_id and delegation id below is suffixed with a per-run unique
// value (uniqueID), and every account-balance assertion checks the
// DELTA this test's own calls caused, never an absolute value: a fixed
// account like expense:energy accumulates real balance across every
// past run this database has ever seen.
package ledgerclient_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"energybroker/internal/ledgerclient"
	"energybroker/internal/money"
	"energybroker/internal/provider"
	"energybroker/internal/testledger"
)

const (
	testListenAddr = ":18438"
	testToken      = "c4.5-ledgerclient-test-token"
	testActor      = "energy-broker"
)

func setUpAccounts(l *testledger.Ledger) {
	l.CreateAccount("expense:energy", "EXPENSE", "TRX", 1)
	l.CreateAccount("asset:tron:energy_wallet", "ASSET", "TRX", 1)
}

// uniqueID gives each test run its own external_id/delegation id
// namespace, so repeated `go test` invocations against the same
// long-lived LEDGER_TEST_DATABASE_URL never collide on a unique
// constraint or an idempotency key some earlier run already used.
func uniqueID(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestReportEnergyCost_PostsExactE4Entry is this chunk's own first
// acceptance criterion: against a real running C1, a confirmed
// delegation results in exactly the E4 entry landing in C1's journal,
// TRX balances move by the exact delegation cost, order_id correctly
// links it.
func TestReportEnergyCost_PostsExactE4Entry(t *testing.T) {
	l := testledger.Start(t, testListenAddr, testToken, testActor)
	setUpAccounts(l)

	order := l.CreateOrder(uniqueID(t, "c45-order-1"), "cust-c45-1")
	delegationID := uniqueID(t, "live-delegation-1")

	client := ledgerclient.New(l.BaseURL(), l.Token())
	cost, err := money.ParseDecimal("2.438000")
	if err != nil {
		t.Fatalf("ParseDecimal: %v", err)
	}
	delegation := provider.Delegation{
		ID:            delegationID,
		ProviderName:  provider.Tronsell,
		TargetAddress: "TPayoutSlot00000000000000000001",
		EnergyUnits:   65000,
		CostTRX:       cost,
		RequestedAt:   time.Now().UTC(),
	}

	expenseBefore := l.AccountBalance("expense:energy")
	walletBefore := l.AccountBalance("asset:tron:energy_wallet")

	if err := client.ReportEnergyCost(context.Background(), delegation, order.ID); err != nil {
		t.Fatalf("ReportEnergyCost: %v", err)
	}

	if got := l.JournalEntryCount("broker:energy_cost:" + delegationID); got != 1 {
		t.Fatalf("journal_entries rows for this delegation's idempotency key = %d, want exactly 1", got)
	}

	if got := l.AccountBalance("expense:energy") - expenseBefore; got != int64(cost) {
		t.Fatalf("expense:energy balance moved by %d, want exactly %d (the delegation's own cost, in minor units)", got, int64(cost))
	}
	if got := l.AccountBalance("asset:tron:energy_wallet") - walletBefore; got != -int64(cost) {
		t.Fatalf("asset:tron:energy_wallet balance moved by %d, want exactly -%d", got, int64(cost))
	}
}

// TestReportEnergyCost_ReplaySimulatingARestartHitsIdempotencyPathNoDoubleCharge
// is this chunk's own second acceptance criterion.
func TestReportEnergyCost_ReplaySimulatingARestartHitsIdempotencyPathNoDoubleCharge(t *testing.T) {
	l := testledger.Start(t, testListenAddr, testToken, testActor)
	setUpAccounts(l)

	order := l.CreateOrder(uniqueID(t, "c45-order-2"), "cust-c45-2")
	delegationID := uniqueID(t, "live-delegation-2")
	client := ledgerclient.New(l.BaseURL(), l.Token())
	cost, _ := money.ParseDecimal("2.438000")
	delegation := provider.Delegation{
		ID: delegationID, ProviderName: provider.Tronsell,
		TargetAddress: "TPayoutSlot00000000000000000002", EnergyUnits: 65000,
		CostTRX: cost, RequestedAt: time.Now().UTC(),
	}

	expenseBefore := l.AccountBalance("expense:energy")

	if err := client.ReportEnergyCost(context.Background(), delegation, order.ID); err != nil {
		t.Fatalf("ReportEnergyCost (1st): %v", err)
	}
	// A fresh client, same as a C4 process restarting between the first
	// call succeeding on C1's side and this service ever learning that --
	// the exact scenario this acceptance criterion names.
	restarted := ledgerclient.New(l.BaseURL(), l.Token())
	if err := restarted.ReportEnergyCost(context.Background(), delegation, order.ID); err != nil {
		t.Fatalf("ReportEnergyCost (2nd, replayed after a simulated restart): %v", err)
	}

	if got := l.JournalEntryCount("broker:energy_cost:" + delegationID); got != 1 {
		t.Fatalf("journal_entries rows after two identical reports = %d, want exactly 1 (no double-post)", got)
	}
	if got := l.AccountBalance("expense:energy") - expenseBefore; got != int64(cost) {
		t.Fatalf("expense:energy balance moved by %d, want exactly %d, not double-charged", got, int64(cost))
	}
}

// TestReportEnergyCost_DifferentPayloadSameKeyIsAP1Bug proves the
// structurally-impossible idempotency_conflict path is at least
// correctly surfaced, not silently swallowed, should it ever happen
// (e.g. a real bug generating a colliding delegation id).
func TestReportEnergyCost_DifferentPayloadSameKeyIsAP1Bug(t *testing.T) {
	l := testledger.Start(t, testListenAddr, testToken, testActor)
	setUpAccounts(l)

	order := l.CreateOrder(uniqueID(t, "c45-order-3"), "cust-c45-3")
	delegationID := uniqueID(t, "live-delegation-3")
	client := ledgerclient.New(l.BaseURL(), l.Token())
	cost1, _ := money.ParseDecimal("2.438000")
	cost2, _ := money.ParseDecimal("9.999999") // same id, different amount -> a real conflict

	delegation1 := provider.Delegation{ID: delegationID, ProviderName: provider.Tronsell, TargetAddress: "T1", EnergyUnits: 65000, CostTRX: cost1, RequestedAt: time.Now().UTC()}
	delegation2 := delegation1
	delegation2.CostTRX = cost2

	if err := client.ReportEnergyCost(context.Background(), delegation1, order.ID); err != nil {
		t.Fatalf("ReportEnergyCost (1st): %v", err)
	}
	err := client.ReportEnergyCost(context.Background(), delegation2, order.ID)
	if !errors.Is(err, ledgerclient.ErrIdempotencyConflictBug) {
		t.Fatalf("ReportEnergyCost (2nd, colliding id different payload) error = %v, want ErrIdempotencyConflictBug", err)
	}
}

// TestReportEnergyCost_NotHaltBlocked is this chunk's own third
// acceptance criterion, answered empirically against a real halted C1
// instance rather than assumed either way: a bare POST /v1/entries with
// entry_type=energy_cost is confirmed, by reading C1's own real
// implementation (journal.Post never consults halt.IsHalted; only
// orders.Transition does, for the 4 pairs C1.5's table marks
// HaltBlocked, none of which this call goes through), to succeed even
// while the ledger is halted.
func TestReportEnergyCost_NotHaltBlocked(t *testing.T) {
	l := testledger.Start(t, testListenAddr, testToken, testActor)
	setUpAccounts(l)

	order := l.CreateOrder(uniqueID(t, "c45-order-4"), "cust-c45-4")
	delegationID := uniqueID(t, "live-delegation-4")
	l.SetHalted(true, "C4.5_LIVE_TEST_HALT")
	t.Cleanup(func() { l.SetHalted(false, "") })

	client := ledgerclient.New(l.BaseURL(), l.Token())
	cost, _ := money.ParseDecimal("2.438000")
	delegation := provider.Delegation{
		ID: delegationID, ProviderName: provider.Tronsell,
		TargetAddress: "TPayoutSlot00000000000000000004", EnergyUnits: 65000,
		CostTRX: cost, RequestedAt: time.Now().UTC(),
	}

	if err := client.ReportEnergyCost(context.Background(), delegation, order.ID); err != nil {
		t.Fatalf("ReportEnergyCost against a HALTED C1: %v -- expected this to succeed (energy_cost entries are not halt-gated)", err)
	}
	if got := l.JournalEntryCount("broker:energy_cost:" + delegationID); got != 1 {
		t.Fatalf("journal_entries rows = %d, want exactly 1 -- the entry should have posted despite the halt", got)
	}
}
