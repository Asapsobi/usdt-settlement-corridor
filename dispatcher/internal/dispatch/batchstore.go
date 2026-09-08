package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"dispatcher/internal/db"
	"dispatcher/internal/money"
)

// QueueStatus is one batch_queue row's own lifecycle.
type QueueStatus string

const (
	QueueQueued  QueueStatus = "QUEUED"
	QueueBatched QueueStatus = "BATCHED"
)

// QueuedOrder is one Sweep-tier order waiting for its payout to be
// accumulated into a batch. Its own E2 conversion entry has already
// landed by the time it is enqueued (AccumulateForBatch's own job) --
// this table only ever concerns the on-chain payout leg.
type QueuedOrder struct {
	ID               int64
	OrderID          int64
	ExternalID       string
	CustomerID       string
	RecipientAddress string
	AmountOut        money.Amount
	Status           QueueStatus
	BatchID          *int64
	EnqueuedAt       time.Time
}

// BatchStatus is one batches row's own lifecycle -- CUT (built, energy
// reserved) -> SIGNED -> BROADCAST -> CONFIRMED, or FAILED.
type BatchStatus string

const (
	BatchCut       BatchStatus = "CUT"
	BatchSigned    BatchStatus = "SIGNED"
	BatchBroadcast BatchStatus = "BROADCAST"
	BatchConfirmed BatchStatus = "CONFIRMED"
	BatchFailed    BatchStatus = "FAILED"
)

// Batch is one cut multisend transaction. UnsignedTx and SignedTx are
// both hex-encoded actual bytes (not just hashes) -- see this table's
// own migration doc comment for why: a hash alone cannot be turned back
// into what BroadcastBatch needs to sign or broadcast on resume.
type Batch struct {
	ID             int64
	SlotID         int
	RecipientCount int
	UnsignedTx     *string
	SignedTx       *string
	TronTxID       *string
	Status         BatchStatus
	CutAt          time.Time
	BroadcastAt    *time.Time
}

// terminal reports whether s is a status BroadcastBatch must never act
// past -- no re-signing, no re-broadcasting, just return the row as it
// stands. Mirrors BroadcastStatus.terminal's own reasoning.
func (s BatchStatus) terminal() bool {
	return s == BatchBroadcast || s == BatchConfirmed || s == BatchFailed
}

var ErrQueuedOrderNotFound = errors.New("dispatch: no such queued order")
var ErrBatchNotFound = errors.New("dispatch: no such batch")

// BatchStore is batch_queue's and batches' own entry point.
type BatchStore struct {
	pool *db.Pool
}

// NewBatchStore wires a BatchStore.
func NewBatchStore(pool *db.Pool) *BatchStore {
	return &BatchStore{pool: pool}
}

// Enqueue adds orderID to the queue, idempotent on order_id -- a second
// Enqueue for the same order (a retried AccumulateForBatch call) returns
// the existing row unchanged rather than erroring or duplicating it.
func (s *BatchStore) Enqueue(ctx context.Context, orderID int64, externalID, customerID, recipientAddress string, amountOut money.Amount) (QueuedOrder, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO batch_queue (order_id, external_id, customer_id, recipient_address, amount_out, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (order_id) DO NOTHING
		RETURNING id, order_id, external_id, customer_id, recipient_address, amount_out, status, batch_id, enqueued_at
	`, orderID, externalID, customerID, recipientAddress, int64(amountOut), string(QueueQueued))
	q, err := scanQueuedOrder(row)
	if err == nil {
		return q, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return QueuedOrder{}, fmt.Errorf("dispatch: enqueuing order %d for batching: %w", orderID, err)
	}
	return s.getByOrderID(ctx, orderID)
}

func (s *BatchStore) getByOrderID(ctx context.Context, orderID int64) (QueuedOrder, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, order_id, external_id, customer_id, recipient_address, amount_out, status, batch_id, enqueued_at
		FROM batch_queue WHERE order_id = $1
	`, orderID)
	q, err := scanQueuedOrder(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QueuedOrder{}, ErrQueuedOrderNotFound
		}
		return QueuedOrder{}, fmt.Errorf("dispatch: fetching queued order %d: %w", orderID, err)
	}
	return q, nil
}

