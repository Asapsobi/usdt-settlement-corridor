package httpapi

import (
	"net/http"
	"time"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

type postAccountRequest struct {
	Code  string `json:"code"`
	Type  string `json:"type"`
	Asset string `json:"asset"`
}

type accountResponse struct {
	ID         int64     `json:"id"`
	Code       string    `json:"code"`
	Type       string    `json:"type"`
	Asset      string    `json:"asset"`
	NormalSide int16     `json:"normal_side"`
	OpenedAt   time.Time `json:"opened_at"`
}

func toAccountResponse(a accounts.Account) accountResponse {
	return accountResponse{
		ID: a.ID, Code: a.Code, Type: string(a.Type), Asset: string(a.Asset),
		NormalSide: a.NormalSide, OpenedAt: a.OpenedAt,
	}
}

// postAccount is POST /v1/accounts -- the one write route in this API
// that is idempotent on a caller-supplied natural key (the account code)
// rather than the Idempotency-Key header (still required by the blanket
// "no header, no write" rule, but not threaded anywhere: accounts.Create
// is already idempotent on code by itself, per C1.1, and does not detect
// or report a mismatch against what a retried or racing call asked for --
// this handler inherits that behavior unchanged rather than adding a
// stricter check C1.1 itself deliberately does not have).
//
// Exists because no component other than this one may import
// internal/accounts directly (per this package's own doc comment), and
// every write-path account this system needs beyond the fixed chart
// internal/accounts.Seed creates up front -- a payout slot, a customer's
// liability account in a given asset -- is created on demand by whichever
// remote caller first needs to reference it in an entry. Before this
// chunk, nothing served that need over HTTP at all (see
// cmd/seed-console's own doc comment, written when that gap was still
// open).
func (s *Server) postAccount(w http.ResponseWriter, r *http.Request) {
	var req postAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	acc, err := accounts.Create(r.Context(), s.Pool, req.Code, accounts.Type(req.Type), money.Asset(req.Asset))
	if err != nil {
		writeErr(w, err)
		return
	}

	respondJSON(w, http.StatusCreated, toAccountResponse(acc))
}
