package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"dispatcher/internal/db"
)

// BroadcastStatus is one dispatch_attempts row's own lifecycle:
// BUILT -> SIGNED -> BROADCAST -> CONFIRMED (C5.6), or -> FAILED at any
// point before BROADCAST (this chunk never sets FAILED itself -- the
// status exists in the schema for a later chunk's use, e.g. a freeze
// detected before broadcast).
type BroadcastStatus string

const (
	BroadcastBuilt     BroadcastStatus = "BUILT"
	BroadcastSigned    BroadcastStatus = "SIGNED"
	BroadcastBroadcast BroadcastStatus = "BROADCAST"
	BroadcastConfirmed BroadcastStatus = "CONFIRMED"
	BroadcastFailed    BroadcastStatus = "FAILED"
)

// terminal reports whether s is a status Broadcast must never act past --
// no re-signing, no re-broadcasting, just return the row as it stands.
func (s BroadcastStatus) terminal() bool {
	return s == BroadcastBroadcast || s == BroadcastConfirmed || s == BroadcastFailed
}

// BroadcastAttempt is one dispatch_attempts row: a single constructed,
// content-addressed transfer and how far it has gotten toward the chain.
// Distinct from Attempt (dispatch_state, C5.3): that is one row per
// ORDER; this is one row per (order, attempt_number), because an order
// can accumulate more than one genuinely different transfer across
// retries, never more than one row for the SAME transfer (unsigned_tx_hash
// is unique).
type BroadcastAttempt struct {
	ID             int64
	OrderID        int64
	SlotID         int
	AttemptNumber  int
	UnsignedTxHash string
	SignedTx       *string
	SignedTxHash   *string
	BroadcastAt    *time.Time
	TronTxID       *string
	Status         BroadcastStatus
	CreatedAt      time.Time
}

// ErrBroadcastAttemptNotFound means no dispatch_attempts row exists for
// the given id.
var ErrBroadcastAttemptNotFound = errors.New("dispatch: no such broadcast attempt")

// AttemptStore is dispatch_attempts' own entry point.
type AttemptStore struct {
	pool *db.Pool
}

// NewAttemptStore wires an AttemptStore.
func NewAttemptStore(pool *db.Pool) *AttemptStore {
	return &AttemptStore{pool: pool}
}

// withTx runs fn inside a transaction on this store's own pool -- Broadcast's
// own way to hold the row lock getForUpdate takes for exactly as long as
// its own sign+broadcast sequence needs, no longer.
func (s *AttemptStore) withTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return db.Tx(ctx, s.pool, fn)
}

// Create inserts a BUILT row for (orderID, slotID, attemptNumber,
// unsignedTxHash), idempotent on unsignedTxHash: a second Create for the
// identical content-addressed transfer -- whether a genuine concurrent
// race or a retry after a crash -- returns the SAME row unchanged, never
// a duplicate. This is the mechanism invariant 1's exactly-once proof
// rests on: the row, not application-level locking, is what makes two
// concurrent callers agree on one outcome.
func (s *AttemptStore) Create(ctx context.Context, orderID int64, slotID, attemptNumber int, unsignedTxHash string) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dispatch_attempts (order_id, slot_id, attempt_number, unsigned_tx_hash, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (unsigned_tx_hash) DO NOTHING
		RETURNING id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
	`, orderID, slotID, attemptNumber, unsignedTxHash, string(BroadcastBuilt))

	attempt, err := scanBroadcastAttempt(row)
	if err == nil {
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: creating broadcast attempt for order %d: %w", orderID, err)
	}

	// ON CONFLICT DO NOTHING returned no row: this exact transfer was
	// already recorded, by this call or a concurrent one.
	return s.getByUnsignedTxHash(ctx, unsignedTxHash)
}

func (s *AttemptStore) getByUnsignedTxHash(ctx context.Context, unsignedTxHash string) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
		FROM dispatch_attempts WHERE unsigned_tx_hash = $1
	`, unsignedTxHash)
	attempt, err := scanBroadcastAttempt(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BroadcastAttempt{}, ErrBroadcastAttemptNotFound
		}
		return BroadcastAttempt{}, fmt.Errorf("dispatch: fetching broadcast attempt %q: %w", unsignedTxHash, err)
	}
	return attempt, nil
}

// Get fetches one dispatch_attempts row by its own id.
func (s *AttemptStore) Get(ctx context.Context, id int64) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
		FROM dispatch_attempts WHERE id = $1
	`, id)
	attempt, err := scanBroadcastAttempt(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BroadcastAttempt{}, ErrBroadcastAttemptNotFound
		}
		return BroadcastAttempt{}, fmt.Errorf("dispatch: fetching broadcast attempt %d: %w", id, err)
	}
	return attempt, nil
}

// LatestForOrder fetches orderID's own most recent attempt (highest
// attempt_number) -- C5.7's own reconciliation job uses this to decide
// whether an order still in DISPATCHING has given up (its latest attempt
// is FAILED) or is still legitimately in flight.
func (s *AttemptStore) LatestForOrder(ctx context.Context, orderID int64) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
		FROM dispatch_attempts WHERE order_id = $1 ORDER BY attempt_number DESC LIMIT 1
	`, orderID)
	attempt, err := scanBroadcastAttempt(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BroadcastAttempt{}, ErrBroadcastAttemptNotFound
		}
		return BroadcastAttempt{}, fmt.Errorf("dispatch: fetching latest broadcast attempt for order %d: %w", orderID, err)
	}
	return attempt, nil
}

