package httpapi

import (
	"net/http"
	"time"

	"depositwatcher/internal/chain"
)

type cursorResponse struct {
	LastScanned          int64     `json:"last_scanned"`
	LastCandidateScanned *int64    `json:"last_candidate_scanned,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// getCursor is GET /v1/system/cursor (ops-console-build-prompts.md's
// OC.4) -- the first route to expose the raw ingestion_cursor row
// directly; every prior operator-facing signal (GET /v1/system/invariants'
// own cursor_lag_blocks, the watcher_cursor_lag_blocks metric) only ever
// derived a lag number from it, never the row itself.
func (s *Server) getCursor(w http.ResponseWriter, r *http.Request) {
	state, err := chain.GetCursorState(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := cursorResponse{LastScanned: int64(state.LastScanned), UpdatedAt: state.UpdatedAt}
	if state.LastCandidateScanned != nil {
		v := int64(*state.LastCandidateScanned)
		resp.LastCandidateScanned = &v
	}
	respondJSON(w, http.StatusOK, resp)
}

type postCursorRequest struct {
	LastScanned          *int64 `json:"last_scanned"`
	LastCandidateScanned *int64 `json:"last_candidate_scanned"`
	Reason               string `json:"reason"`
}

// postCursor is POST /v1/system/cursor -- an operator override, closing
// the gap this project's own two real proof runs hit by hand: fast-
// forwarding last_scanned to near the real chain tip and pointing
// last_candidate_scanned at a specific block, directly against Postgres,
// because no HTTP route existed for it. Validates the same invariant
// internal/candidates/loop.go's own gating already assumes elsewhere
// (candidate scanning is never ahead of ingestion) and refuses to set
// either value into the future relative to the chain's own current tip,
// read fresh at request time -- a stale or wrong operator-supplied value
// is caught here, not three ticks later as an ingestion error.
func (s *Server) postCursor(w http.ResponseWriter, r *http.Request) {
	var req postCursorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LastScanned == nil || req.LastCandidateScanned == nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"last_scanned and last_candidate_scanned are both required"))
		return
	}
	if req.Reason == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "reason is required"))
		return
	}
	if *req.LastScanned < 0 || *req.LastCandidateScanned < 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"last_scanned and last_candidate_scanned must not be negative"))
		return
	}
	if *req.LastCandidateScanned > *req.LastScanned {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"last_candidate_scanned must not be ahead of last_scanned"))
		return
	}

	if s.ChainPool != nil {
		tip, err := s.ChainPool.LatestBlockHeader(r.Context())
		if err != nil {
			writeAPIError(w, newAPIError(http.StatusServiceUnavailable, errSystemComponentNotReady.Code,
				"could not read the current chain tip to validate against: "+err.Error()))
			return
		}
		tipHeight := int64(tip.Number.Uint64())
		if *req.LastScanned > tipHeight {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
				"last_scanned may not be set past the chain's own current tip"))
			return
		}
	}

	if err := chain.SetCursorState(r.Context(), s.Pool, uint64(*req.LastScanned), uint64(*req.LastCandidateScanned)); err != nil {
		writeErr(w, err)
		return
	}
	s.getCursor(w, r)
}
