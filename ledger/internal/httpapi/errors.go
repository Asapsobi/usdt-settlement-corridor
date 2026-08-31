package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
	"ledger/internal/recon"
)

// apiError is the one shape every error response takes: a stable status,
// a stable code string (never reused for a different condition, per the
// build spec), and a human-readable message that may change freely.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

// The nine codes below are exactly the build spec's table, verbatim.
// invalid_request, invalid_entry, and unauthorized are documented
// extensions this chunk adds for conditions the table doesn't name but
// that a real HTTP boundary cannot avoid having some stable code for:
// malformed requests, entry-shape problems that aren't specifically an
// unbalanced sum or an asset mismatch (too few lines, a zero-amount
// line, a transition given an entry when none is allowed or none when
// one is required), and failed authentication. Every one of these is
// listed in docs/errors.md alongside the spec's own nine.
var (
	errIdempotencyConflict = newAPIError(http.StatusConflict, "idempotency_conflict", "idempotency key already used with a different payload")
	errUnbalancedEntry     = newAPIError(http.StatusUnprocessableEntity, "unbalanced_entry", "entry does not balance to zero for at least one asset")
	errAssetMismatch       = newAPIError(http.StatusUnprocessableEntity, "asset_mismatch", "line asset does not match its account's asset")
	errSystemHalted        = newAPIError(http.StatusLocked, "system_halted", "the ledger is halted")
	errIllegalTransition   = newAPIError(http.StatusConflict, "illegal_transition", "no such transition is legal from the order's current state")
	errVersionConflict     = newAPIError(http.StatusConflict, "version_conflict", "expected_version did not match the order's current version")
	errAccountNotFound     = newAPIError(http.StatusNotFound, "account_not_found", "no account exists with that code")
	errOrderNotFound       = newAPIError(http.StatusNotFound, "order_not_found", "no order exists with that id")
	errInvalidAmount       = newAPIError(http.StatusBadRequest, "invalid_amount", "amount must be a decimal string in the asset's minor units, not a JSON number")
	errInternal            = newAPIError(http.StatusInternalServerError, "internal", "internal error")

	errInvalidRequest = newAPIError(http.StatusBadRequest, "invalid_request", "the request could not be validated")
	errInvalidEntry   = newAPIError(http.StatusUnprocessableEntity, "invalid_entry", "the entry's shape is invalid for this operation")
	errUnauthorized   = newAPIError(http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
)

// mapError translates a domain error from any lower layer into the
// stable apiError HTTP callers see. Order matters: more specific
// sentinels are checked before more general ones from the same package
// (e.g. journal.ErrTooFewLines before nothing else in journal claims
// "invalid_entry" as broadly).
func mapError(err error) *apiError {
	switch {
	case errors.Is(err, journal.ErrIdempotencyConflict):
		return errIdempotencyConflict
	case errors.Is(err, journal.ErrUnbalanced):
		return errUnbalancedEntry
	case errors.Is(err, journal.ErrAssetMismatch):
		return errAssetMismatch
	case errors.Is(err, journal.ErrTooFewLines),
		errors.Is(err, journal.ErrTooManyLines),
		errors.Is(err, journal.ErrZeroAmountLine):
		return errInvalidEntry
	case errors.Is(err, journal.ErrInvalidIdempotencyKey):
		return errInvalidRequest

	case errors.Is(err, orders.ErrSystemHalted):
		return errSystemHalted
	case errors.Is(err, orders.ErrIllegalTransition):
		return errIllegalTransition
	case errors.Is(err, orders.ErrVersionConflict):
		return errVersionConflict
	case errors.Is(err, orders.ErrOrderNotFound):
		return errOrderNotFound
	case errors.Is(err, orders.ErrEntryRequired), errors.Is(err, orders.ErrEntryNotAllowed):
		return errInvalidEntry
	case errors.Is(err, orders.ErrInvalidParams):
		return errInvalidRequest

	case errors.Is(err, accounts.ErrAccountNotFound):
		return errAccountNotFound
	case errors.Is(err, accounts.ErrInvalidCode), errors.Is(err, accounts.ErrUnknownAccountType):
		return errInvalidRequest

	case errors.Is(err, money.ErrInvalidDecimal),
		errors.Is(err, money.ErrTooManyDecimals),
		errors.Is(err, money.ErrOverflow),
		errors.Is(err, money.ErrZeroValueAmount),
		errors.Is(err, money.ErrUnknownAsset):
		return errInvalidAmount

	case errors.Is(err, halt.ErrClearRequiresOperator):
		return errInvalidRequest

	case errors.Is(err, recon.ErrInvalidSnapshotParams):
		return errInvalidRequest

	default:
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

// writeErr maps err through mapError and writes it. The one place every
// handler's error path funnels through.
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, mapError(err))
}
