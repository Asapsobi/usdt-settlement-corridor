package dispatch

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"dispatcher/internal/energy"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/txbuild"
)

// EnergyReserver is the narrow slice of energy.Client CutBatch needs --
// one reservation call, sized for the whole batch (see CutBatch's own
// doc comment on why this must never become one call per recipient).
// energy.Reservation is reused directly, not re-declared, so *energy.Client
// satisfies this interface with no adapter -- the same reason
// SigningService reuses signing.SigningRequest directly.
type EnergyReserver interface {
	Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error)
}

// ErrBatchWindowNotReady means the queue has neither reached MaxSize nor
// waited MaxWait yet -- not an error a caller should alert on, just
// "nothing to cut this tick."
var ErrBatchWindowNotReady = errors.New("dispatch: batch window not yet ready to cut")

// AccumulateForBatch adds a screened Sweep-tier order to the current
// open batch window. Unlike Direct/Standard orders, a Sweep order still
// enters `dispatching` (E2, via EnterDispatching) immediately -- §B's
// batching model only ever changes WHEN the on-chain payout leg happens,
// never whether or when the conversion entry itself lands (invariant 3
// applies identically across every tier).
func (d *Dispatcher) AccumulateForBatch(ctx context.Context, order ledgerclient.Order, slotID int, occurredAt time.Time) error {
	if _, err := d.EnterDispatching(ctx, order, slotID, occurredAt); err != nil {
		return fmt.Errorf("dispatch: entering dispatching before accumulating order %d for batch: %w", order.ID, err)
	}
	if _, err := d.Batches.Enqueue(ctx, order.ID, order.ExternalID, order.CustomerID, order.RecipientAddress, order.AmountOut); err != nil {
		return fmt.Errorf("dispatch: enqueuing order %d for batch: %w", order.ID, err)
	}
	return nil
}

// BatchWindow is CutBatch's own configuration: how long to accumulate
// orders before cutting a batch (whichever of MaxWait / MaxSize triggers
// first), and what to build and reserve against once it does. Real
// values (MultisendContractAddress, EnergyPerRecipientEstimate) are
// necessarily provisional -- see txbuild.BuildMultisend's and this
// file's own doc comments on why neither has a real, verified number to
// use yet.
type BatchWindow struct {
	MaxWait time.Duration
	MaxSize int

	SlotID                   int
	SlotAddress              string
	MultisendContractAddress string
	Ref                      txbuild.BlockReference

	// EnergyPerRecipientEstimate is a CONFIGURED estimate, not a verified
	// constant -- component-map's own "Read this fourth" says this
	// outright: the week-6 batched-multisend energy measurement (35,000
	// per recipient) has never actually been observed, and this chunk
	// does not get to assume it is correct. Sized for the batch total
	// (EnergyPerRecipientEstimate * recipient count), never reserved
	// per-recipient -- that would silently forfeit Sweep's own margin
	// advantage.
	EnergyPerRecipientEstimate int64
	EnergyDeadline             time.Time
}

