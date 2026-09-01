package httpapi

import (
	"net/http"
	"strconv"

	"ledger/internal/accounts"
	"ledger/internal/journal"
	"ledger/internal/money"
)

type balanceResponse struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Balance     string `json:"balance"`
}

func toBalanceResponse(code string, amt money.Amount) (balanceResponse, error) {
	formatted, err := money.Format(amt)
	if err != nil {
		return balanceResponse{}, err
	}
	return balanceResponse{AccountCode: code, Asset: string(amt.Asset), Balance: formatted}, nil
}

// getAccountBalance is GET /v1/accounts/{code}/balance, optionally
// ?as_of_entry_id=N for BalanceAsOf instead of the current cached value.
func (s *Server) getAccountBalance(w http.ResponseWriter, r *http.Request) {
	code, ok := urlParam(w, r, "code")
	if !ok {
		return
	}

	var amt money.Amount
	var err error
	if raw := r.URL.Query().Get("as_of_entry_id"); raw != "" {
		entryID, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "as_of_entry_id must be an integer"))
			return
		}
		amt, err = journal.BalanceAsOf(r.Context(), s.Pool, code, entryID)
	} else {
		amt, err = journal.Balance(r.Context(), s.Pool, code)
	}
	if err != nil {
		writeErr(w, err)
		return
	}

	resp, err := toBalanceResponse(code, amt)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

// getBalances is GET /v1/balances?prefix=... -- e.g.
// asset:tron:slot: to check every payout slot's balance against its cap
// in one call. N+1 queries (one List, one Balance per matched account)
// rather than a single joined query: at MVP scale (six slots, a handful
// of deposit/position accounts touched per request) that cost is
// negligible, and it reuses accounts.List and journal.Balance exactly as
// built rather than introducing a new query path duplicating both.
func (s *Server) getBalances(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	matched, err := accounts.List(r.Context(), s.Pool, accounts.ListFilter{CodePrefix: prefix})
	if err != nil {
		writeErr(w, err)
		return
	}

	out := make([]balanceResponse, 0, len(matched))
	for _, acc := range matched {
		amt, err := journal.Balance(r.Context(), s.Pool, acc.Code)
		if err != nil {
			writeErr(w, err)
			return
		}
		resp, err := toBalanceResponse(acc.Code, amt)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, resp)
	}
	respondJSON(w, http.StatusOK, map[string]any{"balances": out})
}

// getTrialBalance is GET /v1/trial-balance: the per-asset sum across
// every journal_lines row, independent of the account_balances cache --
// see journal.TrialBalance's own doc comment for why that independence
// is what makes this the strongest health signal in the system.
func (s *Server) getTrialBalance(w http.ResponseWriter, r *http.Request) {
	trial, err := journal.TrialBalance(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}

	out := make(map[string]string, len(trial))
	for asset, units := range trial {
		formatted, err := money.Format(money.Amount{Asset: asset, Units: units})
		if err != nil {
			writeErr(w, err)
			return
		}
		out[string(asset)] = formatted
	}
	respondJSON(w, http.StatusOK, out)
}
