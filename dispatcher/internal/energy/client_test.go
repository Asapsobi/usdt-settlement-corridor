package energy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeC4Server stands in for a real C4 instance, built to C4's actual
// shipped contract (energybroker/internal/httpapi) -- not a guess.
func fakeC4Server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return ts
}

func TestReserve_SendsIdempotencyKeyAsHeaderNotBodyField(t *testing.T) {
	var gotIdemKey string
	var gotBody map[string]any
	ts := fakeC4Server(t, func(w http.ResponseWriter, r *http.Request) {
		gotIdemKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reservationResponse{
			ID: 1, ExternalID: "order-1", OrderID: 42, TargetAddress: "TSlot1", EnergyUnits: 500,
			Tier: "STANDARD", Status: "CONFIRMED", CreatedAt: time.Now(), Deadline: time.Now().Add(time.Minute),
		})
	})

	c := New(ts.URL, "test-token")
	res, err := c.Reserve(t.Context(), "order-1", "TSlot1", 500, "STANDARD", time.Now().Add(time.Minute), "dispatcher:reserve:order-1:1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if gotIdemKey != "dispatcher:reserve:order-1:1" {
		t.Fatalf("Idempotency-Key header = %q, want the idempotency key", gotIdemKey)
	}
	if _, ok := gotBody["idempotency_key"]; ok {
		t.Fatal("idempotency_key was sent as a body field; C4's real contract expects it as a header only")
	}
	if res.Status != "CONFIRMED" || res.OrderID != 42 {
		t.Fatalf("Reserve result = %+v, want Status=CONFIRMED OrderID=42", res)
	}
}

func TestReserve_NonCreatedStatusIsAnAPIError(t *testing.T) {
	ts := fakeC4Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "invalid_request", Message: "bad tier"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.Reserve(t.Context(), "order-1", "TSlot1", 500, "BOGUS", time.Now().Add(time.Minute), "idem-1")
	var apiErr *APIError
	if err == nil {
		t.Fatal("Reserve: want an error, got nil")
	}
	if !errorsAs(err, &apiErr) {
		t.Fatalf("Reserve error = %v, want *APIError", err)
	}
	if apiErr.Code != "invalid_request" {
		t.Fatalf("APIError.Code = %q, want invalid_request", apiErr.Code)
	}
}

func errorsAs(err error, target **APIError) bool {
	apiErr, ok := err.(*APIError)
	if !ok {
		return false
	}
	*target = apiErr
	return true
}

func TestPoll_ReturnsOnceTerminal(t *testing.T) {
	calls := 0
	ts := fakeC4Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := "PENDING"
		if calls >= 3 {
			status = "CONFIRMED"
		}
		_ = json.NewEncoder(w).Encode(reservationResponse{ID: 1, Status: status, CreatedAt: time.Now(), Deadline: time.Now().Add(time.Minute)})
	})

	c := New(ts.URL, "test-token")
	res, err := c.Poll(t.Context(), 1, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "CONFIRMED" {
		t.Fatalf("Poll result status = %q, want CONFIRMED", res.Status)
	}
	if calls < 3 {
		t.Fatalf("Poll returned after %d calls, want it to have polled until terminal", calls)
	}
}

func TestPoll_TimesOutDistinctFromFailed(t *testing.T) {
	ts := fakeC4Server(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reservationResponse{ID: 1, Status: "PENDING", CreatedAt: time.Now(), Deadline: time.Now().Add(time.Minute)})
	})

	c := New(ts.URL, "test-token")
	_, err := c.Poll(t.Context(), 1, time.Now().Add(600*time.Millisecond))
	if err != ErrReservationTimeout {
		t.Fatalf("Poll error = %v, want ErrReservationTimeout", err)
	}
}
