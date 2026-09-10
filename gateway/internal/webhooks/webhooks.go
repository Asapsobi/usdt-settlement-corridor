// Package webhooks is C6.6: the trigger loop that notices an order
// reach settled/held/refunded (polling GET /v1/orders?state=X the same
// way every sibling component discovers its own work, per "Read this
// fourth" -- there is still no event bus), and the delivery loop that
// HMAC-signs and POSTs the resulting webhook_deliveries row to each
// customer's own endpoint, retrying with backoff up to
// DefaultMaxAttempts times before handing off to C6.5's own
// pull-based backstop.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
)

// Delivery is one row of webhook_deliveries.
type Delivery struct {
	ID            int64
	CustomerID    int64
	ExternalID    string
	EventType     string
	Payload       json.RawMessage
	CreatedAt     time.Time
	DeliveredAt   *time.Time
	AttemptCount  int
	NextAttemptAt time.Time
	LastError     *string
}

// Store is the webhook_deliveries table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Enqueue inserts a new delivery row for one (external_id, event_type)
// state transition -- idempotent (ON CONFLICT DO NOTHING): the trigger
// loop's own re-poll of a cursor window it already advanced past is a
// harmless no-op, never a duplicate delivery. Reports whether this call
// was the one that actually inserted the row.
func (s *Store) Enqueue(ctx context.Context, customerID int64, externalID, eventType string, payload json.RawMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (customer_id, external_id, event_type, payload)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (external_id, event_type) DO NOTHING
	`, customerID, externalID, eventType, []byte(payload))
	if err != nil {
		return false, fmt.Errorf("webhooks: enqueuing %s/%s: %w", externalID, eventType, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DefaultMaxAttempts is how many delivery attempts a row gets before
// ClaimDue stops returning it and the delivery loop alerts -- decision
// 5's own "8 retries."
const DefaultMaxAttempts = 8

// ClaimDue returns every not-yet-delivered row due now, oldest first,
// up to limit -- rows still within DefaultMaxAttempts only; an
// exhausted row simply stops being returned, same "the row's own state
// gates whether it's picked up again" pattern this whole project uses.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, external_id, event_type, payload, created_at, delivered_at, attempt_count, next_attempt_at, last_error
		FROM webhook_deliveries
		WHERE delivered_at IS NULL AND attempt_count < $1 AND next_attempt_at <= $2
		ORDER BY next_attempt_at ASC
		LIMIT $3
	`, DefaultMaxAttempts, now, limit)
	if err != nil {
		return nil, fmt.Errorf("webhooks: claiming due deliveries: %w", err)
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDelivered records a successful delivery.
func (s *Store) MarkDelivered(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries SET delivered_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("webhooks: marking %d delivered: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordFailure bumps attempt_count, stores lastErr, and schedules
// nextAttemptAt -- returning the row's new attempt_count so the caller
// can tell "just now became exhausted" (newAttemptCount ==
// DefaultMaxAttempts) from every attempt before it, the same
// claim-via-the-row pattern reconcile.MarkAddressPendingAlerted uses
// for its own "alert once" guarantee: whichever caller's UPDATE
// produces exactly DefaultMaxAttempts is the one and only one to
// observe that transition.
func (s *Store) RecordFailure(ctx context.Context, id int64, lastErr string, nextAttemptAt time.Time) (newAttemptCount int, err error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE webhook_deliveries
		SET attempt_count = attempt_count + 1, last_error = $1, next_attempt_at = $2
		WHERE id = $3
		RETURNING attempt_count
	`, lastErr, nextAttemptAt, id)
	if err := row.Scan(&newAttemptCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("webhooks: recording failure for %d: %w", id, err)
	}
	return newAttemptCount, nil
}

// ErrNotFound means no webhook_deliveries row exists for the given id.
var ErrNotFound = errors.New("webhooks: no such delivery")

type scanRow interface {
	Scan(dest ...any) error
}

func scanDelivery(row scanRow) (Delivery, error) {
	var d Delivery
	var payload []byte
	if err := row.Scan(&d.ID, &d.CustomerID, &d.ExternalID, &d.EventType, &payload, &d.CreatedAt, &d.DeliveredAt,
		&d.AttemptCount, &d.NextAttemptAt, &d.LastError); err != nil {
		return Delivery{}, err
	}
	d.Payload = payload
	return d, nil
}
