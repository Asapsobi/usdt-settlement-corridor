package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gateway/internal/c1client"
	"gateway/internal/db"
)

// DefaultTriggerInterval is how often RunTriggerLoop polls C1, when
// unset -- "a tight dedicated poll (default 5s, much tighter than C3's
// or C4's own background intervals)" per "Read this fourth": this is
// the one polling loop in the system with a customer-facing SLA
// sitting on top of it.
const DefaultTriggerInterval = 5 * time.Second

// watchedStates are the only C1 order states this loop ever fires a
// webhook for -- decision 5's own "settled, held, refunded".
var watchedStates = []string{"settled", "held", "refunded"}

// OrderPoller is the one call this loop needs from C1 --
// c1client.Client's real implementation, or a fake for testing.
type OrderPoller interface {
	ListOrdersByState(ctx context.Context, state, cursor string) (orders []c1client.Order, newCursor string, err error)
}

// Trigger polls C1 for settled/held/refunded transitions and enqueues
// one webhook_deliveries row per transition observed, for customers
// with a webhook_url configured.
type Trigger struct {
	pool   *db.Pool
	poller OrderPoller
	store  *Store
	lookup WebhookURLLookup
}

// WebhookURLLookup resolves a C1 customer_id string (this gateway's own
// numeric customer id, formatted as a string -- see c1client.Order's
// own CustomerID field) to that customer's configured webhook_url, or
// "" if none is set. A customer with none configured is skipped --
// see webhook_deliveries' own migration comment.
type WebhookURLLookup interface {
	WebhookURLFor(ctx context.Context, customerID int64) (string, error)
}

// NewTrigger wires a Trigger.
func NewTrigger(pool *db.Pool, poller OrderPoller, store *Store, lookup WebhookURLLookup) *Trigger {
	return &Trigger{pool: pool, poller: poller, store: store, lookup: lookup}
}

// eventPayload is the JSON body every webhook delivers -- deliberately
// small and customer-safe, the same fields C6.5's own status endpoint
// exposes, never an internal C1 id or account code.
type eventPayload struct {
	ExternalID string `json:"external_id"`
	EventType  string `json:"event_type"`
	State      string `json:"state"`
	AmountIn   string `json:"amount_in"`
	AmountOut  string `json:"amount_out"`
	Tier       string `json:"tier"`
}

// RunTriggerLoop polls on interval until ctx is cancelled, returning
// ctx.Err(). Runs one Tick immediately.
func (t *Trigger) RunTriggerLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultTriggerInterval
	}

	if err := t.Tick(ctx); err != nil {
		slog.Error("webhooks: initial trigger tick failed", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := t.Tick(ctx); err != nil {
				slog.Error("webhooks: trigger tick failed", "error", err)
			}
		}
	}
}

// Tick polls every watched state once, enqueueing a delivery for each
// newly observed order and advancing that state's own cursor. One
// state's failure never blocks the others.
func (t *Trigger) Tick(ctx context.Context) error {
	for _, state := range watchedStates {
		if err := t.tickState(ctx, state); err != nil {
			slog.Error("webhooks: trigger tick failed for state", "state", state, "error", err)
		}
	}
	return nil
}

func (t *Trigger) tickState(ctx context.Context, state string) error {
	cursor, err := getCursor(ctx, t.pool, state)
	if err != nil {
		return err
	}

	orders, newCursor, err := t.poller.ListOrdersByState(ctx, state, cursor)
	if err != nil {
		return fmt.Errorf("webhooks: polling state %q: %w", state, err)
	}

	for _, o := range orders {
		customerID, err := strconv.ParseInt(o.CustomerID, 10, 64)
		if err != nil {
			slog.Error("webhooks: order carries a non-numeric customer_id, skipping",
				"external_id", o.ExternalID, "customer_id", o.CustomerID, "error", err)
			continue
		}

		url, err := t.lookup.WebhookURLFor(ctx, customerID)
		if err != nil {
			slog.Error("webhooks: looking up webhook_url failed, skipping this tick",
				"external_id", o.ExternalID, "customer_id", customerID, "error", err)
			continue
		}
		if url == "" {
			continue // no endpoint configured -- nothing to deliver
		}

		payload, err := json.Marshal(eventPayload{
			ExternalID: o.ExternalID, EventType: state, State: state,
			AmountIn: o.AmountIn.Format(), AmountOut: o.AmountOut.Format(), Tier: o.Tier,
		})
		if err != nil {
			return fmt.Errorf("webhooks: encoding payload for %q: %w", o.ExternalID, err)
		}
		if _, err := t.store.Enqueue(ctx, customerID, o.ExternalID, state, payload); err != nil {
			return err
		}
	}

	if newCursor != "" && newCursor != cursor {
		if err := setCursor(ctx, t.pool, state, newCursor); err != nil {
			return err
		}
	}
	return nil
}
