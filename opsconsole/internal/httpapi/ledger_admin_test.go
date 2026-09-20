package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/opclient"
	"opsconsole/internal/session"
)

// TestLedgerAdminTemplates_RenderReasonField is OC.16's own acceptance
// criterion: "A grep-based test confirms both reversal and
// forced-transition templates render a reason field."
func TestLedgerAdminTemplates_RenderReasonField(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"reversal (ledger_entries)", ledgerEntriesContent},
		{"forced transition (ledger_transition)", ledgerTransitionContent},
	} {
		if !strings.Contains(tc.content, `name="reason"`) {
			t.Errorf("%s template has no name=\"reason\" field", tc.name)
		}
		if !strings.Contains(tc.content, `<input type="text" name="reason" required>`) {
			t.Errorf("%s template's reason field is not marked required (client-side courtesy only)", tc.name)
		}
	}
}

// fakeLedgerServer stands in for C1 in the tests below. It serves the
// safe reads a page render needs (halt state, GetOrder, GetEntry) with
// minimal fixtures, and records whether the DANGEROUS write path
// (.../reversal or .../transitions) was ever called -- OC.16's own
// acceptance bar is "the server check is the one that matters": that
// write must never fire on invalid input, not that no downstream call
// of any kind happens (re-fetching the order/entry to redisplay
// accurate state in the error form is a harmless, unrelated read).
type fakeLedgerServer struct {
	*httptest.Server
	dangerousWriteCalled bool
}

func newFakeLedgerServer(t *testing.T) *fakeLedgerServer {
	t.Helper()
	f := &fakeLedgerServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/system/halt":
			_, _ = w.Write([]byte(`{"halted":false}`))
		case strings.HasSuffix(r.URL.Path, "/reversal") || strings.HasSuffix(r.URL.Path, "/transitions"):
			f.dangerousWriteCalled = true
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/v1/entries/"):
			_, _ = w.Write([]byte(`{"id":42,"idempotency_key":"k","entry_type":"t","actor":"a","occurred_at":"2026-01-01T00:00:00Z","recorded_at":"2026-01-01T00:00:00Z","outcome":"created","lines":[]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/orders/"):
			_, _ = w.Write([]byte(`{"external_id":"order-1","state":"screened","version":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unexpected path in test fake: ` + r.Method + ` ` + r.URL.Path + `"}}`))
		}
	}))
	return f
}

func testServerWithLedger(t *testing.T, baseURL string) *Server {
	t.Helper()
	log, err := auditlog.Open(t.TempDir() + "/audit.jsonl")
	if err != nil {
		t.Fatalf("opening test audit log: %v", err)
	}
	return &Server{
		Ledger:    opclient.NewLedgerClient(baseURL, "test-token"),
		Audit:     log,
		Templates: MustLoadTemplates(),
	}
}

func withTestSession(r *http.Request) *http.Request {
	sess := session.Session{Username: "test-operator", DisplayName: "Test Operator"}
	return r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, sess))
}

func withChiParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TestPostLedgerEntryReversal_EmptyReasonRejectedServerSide is OC.16's
// own acceptance criterion, the reversal half.
func TestPostLedgerEntryReversal_EmptyReasonRejectedServerSide(t *testing.T) {
	fake := newFakeLedgerServer(t)
	defer fake.Close()
	s := testServerWithLedger(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/ledger/entries/42/reversal", strings.NewReader("reason=&confirm_id=42"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTestSession(req)
	req = withChiParam(req, "id", "42")

	w := httptest.NewRecorder()
	s.postLedgerEntryReversal(w, req)

	if fake.dangerousWriteCalled {
		t.Error("C1's real reversal endpoint was called despite an empty reason")
	}
	if !strings.Contains(w.Body.String(), "reason is required") {
		t.Errorf("expected a reason-is-required message in the response, got: %s", w.Body.String())
	}
}

// TestPostLedgerTransition_EmptyReasonRejectedServerSide is OC.16's own
// acceptance criterion, the forced-transition half.
func TestPostLedgerTransition_EmptyReasonRejectedServerSide(t *testing.T) {
	fake := newFakeLedgerServer(t)
	defer fake.Close()
	s := testServerWithLedger(t, fake.URL)

	form := "to_state=held&expected_version=1&reason=&confirm_phrase=" + strings.ReplaceAll(transitionConfirmPhrase, " ", "+")
	req := httptest.NewRequest(http.MethodPost, "/ledger/orders/order-1/transitions", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTestSession(req)
	req = withChiParam(req, "external_id", "order-1")

	w := httptest.NewRecorder()
	s.postLedgerTransition(w, req)

	if fake.dangerousWriteCalled {
		t.Error("C1's real forced-transition endpoint was called despite an empty reason")
	}
	if !strings.Contains(w.Body.String(), "reason is required") {
		t.Errorf("expected a reason-is-required message in the response, got: %s", w.Body.String())
	}
}

// TestPostLedgerTransition_WrongConfirmPhraseRejectedServerSide proves
// the forced-transition route's own, stronger confirmation (a typed
// phrase, not just a non-empty reason) is also enforced server-side
// before any downstream call.
func TestPostLedgerTransition_WrongConfirmPhraseRejectedServerSide(t *testing.T) {
	fake := newFakeLedgerServer(t)
	defer fake.Close()
	s := testServerWithLedger(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/ledger/orders/order-1/transitions", strings.NewReader("to_state=held&expected_version=1&reason=investigating+a+stuck+payout&confirm_phrase=yes"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTestSession(req)
	req = withChiParam(req, "external_id", "order-1")

	w := httptest.NewRecorder()
	s.postLedgerTransition(w, req)

	if fake.dangerousWriteCalled {
		t.Error("C1's real forced-transition endpoint was called despite a wrong confirmation phrase")
	}
	if !strings.Contains(w.Body.String(), "confirmation phrase did not match") {
		t.Errorf("expected a confirmation-phrase-mismatch message in the response, got: %s", w.Body.String())
	}
}

// TestPostLedgerEntryReversal_WrongConfirmIDRejectedServerSide proves
// the typed-id confirmation on reversal is also enforced server-side.
func TestPostLedgerEntryReversal_WrongConfirmIDRejectedServerSide(t *testing.T) {
	fake := newFakeLedgerServer(t)
	defer fake.Close()
	s := testServerWithLedger(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/ledger/entries/42/reversal", strings.NewReader("reason=duplicate+posting&confirm_id=41"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withTestSession(req)
	req = withChiParam(req, "id", "42")

	w := httptest.NewRecorder()
	s.postLedgerEntryReversal(w, req)

	if fake.dangerousWriteCalled {
		t.Error("C1's real reversal endpoint was called despite a confirm_id that didn't match")
	}
	if !strings.Contains(w.Body.String(), "confirmation did not match") {
		t.Errorf("expected a confirmation-did-not-match message in the response, got: %s", w.Body.String())
	}
}
