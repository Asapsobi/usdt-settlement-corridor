package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"screening/internal/cache"
)

type screeningResultResponse struct {
	ID            int64           `json:"id"`
	ProviderName  string          `json:"provider_name"`
	SenderAddress string          `json:"sender_address"`
	RiskScore     float64         `json:"risk_score"`
	Flagged       bool            `json:"flagged"`
	ReasonCodes   []string        `json:"reason_codes"`
	RawResponse   json.RawMessage `json:"raw_response"`
	CheckedAt     time.Time       `json:"checked_at"`
}

func toScreeningResultResponse(h cache.Hit) screeningResultResponse {
	return screeningResultResponse{
		ID: h.ID, ProviderName: h.ProviderName, SenderAddress: h.SenderAddress,
		RiskScore: h.RiskScore, Flagged: h.Flagged, ReasonCodes: h.ReasonCodes,
		RawResponse: h.RawResponse, CheckedAt: h.CheckedAt,
	}
}

// getScreeningResults is GET /v1/screening-results?sender_address=<addr>
// -- an audit lookup, returning the FULL history for that address
// (unfiltered by expiry or invalidation, unlike internal/cache.Get),
// newest first. sender_address is required: this is an audit endpoint
// over a table with no natural upper bound on row count, not a general
// browse-everything listing.
func (s *Server) getScreeningResults(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("sender_address")
	if address == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "sender_address is required"))
		return
	}

	list, err := cache.ListByAddress(r.Context(), s.Pool, address)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]screeningResultResponse, len(list))
	for i, h := range list {
		out[i] = toScreeningResultResponse(h)
	}
	respondJSON(w, http.StatusOK, map[string]any{"screening_results": out})
}

type invalidateScreeningResultRequest struct {
	Reason string `json:"reason"`
	Actor  string `json:"actor"`
}

// postInvalidateScreeningResult is POST /v1/screening-results/{id}/invalidate.
// The path names one specific row, but Invalidate's own semantics
// (internal/cache) operate on the (provider, sender_address) PAIR that
// row belongs to -- every cached result for that pair stops being
// trusted, not just the one row named in the URL. This handler's own
// job is exactly resolving id -> (provider, address) before calling it.
func (s *Server) postInvalidateScreeningResult(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req invalidateScreeningResultRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	result, err := cache.GetByID(r.Context(), s.Pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if result == nil {
		writeAPIError(w, errScreeningResultNotFound)
		return
	}

	if err := cache.Invalidate(r.Context(), s.Pool, result.ProviderName, result.SenderAddress, req.Reason, req.Actor); err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "invalidated"})
}