// ListQueued returns up to limit QUEUED orders, oldest first -- CutBatch's
// own candidate pool.
func (s *BatchStore) ListQueued(ctx context.Context, limit int) ([]QueuedOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, order_id, external_id, customer_id, recipient_address, amount_out, status, batch_id, enqueued_at
		FROM batch_queue WHERE status = $1 ORDER BY enqueued_at LIMIT $2
	`, string(QueueQueued), limit)
	if err != nil {
		return nil, fmt.Errorf("dispatch: listing queued orders: %w", err)
	}
	defer rows.Close()

	var out []QueuedOrder
	for rows.Next() {
		q, err := scanQueuedOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// CreateBatch atomically creates a batches row and marks every named
// queued-order id BATCHED, pointing at it -- one transaction, so a
// crash between the two never leaves a batch with an inconsistent queue
// (either both happened, or neither did). unsignedTxHex is the actual
// BuildMultisend output (hex-encoded), stored alongside its own hash --
// see this table's own migration doc comment for why both are kept.
func (s *BatchStore) CreateBatch(ctx context.Context, slotID int, queuedOrderIDs []int64, unsignedTxHex, unsignedTxHash string) (Batch, error) {
	var batch Batch
	err := db.Tx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO batches (slot_id, recipient_count, unsigned_tx, unsigned_tx_hash, status)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, slot_id, recipient_count, unsigned_tx, signed_tx, tron_txid, status, cut_at, broadcast_at
		`, slotID, len(queuedOrderIDs), unsignedTxHex, unsignedTxHash, string(BatchCut))
		var err error
		batch, err = scanBatch(row)
		if err != nil {
			return fmt.Errorf("creating batch: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE batch_queue SET status = $1, batch_id = $2
			WHERE id = ANY($3) AND status = $4
		`, string(QueueBatched), batch.ID, queuedOrderIDs, string(QueueQueued))
		if err != nil {
			return fmt.Errorf("marking queued orders BATCHED: %w", err)
		}
		if int(tag.RowsAffected()) != len(queuedOrderIDs) {
			return fmt.Errorf("expected to batch %d queued orders, actually batched %d (a queued order was claimed elsewhere)", len(queuedOrderIDs), tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return Batch{}, err
	}
	return batch, nil
}

// Get fetches one batches row by its own id.
func (s *BatchStore) Get(ctx context.Context, id int64) (Batch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, slot_id, recipient_count, unsigned_tx, signed_tx, tron_txid, status, cut_at, broadcast_at FROM batches WHERE id = $1
	`, id)
	b, err := scanBatch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Batch{}, ErrBatchNotFound
		}
		return Batch{}, fmt.Errorf("dispatch: fetching batch %d: %w", id, err)
	}
	return b, nil
}

// ListForBatch returns every queued-order row pointing at batchID.
func (s *BatchStore) ListForBatch(ctx context.Context, batchID int64) ([]QueuedOrder, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, order_id, external_id, customer_id, recipient_address, amount_out, status, batch_id, enqueued_at
		FROM batch_queue WHERE batch_id = $1 ORDER BY id
	`, batchID)
	if err != nil {
		return nil, fmt.Errorf("dispatch: listing orders for batch %d: %w", batchID, err)
	}
	defer rows.Close()

	var out []QueuedOrder
	for rows.Next() {
		q, err := scanQueuedOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// Requeue returns a BATCHED queued-order row to QUEUED, detached from
// its old batch -- HandlePartialSettlement's own path for a recipient
// whose payout failed within an otherwise-successful batch: re-enqueued
// for the NEXT batch window as a fresh attempt, never retried inside the
// same broadcast (invariant 1 is per-recipient here, not per-batch).
func (s *BatchStore) Requeue(ctx context.Context, queuedOrderID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE batch_queue SET status = $1, batch_id = NULL, enqueued_at = now()
		WHERE id = $2
	`, string(QueueQueued), queuedOrderID)
	if err != nil {
		return fmt.Errorf("dispatch: requeuing batch_queue row %d: %w", queuedOrderID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrQueuedOrderNotFound
	}
	return nil
}

