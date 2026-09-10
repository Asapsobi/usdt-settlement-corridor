//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/db"
)

// fakeCustomerLookup stands in for customers.Store -- a fixed
// secret/URL pair, matching CustomerLookup's shape directly rather
// than standing up a real customer row for every test.
type fakeCustomerLookup struct {
	secret string
	url    string
}

func (f fakeCustomerLookup) WebhookSecretFor(ctx context.Context, customerID int64) (string, error) {
	return f.secret, nil
}
func (f fakeCustomerLookup) WebhookURLFor(ctx context.Context, customerID int64) (string, error) {
	return f.url, nil
}

// fetchDeliverable re-reads delivery id, ignoring its own scheduled
// next_attempt_at -- lets a test drive repeated attempts without a
// real sleep between them, the same "bypass the schedule, drive Tick's
// own lower-level step directly" approach reconcile's own tests use
// via a relaxed grace period.
func fetchDeliverable(t *testing.T, store *Store, id int64) Delivery {
	t.Helper()
	due, err := store.ClaimDue(t.Context(), time.Now().UTC().Add(24*time.Hour), 100)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	for _, d := range due {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("delivery %d not found among due rows (already delivered or exhausted?)", id)
	return Delivery{}
}

func TestDeliverer_RetriesUntilCustomerEndpointSucceeds(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)

	var calls atomic.Int32
	var lastSig, lastEvent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		lastSig = r.Header.Get("Webhook-Signature")
		lastEvent = r.Header.Get("Webhook-Event")
		if n <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	lookup := fakeCustomerLookup{secret: "test-secret", url: srv.URL}
	d := NewDeliverer(store, lookup, nil, nil)

	externalID := fmt.Sprintf("ext-deliver-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(eventPayload{ExternalID: externalID, EventType: "settled", State: "settled"})
	inserted, err := store.Enqueue(t.Context(), 1, externalID, "settled", payload)
	if err != nil || !inserted {
		t.Fatalf("Enqueue: inserted=%v err=%v", inserted, err)
	}

	row, err := findByExternalID(t, pool, externalID, "settled")
	if err != nil {
		t.Fatalf("findByExternalID: %v", err)
	}

	for i := 0; i < 4; i++ {
		delivery := fetchDeliverable(t, store, row.ID)
		d.attempt(t.Context(), delivery)
	}

	if got := calls.Load(); got != 4 {
		t.Fatalf("customer endpoint received %d requests, want exactly 4 (3 failures + 1 success)", got)
	}
	if lastEvent != "settled" {
		t.Errorf("Webhook-Event header = %q, want settled", lastEvent)
	}
	if lastSig == "" || lastSig[:7] != "sha256=" {
		t.Errorf("Webhook-Signature header = %q, want sha256=<hex>", lastSig)
	}
	final, err := findByExternalID(t, pool, externalID, "settled")
	if err != nil {
		t.Fatalf("findByExternalID (final): %v", err)
	}
	// Signed over the row's own stored payload bytes, not the pre-insert
	// value -- Postgres's jsonb column does not preserve byte-for-byte
	// input formatting, so the bytes actually delivered (and that a real
	// customer would verify against) are whatever jsonb round-trips back,
	// not necessarily identical to what json.Marshal produced here.
	expectedSig := "sha256=" + sign("test-secret", final.Payload)
	if lastSig != expectedSig {
		t.Errorf("Webhook-Signature = %q, want %q", lastSig, expectedSig)
	}
	if final.DeliveredAt == nil {
		t.Errorf("DeliveredAt is nil, want set after the 4th (successful) attempt")
	}
	if final.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d, want 3 (only failures increment it; the successful attempt marks delivered instead)", final.AttemptCount)
	}

	// The now-delivered row must never be claimable again -- ClaimDue's
	// own WHERE delivered_at IS NULL excludes it. Checked directly
	// against ClaimDue rather than by calling the real, global Tick:
	// gateway_test is a real, persistent database shared across test
	// runs, so a global Tick would also attempt delivery of unrelated
	// stray rows left over from other tests, which this test's own
	// fakeCustomerLookup (a fixed URL for any customer id) would
	// misroute to this same httptest.Server and inflate calls for
	// reasons that have nothing to do with this row.
	due, err := store.ClaimDue(t.Context(), time.Now().UTC().Add(24*time.Hour), 1000)
	if err != nil {
		t.Fatalf("ClaimDue after delivery: %v", err)
	}
	for _, dd := range due {
		if dd.ID == row.ID {
			t.Fatalf("delivered row %d is still returned by ClaimDue", row.ID)
		}
	}
}

func TestDeliverer_ExhaustsAfterMaxAttemptsAndAlertsOnce(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	lookup := fakeCustomerLookup{secret: "test-secret", url: srv.URL}
	alerter := &countingWebhookAlerter{}
	d := NewDeliverer(store, lookup, alerter, nil)

	externalID := fmt.Sprintf("ext-exhaust-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(eventPayload{ExternalID: externalID, EventType: "held", State: "held"})
	if _, err := store.Enqueue(t.Context(), 1, externalID, "held", payload); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	row, err := findByExternalID(t, pool, externalID, "held")
	if err != nil {
		t.Fatalf("findByExternalID: %v", err)
	}

	// DefaultMaxAttempts failing attempts, then one extra to confirm no
	// 9th attempt is ever made.
	for i := 0; i < DefaultMaxAttempts; i++ {
		delivery := fetchDeliverable(t, store, row.ID)
		d.attempt(t.Context(), delivery)
	}
	if got := calls.Load(); got != DefaultMaxAttempts {
		t.Fatalf("customer endpoint received %d requests, want exactly %d", got, DefaultMaxAttempts)
	}

	// The row should no longer be claimable at all -- exhausted, not due.
	stillDue, err := store.ClaimDue(t.Context(), time.Now().UTC().Add(24*time.Hour), 100)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	for _, dd := range stillDue {
		if dd.ID == row.ID {
			t.Fatalf("exhausted row %d is still returned by ClaimDue", row.ID)
		}
	}

	if got := alerter.calls.Load(); got != 1 {
		t.Errorf("alert fired %d times, want exactly 1", got)
	}
}

type countingWebhookAlerter struct {
	calls atomic.Int32
}

func (a *countingWebhookAlerter) AlertWebhookExhausted(ctx context.Context, d Delivery) {
	a.calls.Add(1)
}

// findByExternalID re-reads a delivery row directly by its unique
// (external_id, event_type) key -- a plain SELECT, no ClaimDue
// scheduling filter, so it works regardless of the row's own
// delivered/exhausted state.
func findByExternalID(t *testing.T, pool *db.Pool, externalID, eventType string) (Delivery, error) {
	t.Helper()
	row := pool.QueryRow(t.Context(), `
		SELECT id, customer_id, external_id, event_type, payload, created_at, delivered_at, attempt_count, next_attempt_at, last_error
		FROM webhook_deliveries WHERE external_id = $1 AND event_type = $2
	`, externalID, eventType)
	return scanDelivery(row)
}
