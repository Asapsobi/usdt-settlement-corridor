// Package discovery is C3.3: finding out which orders C1 has just moved
// to funded (there is no event bus in this system by design -- see
// docs/03-build/c3-screening-build-prompts.md's "Read this first") and
// remembering them, with their deposit's sender address, in C3's own
// screening_queue -- the thing C3.4 actually pulls from to call a
// screening provider.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"screening/internal/db"
)

// Queryer is db.Queryer under this package's own name, matching every
// other internal package's convention in this module.
type Queryer = db.Queryer

// Status is screening_queue's closed set of states -- a plain string,
// not a Go-level enum, matching the column's own text type (enforced at
// the database level by a CHECK constraint instead, see migration 0003).
type Status string

const (
	Pending   Status = "PENDING"
	Screening Status = "SCREENING"
	Done      Status = "DONE"
)

// QueueEntry is one screening_queue row.
type QueueEntry struct {
	OrderID       int64
	ExternalID    string
	SenderAddress *string
	Status        Status
}

// ErrNotFound means no screening_queue row exists for the given order id.
var ErrNotFound = errors.New("discovery: no such queued order")

// enqueueIfNew inserts a new PENDING row for (orderID, externalID), with
// sender_address if it's already known (nil otherwise). A row already
// present for orderID is left completely untouched -- re-discovering an
// already-queued order (a re-poll after restart, or the same order
// appearing on two overlapping pages) is a no-op, never a second row and
// never a silent overwrite of whatever this queue has already recorded
// for it since (e.g. status having moved on to SCREENING or DONE).
func enqueueIfNew(ctx context.Context, q Queryer, orderID int64, externalID string, senderAddress *string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO screening_queue (order_id, external_id, sender_address)
		VALUES ($1, $2, $3)
		ON CONFLICT (order_id) DO NOTHING
	`, orderID, externalID, senderAddress)
	if err != nil {
		return fmt.Errorf("discovery: enqueueing order %d (%s): %w", orderID, externalID, err)
	}
	return nil
}

// setSenderAddress records a resolved sender address for orderID -- the
// retry pass's own write, once GetSenderAddress finally succeeds for a
// row that was enqueued without one.
func setSenderAddress(ctx context.Context, q Queryer, orderID int64, address string) error {
	_, err := q.Exec(ctx, `
		UPDATE screening_queue SET sender_address = $1, updated_at = now() WHERE order_id = $2
	`, address, orderID)
	if err != nil {
		return fmt.Errorf("discovery: recording sender_address for order %d: %w", orderID, err)
	}
	return nil
}

// listMissingSenderAddress returns every queued order still waiting on a
// resolved sender address -- the retry pass's own read.
func listMissingSenderAddress(ctx context.Context, q Queryer) ([]QueueEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT order_id, external_id, sender_address, status
		FROM screening_queue
		WHERE sender_address IS NULL
		ORDER BY order_id
	`)
	if err != nil {
		return nil, fmt.Errorf("discovery: listing orders missing sender_address: %w", err)
	}
	defer rows.Close()

	var out []QueueEntry
	for rows.Next() {
		var e QueueEntry
		var status string
		if err := rows.Scan(&e.OrderID, &e.ExternalID, &e.SenderAddress, &status); err != nil {
			return nil, fmt.Errorf("discovery: listing orders missing sender_address: %w", err)
		}
		e.Status = Status(status)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("discovery: listing orders missing sender_address: %w", err)
	}
	return out, nil
}

