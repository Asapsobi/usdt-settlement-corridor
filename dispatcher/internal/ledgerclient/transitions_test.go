package ledgerclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"dispatcher/internal/money"
)

func TestTransitionWithEntry_PostsInlineEntryAndReturnsOrder(t *testing.T) {
	var gotBody postTransitionRequest
	var gotIdemKey string
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders/order-1/transitions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotIdemKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(orderResponse{ID: 1, ExternalID: "order-1", State: "dispatching", Version: 4,
			AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
	})

	c := New(ts.URL, "test-token")
	amt, err := money.ParseDecimal("100.000000")
	if err != nil {
		t.Fatalf("ParseDecimal: %v", err)
	}
	lines := []EntryLine{
		{AccountCode: "liability:customer:cust-1", Asset: "USDT_BEP20", Amount: amt},
		{AccountCode: "asset:tron:slot:1", Asset: "USDT_TRC20", Amount: amt},
	}
	order, err := c.TransitionWithEntry(t.Context(), "order-1", "dispatching", 3, "dispatch attempt", "conversion", time.Now(), lines, "dispatcher:enter-dispatching:order-1:3")
	if err != nil {
		t.Fatalf("TransitionWithEntry: %v", err)
	}
	if order.State != "dispatching" || order.Version != 4 {
		t.Fatalf("order = %+v, want State=dispatching Version=4", order)
	}
	if gotIdemKey != "dispatcher:enter-dispatching:order-1:3" {
		t.Fatalf("Idempotency-Key header = %q", gotIdemKey)
	}
	if gotBody.ToState != "dispatching" || gotBody.ExpectedVersion != 3 {
		t.Fatalf("request body = %+v, want ToState=dispatching ExpectedVersion=3", gotBody)
	}
	if gotBody.Entry == nil || gotBody.Entry.EntryType != "conversion" || len(gotBody.Entry.Lines) != 2 {
		t.Fatalf("request body Entry = %+v, want inline conversion entry with 2 lines", gotBody.Entry)
	}
	if gotBody.EntryID != nil {
		t.Fatalf("request body EntryID = %v, want nil for an inline-entry transition", gotBody.EntryID)
	}
}

func TestTransitionWithEntry_VersionConflictClassifies(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "version_conflict", Message: "expected_version did not match"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.TransitionWithEntry(t.Context(), "order-1", "dispatching", 2, "reason", "conversion", time.Now(), nil, "idem-1")
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("TransitionWithEntry error = %v, want ErrVersionConflict", err)
	}
}

func TestTransitionWithEntry_SystemHaltedClassifies(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "system_halted", Message: "the ledger is halted"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.TransitionWithEntry(t.Context(), "order-1", "dispatching", 2, "reason", "conversion", time.Now(), nil, "idem-1")
	if !errors.Is(err, ErrSystemHalted) {
		t.Fatalf("TransitionWithEntry error = %v, want ErrSystemHalted", err)
	}
}

func TestTransitionWithEntryID_NamesAnExistingEntryNotAnInlineOne(t *testing.T) {
	var gotBody postTransitionRequest
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(orderResponse{ID: 1, ExternalID: "order-1", State: "held", Version: 5,
			AmountIn: "1.000000", AmountOut: "1.000000", FeeUnits: "0.000000", NetworkFeeUnits: "0.000000"})
	})

	c := New(ts.URL, "test-token")
	order, err := c.TransitionWithEntryID(t.Context(), "order-1", "held", 4, "dispatch failed", 999, time.Now(), "idem-2")
	if err != nil {
		t.Fatalf("TransitionWithEntryID: %v", err)
	}
	if order.State != "held" {
		t.Fatalf("order.State = %q, want held", order.State)
	}
	if gotBody.Entry != nil {
		t.Fatalf("request body Entry = %+v, want nil for an entry-id transition", gotBody.Entry)
	}
	if gotBody.EntryID == nil || *gotBody.EntryID != 999 {
		t.Fatalf("request body EntryID = %v, want 999", gotBody.EntryID)
	}
}

