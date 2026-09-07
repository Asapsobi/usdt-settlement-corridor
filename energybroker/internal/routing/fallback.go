package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"energybroker/internal/db"
)

// Fallback reason values -- manual_fallback_events.reason's own
// vocabulary, distinct from Selection.Reason's code-flavored constants
// (ReasonFallbackLadder/ReasonManualRequired) because this is what an
// operator reads in the runbook and in alerts, not an internal routing
// decision label. See reasonForSelection for the mapping between the
// two.
const (
	FallbackReasonAllUnhealthy   = "all_unhealthy"
	FallbackReasonAllOverCeiling = "all_over_ceiling"
)

// KnownFallbackReasons lists every FallbackReason constant above, for
// the runbook-drift test (fallback_test.go's own
// TestRunbookCoversEveryFallbackReason) -- mirroring C1.10's own
// halt.KnownReasons/TestRunbookCoversEveryReason pattern exactly.
var KnownFallbackReasons = []string{FallbackReasonAllUnhealthy, FallbackReasonAllOverCeiling}

// reasonForSelection maps a Selection's own Reason to this table's own
// reason vocabulary. Returns an error for ReasonWeighted (there is
// nothing to record -- a weighted pick isn't a fallback at all) or any
// other unrecognized value, so a caller mistakenly triggering this for a
// successful selection fails loudly rather than recording a nonsense row.
func reasonForSelection(selectionReason string) (string, error) {
	switch selectionReason {
	case ReasonFallbackLadder:
		return FallbackReasonAllUnhealthy, nil
	case ReasonManualRequired:
		return FallbackReasonAllOverCeiling, nil
	default:
		return "", fmt.Errorf("routing: %q is not a fallback Selection.Reason (OnFallbackTriggered is only for ReasonFallbackLadder/ReasonManualRequired)", selectionReason)
	}
}

// FallbackEvent is one manual_fallback_events row.
type FallbackEvent struct {
	ID          int64
	TriggeredAt time.Time
	Reason      string
	OrderID     *int64
	ResolvedAt  *time.Time
	Resolution  *string
}

// ErrFallbackEventNotFound means no manual_fallback_events row exists
// with the given id.
var ErrFallbackEventNotFound = errors.New("routing: no such fallback event")

// OnFallbackTriggered records a manual_fallback_events row for
// selectionReason (a SelectProvider result's own ReasonFallbackLadder or
// ReasonManualRequired), de-duplicated against any already-OPEN
// (unresolved) event for the same reason -- several reservations hitting
// the same outage window, or the buffer's own replenishment loop ticking
// repeatedly through it, must produce exactly one row, not one per
// caller. orderID is the specific reservation that triggered this, or
// nil if Replenish's own background loop detected it before any order
// needed the energy.
//
// This never attempts a JustLendDAO call itself -- per "Read this
// second" in this component's own build spec, that stays a human-
// executed runbook step at MVP (see docs/runbook-energy-fallback.md).
// This function's only job is: record it, alert loudly (a structured
// log today; a real alerting hook is a later, separate integration --
// same posture C2.8 took for its own orphaned-deposit alerting), and
// return the event so a caller can log its id.
func (r *Router) OnFallbackTriggered(ctx context.Context, selectionReason string, orderID *int64) (FallbackEvent, error) {
	reason, err := reasonForSelection(selectionReason)
	if err != nil {
		return FallbackEvent{}, err
	}

	if existing, err := openFallbackEventByReason(ctx, r.pool, reason); err != nil {
		return FallbackEvent{}, err
	} else if existing != nil {
		return *existing, nil
	}

	event, err := insertFallbackEvent(ctx, r.pool, reason, orderID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Lost a race to a concurrent trigger for the same reason --
			// its row is the correct one to return, not an error.
			existing, getErr := openFallbackEventByReason(ctx, r.pool, reason)
			if getErr != nil {
				return FallbackEvent{}, getErr
			}
			if existing != nil {
				return *existing, nil
			}
		}
		return FallbackEvent{}, err
	}

	slog.Error("routing: MANUAL FALLBACK TRIGGERED -- see docs/runbook-energy-fallback.md",
		"event_id", event.ID, "reason", reason, "order_id", orderID)
	return event, nil
}

// Resolve marks eventID resolved -- the runbook's own "mark this event
// resolved" step, once an operator has either manually delegated energy
// via JustLendDAO's UI as a stopgap or confirmed the vendors recovered
// on their own.
func (r *Router) Resolve(ctx context.Context, eventID int64, resolution string) error {
	if resolution == "" {
		return errors.New("routing: Resolve requires a non-empty resolution")
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE manual_fallback_events SET resolved_at = now(), resolution = $1
		WHERE id = $2 AND resolved_at IS NULL
	`, resolution, eventID)
	if err != nil {
		return fmt.Errorf("routing: resolving fallback event %d: %w", eventID, err)
	}
	if tag.RowsAffected() == 0 {
		// Either eventID doesn't exist, or it's already resolved --
		// resolving twice is a caller bug or a stale UI, not silently
		// swallowed, the same posture holds.Release takes in screening.
		existing, getErr := getFallbackEvent(ctx, r.pool, eventID)
		if getErr == nil && existing != nil {
			return fmt.Errorf("routing: fallback event %d is already resolved", eventID)
		}
		return fmt.Errorf("%w: %d", ErrFallbackEventNotFound, eventID)
	}
	return nil
}

func getFallbackEvent(ctx context.Context, pool db.Queryer, id int64) (*FallbackEvent, error) {
	row := pool.QueryRow(ctx, `
		SELECT id, triggered_at, reason, order_id, resolved_at, resolution
		FROM manual_fallback_events WHERE id = $1
	`, id)
	return scanFallbackEvent(row)
}

func openFallbackEventByReason(ctx context.Context, pool db.Queryer, reason string) (*FallbackEvent, error) {
	row := pool.QueryRow(ctx, `
		SELECT id, triggered_at, reason, order_id, resolved_at, resolution
		FROM manual_fallback_events WHERE reason = $1 AND resolved_at IS NULL
	`, reason)
	return scanFallbackEvent(row)
}

func insertFallbackEvent(ctx context.Context, pool db.Queryer, reason string, orderID *int64) (FallbackEvent, error) {
	row := pool.QueryRow(ctx, `
		INSERT INTO manual_fallback_events (reason, order_id)
		VALUES ($1, $2)
		RETURNING id, triggered_at, reason, order_id, resolved_at, resolution
	`, reason, orderID)
	event, err := scanFallbackEvent(row)
	if err != nil {
		return FallbackEvent{}, err
	}
	return *event, nil
}

func scanFallbackEvent(row pgx.Row) (*FallbackEvent, error) {
	var e FallbackEvent
	if err := row.Scan(&e.ID, &e.TriggeredAt, &e.Reason, &e.OrderID, &e.ResolvedAt, &e.Resolution); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("routing: scanning fallback event: %w", err)
	}
	return &e, nil
}