// ListReadyForScreening returns every PENDING row that already has a
// resolved sender_address -- C3.4's own pipeline pulls from exactly this
// list. A row still missing its address (see listMissingSenderAddress)
// or already past PENDING (SCREENING or DONE) is never returned, so a
// row already claimed by a still-in-flight tick isn't picked up again.
func ListReadyForScreening(ctx context.Context, q Queryer) ([]QueueEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT order_id, external_id, sender_address, status
		FROM screening_queue
		WHERE status = $1 AND sender_address IS NOT NULL
		ORDER BY order_id
	`, string(Pending))
	if err != nil {
		return nil, fmt.Errorf("discovery: listing orders ready for screening: %w", err)
	}
	defer rows.Close()

	var out []QueueEntry
	for rows.Next() {
		var e QueueEntry
		var status string
		if err := rows.Scan(&e.OrderID, &e.ExternalID, &e.SenderAddress, &status); err != nil {
			return nil, fmt.Errorf("discovery: listing orders ready for screening: %w", err)
		}
		e.Status = Status(status)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("discovery: listing orders ready for screening: %w", err)
	}
	return out, nil
}

// MarkScreening moves orderID from PENDING to SCREENING -- claimed by
// this tick, so a concurrent or overlapping tick's own
// ListReadyForScreening no longer returns it. Restarting mid-screening
// (a row stuck in SCREENING) is safe to reprocess: internal/pipeline's
// own ReportVerdict call is idempotent, so re-running it is a no-op on
// C1's side, never a second conflicting transition.
func MarkScreening(ctx context.Context, q Queryer, orderID int64) error {
	if _, err := q.Exec(ctx, `UPDATE screening_queue SET status = $1, updated_at = now() WHERE order_id = $2`,
		string(Screening), orderID); err != nil {
		return fmt.Errorf("discovery: marking order %d SCREENING: %w", orderID, err)
	}
	return nil
}

// MarkDone moves orderID to DONE -- the pipeline's own terminal state,
// reached either after a real verdict was successfully reported or after
// C1 says the order already left funded (a legitimate race, not an
// error -- see ledgerclient.ErrIllegalTransition).
func MarkDone(ctx context.Context, q Queryer, orderID int64) error {
	if _, err := q.Exec(ctx, `UPDATE screening_queue SET status = $1, updated_at = now() WHERE order_id = $2`,
		string(Done), orderID); err != nil {
		return fmt.Errorf("discovery: marking order %d DONE: %w", orderID, err)
	}
	return nil
}

// Get looks up one screening_queue row by order id -- exported for
// callers (tests, and eventually C3.4/C3.9's HTTP surface) that need to
// verify or consume what discovery has recorded.
func Get(ctx context.Context, q Queryer, orderID int64) (QueueEntry, error) {
	var e QueueEntry
	var status string
	err := q.QueryRow(ctx, `
		SELECT order_id, external_id, sender_address, status
		FROM screening_queue WHERE order_id = $1
	`, orderID).Scan(&e.OrderID, &e.ExternalID, &e.SenderAddress, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return QueueEntry{}, fmt.Errorf("%w: order %d", ErrNotFound, orderID)
	}
	if err != nil {
		return QueueEntry{}, fmt.Errorf("discovery: getting order %d: %w", orderID, err)
	}
	e.Status = Status(status)
	return e, nil
}

// QueueStats is screening_queue's own operator-facing summary --
// C3.8's GET /v1/system/queue, and the reactive queue_depth /
// queue_oldest_pending_age_seconds metrics it exposes.
type QueueStats struct {
	// DepthByStatus counts every row currently in each status. A status
	// with zero rows is still present in the map (as 0), never omitted,
	// so a caller never has to guess whether "missing" means zero or
	// "wasn't checked".
	DepthByStatus map[Status]int
	// OldestPendingEnqueuedAt is the enqueued_at of the longest-waiting
	// PENDING row, or nil if there are none.
	OldestPendingEnqueuedAt *time.Time
}

// Stats computes the current QueueStats summary directly from
// screening_queue -- always fresh at call time (a reactive read for a
// metrics scrape or an operator's own GET, never a value that could
// drift from what the table actually says between updates).
func Stats(ctx context.Context, q Queryer) (QueueStats, error) {
	stats := QueueStats{DepthByStatus: map[Status]int{Pending: 0, Screening: 0, Done: 0}}

	rows, err := q.Query(ctx, `SELECT status, count(*) FROM screening_queue GROUP BY status`)
	if err != nil {
		return QueueStats{}, fmt.Errorf("discovery: computing queue depth: %w", err)
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return QueueStats{}, fmt.Errorf("discovery: computing queue depth: %w", err)
		}
		stats.DepthByStatus[Status(status)] = count
	}
	if err := rows.Err(); err != nil {
		return QueueStats{}, fmt.Errorf("discovery: computing queue depth: %w", err)
	}

	var oldest *time.Time
	err = q.QueryRow(ctx, `SELECT min(enqueued_at) FROM screening_queue WHERE status = $1`, string(Pending)).Scan(&oldest)
	if err != nil {
		return QueueStats{}, fmt.Errorf("discovery: computing oldest pending age: %w", err)
	}
	stats.OldestPendingEnqueuedAt = oldest
	return stats, nil
}
