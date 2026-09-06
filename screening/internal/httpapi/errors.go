package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"screening/internal/cache"
	"screening/internal/holds"
	"screening/internal/ledgerclient"
	"screening/internal/rescreen"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string (never reused for a different
// condition), and a human-readable message that may change freely.
// Same discipline as C1.8's and C2.9's own apiError.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

var (
	errInvalidRequest = newAPIError(http.StatusBadRequest, "invalid_request", "the request could not be validated")
	errUnauthorized   = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
	errInternal       = newAPIError(http.StatusInternalServerError, "internal", "internal error")

	errHoldNotFound            = newAPIError(http.StatusNotFound, "hold_not_found", "no hold exists with that id")
	errHoldNotOpen             = newAPIError(http.StatusConflict, "hold_not_open", "that hold has already been resolved")
	errEmptyReviewer           = newAPIError(http.StatusBadRequest, "invalid_request", "reviewer must not be empty")
	errScreeningResultNotFound = newAPIError(http.StatusNotFound, "screening_result_not_found", "no screening result exists with that id")
	errRescreenFlagNotFound    = newAPIError(http.StatusNotFound, "rescreen_flag_not_found", "no rescreen flag exists with that id")

	// errRefundEntryNotImplemented maps holds.ErrRefundEntryNotImplemented
	// -- the EXPECTED result of calling POST /holds/{id}/reject today,
	// per C3.6's own build spec: nothing yet owns constructing the
	// physical BEP20 refund. 501, not 500: this is a known, permanent
	// limitation of the current deployment, not a transient failure.
	errRefundEntryNotImplemented = newAPIError(http.StatusNotImplemented, "refund_entry_not_implemented",
		"reject is not yet implemented: no owner exists for constructing the physical BEP20 refund entry")

	// errIllegalTransition/errUnexpectedHalt/errSystemHalted map the
	// same three ledgerclient sentinels release/reject can surface --
	// see ledgerclient.ErrIllegalTransition's and ErrUnexpectedHalt's
	// own doc comments for exactly when each fires.
	errIllegalTransition = newAPIError(http.StatusConflict, "illegal_transition", "the order already left the state this action requires")
	errUnexpectedHalt    = newAPIError(http.StatusInternalServerError, "unexpected_halt", "C1 reported an unexpected system halt for this transition")
	errSystemHalted      = newAPIError(http.StatusLocked, "system_halted", "the ledger is halted; this action is deferred until it clears")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see, same convention as C1.8/C2.9's own
// mapError: more specific sentinels checked before more general ones.
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, holds.ErrEmptyReviewer):
		return errEmptyReviewer
	case errors.Is(err, holds.ErrNotOpen):
		return errHoldNotOpen
	case errors.Is(err, holds.ErrNotFound):
		return errHoldNotFound
	case errors.Is(err, holds.ErrRefundEntryNotImplemented):
		return errRefundEntryNotImplemented

	case errors.Is(err, rescreen.ErrNotFound):
		return errRescreenFlagNotFound
	case errors.Is(err, rescreen.ErrEmptyResolution), errors.Is(err, rescreen.ErrEmptyActor):
		return errInvalidRequest
	case errors.Is(err, cache.ErrEmptyReason), errors.Is(err, cache.ErrEmptyActor):
		return errInvalidRequest

	case errors.Is(err, ledgerclient.ErrIllegalTransition):
		return errIllegalTransition
	case errors.Is(err, ledgerclient.ErrUnexpectedHalt):
		return errUnexpectedHalt

	default:
		var apiErr *ledgerclient.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "system_halted" {
			return errSystemHalted
		}
		return errInternal
	}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: e.Code, Message: e.Message}})
}

// writeErr maps err through mapError and writes it -- the one place
// every handler's error path funnels through.
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, mapError(err))
}