// CutBatch builds one multisend transaction covering every order
// currently queued (up to window.MaxSize), reserves ONE energy
// allocation sized for the whole batch, and records the batch locally.
// Returns ErrBatchWindowNotReady if the queue has neither reached
// MaxSize nor waited MaxWait -- not itself an error worth alerting on.
func (d *Dispatcher) CutBatch(ctx context.Context, window BatchWindow, energy EnergyReserver) (Batch, error) {
	queued, err := d.Batches.ListQueued(ctx, window.MaxSize)
	if err != nil {
		return Batch{}, err
	}
	if len(queued) == 0 {
		return Batch{}, ErrBatchWindowNotReady
	}
	ready := len(queued) >= window.MaxSize || time.Since(queued[0].EnqueuedAt) >= window.MaxWait
	if !ready {
		return Batch{}, ErrBatchWindowNotReady
	}

	recipients := make([]txbuild.Recipient, len(queued))
	queuedIDs := make([]int64, len(queued))
	for i, q := range queued {
		recipients[i] = txbuild.Recipient{Address: q.RecipientAddress, Amount: q.AmountOut}
		queuedIDs[i] = q.ID
	}

	unsignedTx, err := txbuild.BuildMultisend(window.SlotAddress, window.MultisendContractAddress, recipients, window.Ref)
	if err != nil {
		return Batch{}, fmt.Errorf("dispatch: building multisend for a %d-recipient batch: %w", len(queued), err)
	}

	batch, err := d.Batches.CreateBatch(ctx, window.SlotID, queuedIDs, hex.EncodeToString(unsignedTx), hashHex(unsignedTx))
	if err != nil {
		return Batch{}, fmt.Errorf("dispatch: recording batch: %w", err)
	}

	totalUnits := window.EnergyPerRecipientEstimate * int64(len(queued))
	energyExternalID := fmt.Sprintf("batch-%d", batch.ID)
	energyIdemKey := fmt.Sprintf("dispatcher:reserve:batch:%d", batch.ID)
	if _, err := energy.Reserve(ctx, energyExternalID, window.SlotAddress, totalUnits, "SWEEP", window.EnergyDeadline, energyIdemKey); err != nil {
		return Batch{}, fmt.Errorf("dispatch: reserving energy for batch %d (%d units, %d recipients): %w", batch.ID, totalUnits, len(queued), err)
	}

	return batch, nil
}

// HandlePartialSettlement reports the outcome of one broadcast multisend
// batch, per recipient: succeeded[orderID] = true settles that order
// (its own E3 entry, its own amount -- never a shared one, per §B),
// false re-enqueues it into the NEXT batch window as a fresh attempt,
// never retried inside this same broadcast (invariant 1 applies
// per-recipient within a batch, not per-batch). Every order in the batch
// must have an entry in succeeded -- a missing one is a caller bug, not
// something to guess about.
//
// Deviates from the build spec's own sketch
// (HandlePartialSettlement(ctx, batchTxid string, perRecipientResults
// map[string]bool) error) in two ways, both for the same reason: batchID
// (this package's own real identifier, valid the moment CutBatch
// returns) is more robust than a txid string that does not exist until
// after a successful broadcast and cannot be looked up before then; and
// keying results by orderID, not an ambiguous recipient string, matches
// how every other part of this system already identifies an order.
func (d *Dispatcher) HandlePartialSettlement(ctx context.Context, batchID int64, occurredAt time.Time, succeeded map[int64]bool) (settled, requeued int, err error) {
	batch, err := d.Batches.Get(ctx, batchID)
	if err != nil {
		return 0, 0, err
	}
	queuedOrders, err := d.Batches.ListForBatch(ctx, batchID)
	if err != nil {
		return 0, 0, err
	}

	for _, q := range queuedOrders {
		ok, given := succeeded[q.OrderID]
		if !given {
			return settled, requeued, fmt.Errorf("dispatch: HandlePartialSettlement: no result given for order %d in batch %d", q.OrderID, batchID)
		}

		if !ok {
			if err := d.Batches.Requeue(ctx, q.ID); err != nil {
				return settled, requeued, fmt.Errorf("dispatch: requeuing order %d after batch %d partial failure: %w", q.OrderID, batchID, err)
			}
			requeued++
			continue
		}

		order, err := d.Ledger.GetOrder(ctx, q.ExternalID)
		if err != nil {
			return settled, requeued, fmt.Errorf("dispatch: fetching order %s to settle from batch %d: %w", q.ExternalID, batchID, err)
		}
		settleKey := fmt.Sprintf("dispatcher:settle:%d:batch%d", q.OrderID, batchID)
		if err := d.settleOrder(ctx, order, batch.SlotID, settleKey, occurredAt); err != nil {
			return settled, requeued, fmt.Errorf("dispatch: settling order %d from batch %d: %w", q.OrderID, batchID, err)
		}
		if err := d.Store.MarkSettled(ctx, q.OrderID); err != nil {
			return settled, requeued, fmt.Errorf("dispatch: marking order %d SETTLED locally: %w", q.OrderID, err)
		}
		settled++
	}
	return settled, requeued, nil
}
