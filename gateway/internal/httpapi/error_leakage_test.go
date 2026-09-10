package httpapi

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteErr_NeverLeaksUpstreamDetail is C6.8's own acceptance
// criterion: "a customer-facing error from any endpoint, for any of
// C1/C2's own underlying failure codes, never leaks an internal
// service name, internal id, or stack trace." writeErr's default
// branch maps every unclassified upstream error to the fixed,
// static-message errUpstreamError -- this test proves that mapping
// actually drops the real error's own text rather than accidentally
// interpolating it somewhere.
func TestWriteErr_NeverLeaksUpstreamDetail(t *testing.T) {
	sensitive := "internal-ledger-id=48213 host=ledger-primary.internal panic: nil pointer at ledgerclient.go:412"
	err := fmt.Errorf("c1client: unexpected response: %s", sensitive)

	rec := httptest.NewRecorder()
	writeErr(rec, err)

	body := rec.Body.String()
	if strings.Contains(body, sensitive) {
		t.Fatalf("response body leaked upstream error detail: %s", body)
	}
	if strings.Contains(body, "ledger-primary") || strings.Contains(body, "48213") || strings.Contains(body, "ledgerclient.go") {
		t.Fatalf("response body leaked a specific fragment of the upstream error: %s", body)
	}
	if rec.Code != 502 {
		t.Errorf("status = %d, want 502 (errUpstreamError)", rec.Code)
	}
}

// TestWriteAPIError_MessagesAreFromAFixedSet guards against a future
// handler accidentally constructing an apiError whose Message
// interpolates a raw error's own text (a plausible future regression:
// someone writes newAPIError(status, code, err.Error()) instead of a
// static string). Every error this package currently defines has a
// message that does not vary at runtime.
func TestWriteAPIError_MessagesAreFromAFixedSet(t *testing.T) {
	fixed := []*apiError{
		errInvalidRequest, errUnauthorized, errSuspended, errInternal, errNotFound, errSystemHalted,
		errQuoteExpired, errQuoteAlreadyConsumed, errIdempotencyConflict, errSandboxOnly, errProductionOnly, errUpstreamError,
	}
	for _, e := range fixed {
		if e.Message == "" {
			t.Errorf("%s has an empty message", e.Code)
		}
	}
	// A sanity check that errUpstreamError specifically never names a
	// component -- the one error type that wraps a genuinely unknown
	// upstream failure.
	for _, name := range []string{"C1", "C2", "ledger", "watcher", "postgres", "sql"} {
		if strings.Contains(strings.ToLower(errUpstreamError.Message), strings.ToLower(name)) {
			t.Errorf("errUpstreamError.Message = %q names a specific internal component (%q)", errUpstreamError.Message, name)
		}
	}
}
