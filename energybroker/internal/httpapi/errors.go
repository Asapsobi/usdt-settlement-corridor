package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"energybroker/internal/ledgerclient"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

// apiError is the one shape every error response takes: a stable
// status, a stable code string (never reused for a different
// condition), and a human-readable message that may change freely.
// Same discipline as every prior component's own apiError.
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

	errReservationNotFound   = newAPIError(http.StatusNotFound, "reservation_not_found", "no reservation exists with that id")
	errFallbackEventNotFound = newAPIError(http.StatusNotFound, "fallback_event_not_found", "no manual fallback event exists with that id")
	errOrderNotFound         = newAPIError(http.StatusNotFound, "order_not_found", "C1 has no order with that external_id")

	// errUpstreamError covers a non-404 failure reaching C1 through
	// OrderResolver.GetOrder -- surfaced distinctly from errInternal so
	// an operator reading logs/metrics can tell "C1 itself is unhappy"
	// from "this service broke on its own".
	errUpstreamError = newAPIError(http.StatusBadGateway, "upstream_error", "C1 returned an unexpected error resolving this order")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see, same convention as every prior
// component's own mapError: more specific sentinels checked before more
// general ones.
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, reservations.ErrInvalidRequest):
		return errInvalidRequest
	case errors.Is(err, reservations.ErrNotFound):
		return errReservationNotFound

	case errors.Is(err, routing.ErrFallbackEventNotFound):
		return errFallbackEventNotFound
	case errors.Is(err, routing.ErrEmptyResolution), errors.Is(err, routing.ErrEmptyActor):
		return errInvalidRequest

	default:
		var apiErr *ledgerclient.APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusNotFound {
				return errOrderNotFound
			}
			return errUpstreamError
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
