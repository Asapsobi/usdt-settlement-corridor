package ledgerclient

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestTwoCallReporter_HappyPath(t *testing.T) {
	var gotReversalEntryID string
	var gotTransitionBody postTransitionRequest
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/entries":
			_ = json.NewEncoder(w).Encode(entryResponse{ID: 55, IdempotencyKey: "dispatcher:enter_dispatching:1", Outcome: "created"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/entries/55/reversal":
			gotReversalEntryID = "55"
			ref := int64(55)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(entryResponse{ID: 56, IdempotencyKey: "ledger:reverse:dispatcher:enter_dispatching:1", ReversalOf: &ref, Outcome: "created"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/order-1/transitions":
			_ = json.NewDecoder(r.Body).Decode(&gotTransitionBody)
			_ = json.NewEncoder(w).Encode(orderResponse{ID: 1, ExternalID: "order-1", State: "held", Version: 5,
				AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	c := New(ts.URL, "test-token")
	reporter := NewTwoCallReporter(c)
	order := Order{ID: 1, ExternalID: "order-1", Version: 4}

	err := reporter.ReportDispatchFailure(t.Context(), order, "dispatcher:enter_dispatching:1", "energy reservation failed permanently")
	if err != nil {
		t.Fatalf("ReportDispatchFailure: %v", err)
	}
	if gotReversalEntryID != "55" {
		t.Fatalf("reversal was posted against entry %q, want 55", gotReversalEntryID)
	}
	if gotTransitionBody.ToState != "held" || gotTransitionBody.EntryID == nil || *gotTransitionBody.EntryID != 56 {
		t.Fatalf("transition body = %+v, want ToState=held EntryID=56", gotTransitionBody)
	}
	if gotTransitionBody.Entry != nil {
		t.Fatalf("transition body Entry = %+v, want nil (this transition names an existing entry, never posts a new one)", gotTransitionBody.Entry)
	}
}

// TestTwoCallReporter_RetryAfterCrashBetweenReversalAndTransition
// simulates exactly the crash invariant 4 names: the reversal already
// landed (C1 now reports 409 already_reversed with it embedded), but the
// held transition never happened. A retried ReportDispatchFailure call
// must complete the transition without ever re-attempting the reversal
// POST as a fresh (non-conflicting) request.
func TestTwoCallReporter_RetryAfterCrashBetweenReversalAndTransition(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/entries":
			_ = json.NewEncoder(w).Encode(entryResponse{ID: 55, IdempotencyKey: "dispatcher:enter_dispatching:2", Outcome: "created"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/entries/55/reversal":
			// Already reversed by a prior, crashed attempt.
			ref := int64(55)
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(alreadyReversedResponse{
				ExistingReversal: &entryResponse{ID: 56, IdempotencyKey: "ledger:reverse:dispatcher:enter_dispatching:2", ReversalOf: &ref, Outcome: "created"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/order-2/transitions":
			_ = json.NewEncoder(w).Encode(orderResponse{ID: 2, ExternalID: "order-2", State: "held", Version: 5,
				AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	c := New(ts.URL, "test-token")
	reporter := NewTwoCallReporter(c)
	order := Order{ID: 2, ExternalID: "order-2", Version: 4}

	err := reporter.ReportDispatchFailure(t.Context(), order, "dispatcher:enter_dispatching:2", "reason")
	if err != nil {
		t.Fatalf("ReportDispatchFailure (retry after crash): %v", err)
	}
}

func TestTwoCallReporter_VersionConflictRefetchesAndRetriesOnce(t *testing.T) {
	transitionCalls := 0
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/entries":
			_ = json.NewEncoder(w).Encode(entryResponse{ID: 55, IdempotencyKey: "dispatcher:enter_dispatching:3", Outcome: "created"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/entries/55/reversal":
			ref := int64(55)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(entryResponse{ID: 56, IdempotencyKey: "ledger:reverse:dispatcher:enter_dispatching:3", ReversalOf: &ref, Outcome: "created"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/orders/order-3":
			_ = json.NewEncoder(w).Encode(orderResponse{ID: 3, ExternalID: "order-3", Version: 9,
				AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/order-3/transitions":
			transitionCalls++
			if transitionCalls == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "version_conflict", Message: "stale version"}})
				return
			}
			_ = json.NewEncoder(w).Encode(orderResponse{ID: 3, ExternalID: "order-3", State: "held", Version: 10,
				AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	c := New(ts.URL, "test-token")
	reporter := NewTwoCallReporter(c)
	order := Order{ID: 3, ExternalID: "order-3", Version: 4} // stale on purpose

	err := reporter.ReportDispatchFailure(t.Context(), order, "dispatcher:enter_dispatching:3", "reason")
	if err != nil {
		t.Fatalf("ReportDispatchFailure: %v", err)
	}
	if transitionCalls != 2 {
		t.Fatalf("transition calls = %d, want 2 (one conflict, one retry)", transitionCalls)
	}
}

// TestAtomicReporter_AgainstProposedEndpoint tests AtomicReporter
// against a fake server built to "Read this third"'s own proposed
// shape -- not a real C1, which does not have this endpoint yet.
func TestAtomicReporter_AgainstProposedEndpoint(t *testing.T) {
	var gotBody postDispatchFailureRequest
	var gotIdemKey string
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders/order-4/dispatch-failure" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotIdemKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(orderResponse{ID: 4, ExternalID: "order-4", State: "held", Version: 5,
			AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
	})

	c := New(ts.URL, "test-token")
	reporter := NewAtomicReporter(c)
	order := Order{ID: 4, ExternalID: "order-4", Version: 4}

	err := reporter.ReportDispatchFailure(t.Context(), order, "dispatcher:enter_dispatching:4", "energy reservation failed")
	if err != nil {
		t.Fatalf("ReportDispatchFailure: %v", err)
	}
	if gotBody.ConversionEntryIdempotencyKey != "dispatcher:enter_dispatching:4" || gotBody.Reason != "energy reservation failed" {
		t.Fatalf("request body = %+v", gotBody)
	}
	if gotIdemKey == "" {
		t.Fatal("Idempotency-Key header was not sent")
	}
}