func TestGetEntryByIdempotencyKey_SendsKeyAsQueryParam(t *testing.T) {
	var gotQuery string
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("idempotency_key")
		_ = json.NewEncoder(w).Encode(entryResponse{ID: 55, IdempotencyKey: "conv-key-1", Outcome: "created"})
	})

	c := New(ts.URL, "test-token")
	entry, err := c.GetEntryByIdempotencyKey(t.Context(), "conv-key-1")
	if err != nil {
		t.Fatalf("GetEntryByIdempotencyKey: %v", err)
	}
	if gotQuery != "conv-key-1" {
		t.Fatalf("idempotency_key query param = %q, want conv-key-1", gotQuery)
	}
	if entry.ID != 55 {
		t.Fatalf("entry.ID = %d, want 55", entry.ID)
	}
}

func TestGetEntryByIdempotencyKey_NotFoundClassifies(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "entry_not_found", Message: "no journal entry exists"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.GetEntryByIdempotencyKey(t.Context(), "missing-key")
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("GetEntryByIdempotencyKey error = %v, want ErrEntryNotFound", err)
	}
}

func TestPostReversal_FreshReversalReturns201(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/entries/55/reversal" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		ref := int64(55)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(entryResponse{ID: 56, IdempotencyKey: "ledger:reverse:conv-key-1", ReversalOf: &ref, Outcome: "created"})
	})

	c := New(ts.URL, "test-token")
	reversal, alreadyExisted, err := c.PostReversal(t.Context(), 55, "dispatch failed", time.Now())
	if err != nil {
		t.Fatalf("PostReversal: %v", err)
	}
	if alreadyExisted {
		t.Fatal("PostReversal alreadyExisted = true, want false for a fresh reversal")
	}
	if reversal.ID != 56 || reversal.ReversalOf == nil || *reversal.ReversalOf != 55 {
		t.Fatalf("reversal = %+v, want ID=56 ReversalOf=55", reversal)
	}
}

// TestPostReversal_AlreadyReversedReturnsTheExistingReversal is the specific
// behavior C5.7's crash-recovery / retry path depends on: C1's 409 response
// embeds the reversal that already exists, so a caller that retries after a
// crash between "post the reversal" and "record that locally" discovers the
// same outcome without a separate pre-check GET.
func TestPostReversal_AlreadyReversedReturnsTheExistingReversal(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		ref := int64(55)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(alreadyReversedResponse{
			ExistingReversal: &entryResponse{ID: 56, IdempotencyKey: "ledger:reverse:conv-key-1", ReversalOf: &ref, Outcome: "created"},
		})
	})

	c := New(ts.URL, "test-token")
	reversal, alreadyExisted, err := c.PostReversal(t.Context(), 55, "dispatch failed", time.Now())
	if err != nil {
		t.Fatalf("PostReversal: %v", err)
	}
	if !alreadyExisted {
		t.Fatal("PostReversal alreadyExisted = false, want true")
	}
	if reversal.ID != 56 {
		t.Fatalf("reversal.ID = %d, want 56 (the existing reversal returned in the 409 body)", reversal.ID)
	}
}

// TestPostReversal_ConflictWithoutExistingReversalBodyIsAnError covers the
// degraded case reversals.go itself documents: the read-back of the
// existing reversal can fail server-side, in which case the 409 body has
// no existing_reversal at all -- this must surface as an error, not be
// silently treated as alreadyExisted with a zero-value Entry.
func TestPostReversal_ConflictWithoutExistingReversalBodyIsAnError(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "already_reversed", Message: "that entry has already been reversed"}})
	})

	c := New(ts.URL, "test-token")
	_, alreadyExisted, err := c.PostReversal(t.Context(), 55, "dispatch failed", time.Now())
	if err == nil {
		t.Fatal("PostReversal: want an error when the 409 body carries no existing_reversal")
	}
	if alreadyExisted {
		t.Fatal("PostReversal alreadyExisted = true, want false alongside a non-nil error")
	}
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("PostReversal error = %v, want it to classify as ErrAlreadyReversed", err)
	}
}

func TestPostReversal_CannotReverseAReversalClassifies(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "cannot_reverse_a_reversal", Message: "that entry is itself a reversal"}})
	})

	c := New(ts.URL, "test-token")
	_, _, err := c.PostReversal(t.Context(), 56, "reason", time.Now())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "cannot_reverse_a_reversal" {
		t.Fatalf("PostReversal error = %v, want *APIError with code cannot_reverse_a_reversal", err)
	}
}
