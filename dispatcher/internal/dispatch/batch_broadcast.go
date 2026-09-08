package dispatch

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"dispatcher/internal/signing"
	"dispatcher/internal/txbuild"
)

// BroadcastBatch advances batchID from however far it has already gotten
// toward BROADCAST -- the Sweep-tier counterpart to Broadcast, for a
// batch cut by CutBatch rather than a single order's own attempt.
// unsignedTx is the SAME BuildMultisend output CutBatch itself built for
// this batch (also stored on the row, at batches.unsigned_tx, so a
// caller resuming after a crash can pass back exactly what was
// persisted rather than needing to reconstruct it independently).
//
//   - A batch already BROADCAST, CONFIRMED, or FAILED: returned as-is.
//     Never re-signed, never re-broadcast.
//   - A batch already SIGNED (a crash landed between the signature
//     coming back and the broadcast call): the STORED signature
//     (batches.signed_tx, the actual bytes) is reused directly --
//     RequestSignature is never called again for this batch, the same
//     "never re-sign" requirement Broadcast's own doc comment explains
//     for the single-order case, and for the same reason: a second
//     signature over the same payload is a second valid TRON
//     transaction if the first broadcast actually landed despite this
//     process never observing the response.
//   - A batch still CUT: request a signature (idempotent on
//     "dispatcher:sign:batch:<id>", invariant 6's own convention),
//     store the actual bytes, mark SIGNED.
//
// Unlike Broadcast, this does NOT hold a Postgres row lock across the
// broadcast step -- C5.8's own acceptance criteria never required the
// same concurrency-proof rigor C5.5 required for single-order Broadcast
// (no "N concurrent BroadcastBatch calls produce exactly one chain call"
// test exists for batches). Two callers racing to broadcast the SAME
// batch could both reach chain.Broadcast before either's own SIGNED ->
// BROADCAST update lands; only one update wins, but the chain call
// itself is not deduplicated the way Broadcast's own FOR UPDATE lock
// guarantees for single orders. Batches are cut on a slow, operator-scale
// cadence (a batch window, not a per-request race), so this gap is
// judged acceptable for now rather than worth the same machinery
// single-order dispatch needed -- revisit if BroadcastBatch is ever
// called from more than one place concurrently for the same batch.
func (d *Dispatcher) BroadcastBatch(ctx context.Context, batchID int64, unsignedTx []byte, estimatedUSD float64, chain BroadcastClient) (Batch, error) {
	batch, err := d.Batches.Get(ctx, batchID)
	if err != nil {
		return Batch{}, err
	}
	if batch.Status.terminal() {
		return batch, nil
	}

	signed, err := d.ensureBatchSigned(ctx, batch, unsignedTx, estimatedUSD)
	if err != nil {
		return Batch{}, err
	}
	if signed.Status.terminal() {
		return signed, nil
	}

	return d.ensureBatchBroadcast(ctx, signed, unsignedTx, chain)
}

func (d *Dispatcher) ensureBatchSigned(ctx context.Context, batch Batch, unsignedTx []byte, estimatedUSD float64) (Batch, error) {
	if batch.Status.terminal() || batch.Status == BatchSigned {
		return batch, nil
	}

	digest := txbuild.Digest(unsignedTx)
	signIdemKey := fmt.Sprintf("dispatcher:sign:batch:%d", batch.ID)
	sigReq, err := d.Signer.RequestSignature(ctx, batch.SlotID, digest, estimatedUSD, signIdemKey)
	if err != nil {
		return Batch{}, fmt.Errorf("dispatch: requesting signature for batch %d: %w", batch.ID, err)
	}
	switch sigReq.Status {
	case signing.StatusPending:
		return Batch{}, ErrSignaturePending
	case signing.StatusRejected:
		if _, markErr := d.Batches.markBatchFailed(ctx, batch.ID); markErr != nil {
			return Batch{}, fmt.Errorf("dispatch: marking batch %d FAILED after signature rejection: %w", batch.ID, markErr)
		}
		return Batch{}, ErrSignatureRejected
	}

	signatureHex := hex.EncodeToString(sigReq.SignedTx[:])
	return d.Batches.markBatchSigned(ctx, batch.ID, signatureHex, hashHex(sigReq.SignedTx[:]))
}

func (d *Dispatcher) ensureBatchBroadcast(ctx context.Context, batch Batch, unsignedTx []byte, chain BroadcastClient) (Batch, error) {
	if batch.Status.terminal() {
		return batch, nil
	}
	if batch.Status != BatchSigned || batch.SignedTx == nil {
		return Batch{}, fmt.Errorf("dispatch: batch %d is not SIGNED with a stored signature (status %s)", batch.ID, batch.Status)
	}

	signature, err := hex.DecodeString(*batch.SignedTx)
	if err != nil || len(signature) != 65 {
		return Batch{}, fmt.Errorf("dispatch: stored signature for batch %d is not 65 bytes of hex: %v", batch.ID, err)
	}
	var sigArray [65]byte
	copy(sigArray[:], signature)

	fullTx, err := reconstructTransaction(unsignedTx, sigArray)
	if err != nil {
		return Batch{}, fmt.Errorf("dispatch: reconstructing signed transaction for batch %d: %w", batch.ID, err)
	}
	tronTxID, err := chain.Broadcast(ctx, fullTx)
	if err != nil {
		return Batch{}, fmt.Errorf("dispatch: broadcasting batch %d: %w", batch.ID, err)
	}

	return d.Batches.markBatchBroadcast(ctx, batch.ID, tronTxID, time.Now().UTC())
}