// markBatchSigned stores both the actual signature (signedTxHex) and its
// own hash, guarded by "WHERE status = CUT" -- the same lock-free
// concurrent-signing guard AttemptStore.markSigned uses (see its own doc
// comment): two callers racing to sign the same batch both succeed at
// RequestSignature (itself idempotent), but only one's UPDATE actually
// lands; the other reads back whatever the winner wrote.
func (s *BatchStore) markBatchSigned(ctx context.Context, batchID int64, signedTxHex, signedTxHash string) (Batch, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE batches SET status = $1, signed_tx = $2, signed_tx_hash = $3
		WHERE id = $4 AND status = $5
		RETURNING id, slot_id, recipient_count, unsigned_tx, signed_tx, tron_txid, status, cut_at, broadcast_at
	`, string(BatchSigned), signedTxHex, signedTxHash, batchID, string(BatchCut))
	batch, err := scanBatch(row)
	if err == nil {
		return batch, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, fmt.Errorf("dispatch: marking batch %d SIGNED: %w", batchID, err)
	}
	return s.Get(ctx, batchID)
}

// markBatchFailed transitions batchID to FAILED from CUT or SIGNED --
// never from BROADCAST onward, mirroring AttemptStore.MarkFailed's own
// reasoning: once broadcast, whether it landed is a fact to discover,
// never something this side declares by fiat.
func (s *BatchStore) markBatchFailed(ctx context.Context, batchID int64) (Batch, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE batches SET status = $1
		WHERE id = $2 AND status IN ($3, $4)
		RETURNING id, slot_id, recipient_count, unsigned_tx, signed_tx, tron_txid, status, cut_at, broadcast_at
	`, string(BatchFailed), batchID, string(BatchCut), string(BatchSigned))
	batch, err := scanBatch(row)
	if err == nil {
		return batch, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, fmt.Errorf("dispatch: marking batch %d FAILED: %w", batchID, err)
	}
	return s.Get(ctx, batchID)
}

// markBatchBroadcast transitions batchID from SIGNED to BROADCAST,
// guarded the same way markBatchSigned is.
func (s *BatchStore) markBatchBroadcast(ctx context.Context, batchID int64, tronTxID string, broadcastAt time.Time) (Batch, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE batches SET status = $1, tron_txid = $2, broadcast_at = $3
		WHERE id = $4 AND status = $5
		RETURNING id, slot_id, recipient_count, unsigned_tx, signed_tx, tron_txid, status, cut_at, broadcast_at
	`, string(BatchBroadcast), tronTxID, broadcastAt, batchID, string(BatchSigned))
	batch, err := scanBatch(row)
	if err == nil {
		return batch, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, fmt.Errorf("dispatch: marking batch %d BROADCAST: %w", batchID, err)
	}
	return s.Get(ctx, batchID)
}

func scanQueuedOrder(row interface{ Scan(dest ...any) error }) (QueuedOrder, error) {
	var q QueuedOrder
	var status string
	var amountUnits int64
	err := row.Scan(&q.ID, &q.OrderID, &q.ExternalID, &q.CustomerID, &q.RecipientAddress, &amountUnits, &status, &q.BatchID, &q.EnqueuedAt)
	if err != nil {
		return QueuedOrder{}, err
	}
	q.Status = QueueStatus(status)
	q.AmountOut = money.Amount(amountUnits)
	return q, nil
}

func scanBatch(row interface{ Scan(dest ...any) error }) (Batch, error) {
	var b Batch
	var status string
	err := row.Scan(&b.ID, &b.SlotID, &b.RecipientCount, &b.UnsignedTx, &b.SignedTx, &b.TronTxID, &status, &b.CutAt, &b.BroadcastAt)
	if err != nil {
		return Batch{}, err
	}
	b.Status = BatchStatus(status)
	return b, nil
}