// MarkFailed transitions id to FAILED from BUILT or SIGNED (never from
// BROADCAST onward -- once a transaction is on the chain, whether it
// landed is a fact to discover, not something this side declares by
// fiat). Called when a specific attempt is known to be permanently
// unusable (S1 rejected the signing request, for instance) -- distinct
// from the ORDER giving up entirely, which is C5.7's reconciliation job
// and DispatchFailureReporter's own concern, one level up.
func (s *AttemptStore) MarkFailed(ctx context.Context, id int64) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE dispatch_attempts SET status = $1
		WHERE id = $2 AND status IN ($3, $4)
		RETURNING id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
	`, string(BroadcastFailed), id, string(BroadcastBuilt), string(BroadcastSigned))
	attempt, err := scanBroadcastAttempt(row)
	if err == nil {
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: marking broadcast attempt %d FAILED: %w", id, err)
	}
	return s.Get(ctx, id)
}

// getForUpdate re-reads id inside tx with a row lock, blocking a
// concurrent caller already holding it until that caller's own
// transaction commits or rolls back -- what makes "exactly one of two
// concurrent Broadcast calls actually signs and broadcasts" true: the
// loser blocks here, then observes the winner's finished (BROADCAST)
// status once unblocked, rather than racing it.
func (s *AttemptStore) getForUpdate(ctx context.Context, tx pgx.Tx, id int64) (BroadcastAttempt, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
		FROM dispatch_attempts WHERE id = $1 FOR UPDATE
	`, id)
	attempt, err := scanBroadcastAttempt(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BroadcastAttempt{}, ErrBroadcastAttemptNotFound
		}
		return BroadcastAttempt{}, fmt.Errorf("dispatch: fetching broadcast attempt %d for update: %w", id, err)
	}
	return attempt, nil
}

// markSigned stores both the actual signature (signedTxHex, hex-encoded)
// -- what a resumed Broadcast reconstructs the full transaction from,
// without ever calling RequestSignature again -- and its own hash, kept
// purely for a quick audit/display field. Runs standalone (this store's
// own pool, no explicit transaction, no row lock): guarded by
// "WHERE status = BUILT" instead, so two callers racing to sign the same
// attempt both succeed at RequestSignature (itself idempotent) but only
// one of them actually performs this UPDATE -- the other's WHERE clause
// matches zero rows, and it falls back to reading whatever the winner
// wrote, which is byte-identical to what it would have written itself.
func (s *AttemptStore) markSigned(ctx context.Context, id int64, signedTxHex, signedTxHash string) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE dispatch_attempts SET status = $1, signed_tx = $2, signed_tx_hash = $3
		WHERE id = $4 AND status = $5
		RETURNING id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
	`, string(BroadcastSigned), signedTxHex, signedTxHash, id, string(BroadcastBuilt))
	attempt, err := scanBroadcastAttempt(row)
	if err == nil {
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: marking broadcast attempt %d SIGNED: %w", id, err)
	}
	// WHERE status = BUILT matched nothing: someone else already signed
	// (or moved this row further) between this call's own read and this
	// UPDATE. Read back whatever is actually there now.
	return s.Get(ctx, id)
}

func (s *AttemptStore) markBroadcast(ctx context.Context, tx pgx.Tx, id int64, tronTxID string, broadcastAt time.Time) (BroadcastAttempt, error) {
	row := tx.QueryRow(ctx, `
		UPDATE dispatch_attempts SET status = $1, tron_txid = $2, broadcast_at = $3
		WHERE id = $4
		RETURNING id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
	`, string(BroadcastBroadcast), tronTxID, broadcastAt, id)
	attempt, err := scanBroadcastAttempt(row)
	if err != nil {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: marking broadcast attempt %d BROADCAST: %w", id, err)
	}
	return attempt, nil
}

// markConfirmed transitions id from BROADCAST to CONFIRMED -- C5.6's own
// concern, guarded by "WHERE status = BROADCAST" the same way markSigned
// guards on BUILT.
func (s *AttemptStore) markConfirmed(ctx context.Context, id int64) (BroadcastAttempt, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE dispatch_attempts SET status = $1
		WHERE id = $2 AND status = $3
		RETURNING id, order_id, slot_id, attempt_number, unsigned_tx_hash, signed_tx, signed_tx_hash, broadcast_at, tron_txid, status, created_at
	`, string(BroadcastConfirmed), id, string(BroadcastBroadcast))
	attempt, err := scanBroadcastAttempt(row)
	if err == nil {
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return BroadcastAttempt{}, fmt.Errorf("dispatch: marking broadcast attempt %d CONFIRMED: %w", id, err)
	}
	return s.Get(ctx, id)
}

type scanRowAttempt interface {
	Scan(dest ...any) error
}

func scanBroadcastAttempt(row scanRowAttempt) (BroadcastAttempt, error) {
	var a BroadcastAttempt
	var status string
	err := row.Scan(&a.ID, &a.OrderID, &a.SlotID, &a.AttemptNumber, &a.UnsignedTxHash,
		&a.SignedTx, &a.SignedTxHash, &a.BroadcastAt, &a.TronTxID, &status, &a.CreatedAt)
	if err != nil {
		return BroadcastAttempt{}, err
	}
	a.Status = BroadcastStatus(status)
	return a, nil
}
