package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// DefaultDeliverInterval is how often RunDeliverLoop checks for due
// rows, when unset.
const DefaultDeliverInterval = 5 * time.Second

// DefaultBatchSize caps how many rows one Tick claims -- real volume
// here is low (~100 orders/day, per every C*.9's own performance
// framing), so this is generous headroom, not a real limit.
const DefaultBatchSize = 50

// backoffSchedule[i] is how long to wait before attempt i+2, indexed
// by the just-recorded attempt_count (1-based) -- exponential,
// deliberately capped well under an hour since decision 5's own
// backstop (C6.5) is always available regardless of how long delivery
// takes to exhaust.
var backoffSchedule = []time.Duration{
	10 * time.Second, 30 * time.Second, 1 * time.Minute, 5 * time.Minute,
	15 * time.Minute, 30 * time.Minute, 1 * time.Hour, // index 6 (attempt 7); attempt 8 is the last try, no further backoff needed
}

func backoffAfter(attemptCount int) time.Duration {
	if attemptCount-1 < len(backoffSchedule) {
		return backoffSchedule[attemptCount-1]
	}
	return backoffSchedule[len(backoffSchedule)-1]
}

// Alerter surfaces a delivery that exhausted every attempt -- optional
// (nil means no alert, never a panic), same convention every other
// pluggable dependency in this project's own sibling modules uses.
type Alerter interface {
	AlertWebhookExhausted(ctx context.Context, d Delivery)
}

// MetricsRecorder is how this package reports C6.8's own
// webhook_deliveries_total{result} and webhook_retry_exhausted_total --
// optional (nil means no metrics recorded, never a panic), same
// convention screening's own pipeline.MetricsRecorder established.
type MetricsRecorder interface {
	DeliveryAttempted(success bool)
	RetryExhausted()
}

// CustomerLookup resolves a customer id to the two pieces of their own
// record delivery needs: WebhookSecretFor for signing (the raw HMAC
// key, per Customer's own doc comment on why it's stored unhashed) and
// WebhookURLFor for where to send it. customers.Store satisfies this
// directly.
type CustomerLookup interface {
	WebhookSecretFor(ctx context.Context, customerID int64) (string, error)
	WebhookURLFor(ctx context.Context, customerID int64) (string, error)
}

// Deliverer sends due webhook_deliveries rows and records the outcome.
type Deliverer struct {
	store     *Store
	customers CustomerLookup
	alerter   Alerter
	metrics   MetricsRecorder
	http      *http.Client
}

// NewDeliverer wires a Deliverer. alerter and metrics may both be nil.
func NewDeliverer(store *Store, customers CustomerLookup, alerter Alerter, metrics MetricsRecorder) *Deliverer {
	return &Deliverer{store: store, customers: customers, alerter: alerter, metrics: metrics, http: &http.Client{Timeout: 10 * time.Second}}
}

// sign computes the HMAC-SHA256 signature C6.6's own webhooks carry --
// hex-encoded, over the exact payload bytes sent.
func sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// RunDeliverLoop ticks on interval until ctx is cancelled, returning
// ctx.Err(). Runs one Tick immediately.
func (d *Deliverer) RunDeliverLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultDeliverInterval
	}

	if err := d.Tick(ctx); err != nil {
		slog.Error("webhooks: initial deliver tick failed", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := d.Tick(ctx); err != nil {
				slog.Error("webhooks: deliver tick failed", "error", err)
			}
		}
	}
}

// Tick claims every due row and attempts delivery once each. One row's
// failure never blocks the rest of the batch.
func (d *Deliverer) Tick(ctx context.Context) error {
	now := time.Now().UTC()
	due, err := d.store.ClaimDue(ctx, now, DefaultBatchSize)
	if err != nil {
		return fmt.Errorf("webhooks: claiming due deliveries: %w", err)
	}

	for _, delivery := range due {
		d.attempt(ctx, delivery)
	}
	return nil
}

// attempt makes exactly one HTTP delivery attempt for delivery, then
// records success or failure. Never returns an error -- a single row's
// problem (a bad secret lookup, a network failure, a non-2xx response)
// is logged and left for the next tick to retry, exactly like every
// other per-row loop in this project.
func (d *Deliverer) attempt(ctx context.Context, delivery Delivery) {
	secret, err := d.customers.WebhookSecretFor(ctx, delivery.CustomerID)
	if err != nil {
		slog.Error("webhooks: looking up webhook_secret failed, will retry", "delivery_id", delivery.ID, "error", err)
		return
	}

	url, err := d.customers.WebhookURLFor(ctx, delivery.CustomerID)
	if err != nil {
		slog.Error("webhooks: looking up webhook_url failed, will retry", "delivery_id", delivery.ID, "error", err)
		return
	}
	if url == "" {
		// The customer cleared their webhook_url after this row was
		// enqueued -- nothing left to deliver to; C6.5's own backstop is
		// this order's only path forward now. Mark delivered so this row
		// stops being retried forever against nowhere.
		if err := d.store.MarkDelivered(ctx, delivery.ID); err != nil {
			slog.Error("webhooks: marking undeliverable row delivered failed", "delivery_id", delivery.ID, "error", err)
		}
		return
	}

	sig := sign(secret, delivery.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(delivery.Payload))
	if err != nil {
		slog.Error("webhooks: building request failed, will retry", "delivery_id", delivery.ID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Signature", "sha256="+sig)
	req.Header.Set("Webhook-Event", delivery.EventType)
	req.Header.Set("Webhook-Delivery-Id", strconv.FormatInt(delivery.ID, 10))

	resp, err := d.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := d.store.MarkDelivered(ctx, delivery.ID); err != nil {
			slog.Error("webhooks: marking delivered failed", "delivery_id", delivery.ID, "error", err)
			return
		}
		if d.metrics != nil {
			d.metrics.DeliveryAttempted(true)
		}
		return
	}

	failMsg := deliveryFailureMessage(err, resp)
	newAttemptCount, recErr := d.store.RecordFailure(ctx, delivery.ID, failMsg, time.Now().UTC().Add(backoffAfter(delivery.AttemptCount+1)))
	if recErr != nil {
		slog.Error("webhooks: recording delivery failure failed", "delivery_id", delivery.ID, "error", recErr)
		return
	}
	if d.metrics != nil {
		d.metrics.DeliveryAttempted(false)
	}
	slog.Warn("webhooks: delivery attempt failed, will retry", "delivery_id", delivery.ID, "attempt", newAttemptCount, "error", failMsg)

	if newAttemptCount == DefaultMaxAttempts {
		slog.Error("webhooks: delivery exhausted every attempt", "delivery_id", delivery.ID, "external_id", delivery.ExternalID)
		if d.alerter != nil {
			d.alerter.AlertWebhookExhausted(ctx, delivery)
		}
		if d.metrics != nil {
			d.metrics.RetryExhausted()
		}
	}
}

func deliveryFailureMessage(err error, resp *http.Response) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("customer endpoint returned %d", resp.StatusCode)
}
