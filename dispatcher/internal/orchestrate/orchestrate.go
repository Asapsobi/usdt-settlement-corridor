// Package orchestrate is the background loop cmd/dispatchd's own doc
// comment names as deliberately not started: carrying a screened
// Direct/Standard order the rest of the way from EnterDispatching
// through energy reservation, signing, broadcast, and finality
// confirmation -- and re-polling finality for whatever is already in
// flight -- with no human running a CLI command in between. Everything
// this package calls (EnterDispatching, Broadcast, ConfirmFinality,
// energy.Client.Reserve, the real TRON clients in tronchain.go) already
// exists and is already tested; this package is wiring, in the same
// sense cmd/brokerd's own production loop was wiring on top of an
// already-built internal/buffer.
//
// Sweep-tier orders are left untouched here. CutBatch/BroadcastBatch
// need a real multisend contract this project doesn't have on either
// chain yet (component-map.md's own C5 entry) -- driving that loop is a
// separate, not-yet-scoped task, not something this proof run needs
// (Direct/Standard alone is enough to prove one settlement end to end).
package orchestrate

import (
	"context"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/slots"
	"dispatcher/internal/txbuild"
)

// BlockReferenceSource resolves a fresh txbuild.BlockReference from a
// real TRON node before building a transfer -- dispatch.GrpcBroadcastClient's
// own CurrentBlockReference in production, a fixed/fake one in tests.
type BlockReferenceSource interface {
	CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error)
}

// Config is this loop's own required tuning -- no hardcoded default,
// same posture as every other real-money threshold in this project
// (e.g. DISPATCHER_SLOT_BALANCE_CEILING).
type Config struct {
	// EnergyPerTransferUnits is how much TRON energy to reserve from C4
	// before broadcasting a single (non-batched) TRC20 transfer. 65000 is
	// what this codebase's own replay scenarios already use for a
	// STANDARD-tier reservation of this shape (internal/replay/scenarios.go).
	EnergyPerTransferUnits int64

	// EnergyReservationWait bounds how long C4's own Reserve call (its
	// own fast/slow path -- see internal/energy.Client.Reserve, which is
	// synchronous) is given before its deadline.
	EnergyReservationWait time.Duration
}

// Orchestrator bundles everything RunLoop needs: the real Dispatcher, a
// real slot store, a real energy reservation client, and real chain
// clients for block resolution, broadcast, and finality.
type Orchestrator struct {
	Ledger     *ledgerclient.Client
	Slots      *slots.Store
	SlotCaps   slots.Caps
	Dispatcher *dispatch.Dispatcher
	Energy     dispatch.EnergyReserver
	Chain      dispatch.BroadcastClient
	Blocks     BlockReferenceSource
	Finality   dispatch.FinalityReader
	Cfg        Config

	// pending caches, per order, the exact unsigned tx bytes and attempt
	// number a not-yet-terminal Broadcast call used -- see dispatchOne's
	// own doc comment for why a retry must reuse them rather than
	// rebuild. Only ever touched from RunTick's own single-goroutine
	// call path (RunLoop never runs two ticks concurrently), so no mutex.
	pending map[int64]pendingBroadcast
}

type pendingBroadcast struct {
	attemptNumber int
	unsignedTx    []byte
}

// New wires an Orchestrator.
func New(ledger *ledgerclient.Client, slotsStore *slots.Store, slotCaps slots.Caps, dispatcher *dispatch.Dispatcher, energy dispatch.EnergyReserver, chain dispatch.BroadcastClient, blocks BlockReferenceSource, finality dispatch.FinalityReader, cfg Config) *Orchestrator {
	return &Orchestrator{
		Ledger: ledger, Slots: slotsStore, SlotCaps: slotCaps, Dispatcher: dispatcher,
		Energy: energy, Chain: chain, Blocks: blocks, Finality: finality, Cfg: cfg,
		pending: make(map[int64]pendingBroadcast),
	}
}
