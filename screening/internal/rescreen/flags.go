package rescreen

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

// Flag is one rescreen_flags row: a sender address whose fresh verdict
// disagreed with the one that let an order through, for order (still
// screened or dispatching, not yet settled).
type Flag struct {
	ID                    int64
	OrderID               int64
	ExternalID            string
	OrderStateAtDetection string
	PreviousVerdictID     int64
	NewVerdictID          int64
	DetectedAt            time.Time
	Resolution            *string
	ResolvedAt            *time.Time
}

// ErrNotFound means no rescreen_flags row exists with the given id.
var ErrNotFound = errors.New("rescreen: no such flag")

// recordFlag inserts a new rescreen_flags row. Never called twice for
// the same (order, mismatch): once a mismatch is recorded, the fresh
// verdict this tick just cached (via cache.Put) becomes the LATEST row
// for that address, so the next tick's own LatestAny lookup sees an
// already-Hold-classified "previous" verdict and the routine "no
// disagreement" branch fires -- no explicit dedup guard is needed here,
// see RunTick's own doc comment.
func recordFlag(ctx context.Context, q Queryer, orderID int64, externalID, stateAtDetection string, previousVerdictID, newVerdictID int64) (Flag, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO rescreen_flags (order_id, external_id, order_state_at_detection, previous_verdict_id, new_verdict_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, order_id, external_id, order_state_at_detection, previous_verdict_id, new_verdict_id, detected_at, resolution, resolved_at
	`, orderID, externalID, stateAtDetection, previousVerdictID, newVerdictID)
	f, err := scanFlag(row)
	if err != nil {
		return Flag{}, fmt.Errorf("rescreen: recording flag for order %d (%s): %w", orderID, externalID, err)
	}
	return f, nil
}

// ListUnresolved returns every rescreen_flags row with no resolution
// yet, oldest first.
func ListUnresolved(ctx context.Context, q Queryer) ([]Flag, error) {
	rows, err := q.Query(ctx, `
		SELECT id, order_id, external_id, order_state_at_detection, previous_verdict_id, new_verdict_id, detected_at, resolution, resolved_at
		FROM rescreen_flags WHERE resolution IS NULL ORDER BY detected_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("rescreen: listing unresolved flags: %w", err)
	}
	defer rows.Close()

	var out []Flag
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, fmt.Errorf("rescreen: listing unresolved flags: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rescreen: listing unresolved flags: %w", err)
	}
	return out, nil
}

// Get looks up one flag by id.
func Get(ctx context.Context, q Queryer, id int64) (Flag, error) {
	row := q.QueryRow(ctx, `
		SELECT id, order_id, external_id, order_state_at_detection, previous_verdict_id, new_verdict_id, detected_at, resolution, resolved_at
		FROM rescreen_flags WHERE id = $1
	`, id)
	f, err := scanFlag(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Flag{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	if err != nil {
		return Flag{}, fmt.Errorf("rescreen: getting flag %d: %w", id, err)
	}
	return f, nil
}

// Resolve records how a human decided to handle flag id -- purely a
// record of the decision (this package never acts on it, see this
// chunk's own build spec: mechanism, not policy).
func Resolve(ctx context.Context, q Queryer, id int64, resolution string) error {
	if resolution == "" {
		return errors.New("rescreen: resolution must not be empty")
	}
	tag, err := q.Exec(ctx, `
		UPDATE rescreen_flags SET resolution = $1, resolved_at = now() WHERE id = $2 AND resolution IS NULL
	`, resolution, id)
	if err != nil {
		return fmt.Errorf("rescreen: resolving flag %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %d (or already resolved)", ErrNotFound, id)
	}
	return nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanFlag(row scanRow) (Flag, error) {
	var f Flag
	err := row.Scan(&f.ID, &f.OrderID, &f.ExternalID, &f.OrderStateAtDetection,
		&f.PreviousVerdictID, &f.NewVerdictID, &f.DetectedAt, &f.Resolution, &f.ResolvedAt)
	if err != nil {
		return Flag{}, err
	}
	return f, nil
}
