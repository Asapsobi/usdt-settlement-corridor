package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"depositwatcher/internal/chain"
)

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	respondJSON(w, http.StatusOK, healthzResponse{Status: "ok", Version: version, Commit: commit})
}

// readyzHandler additionally checks the database is actually reachable --
// healthz says the process is up, readyz says it can do its job. Same
// distinction as C1.8's own readyz.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type providerHealthResponse struct {
	Name                string `json:"name"`
	Healthy             bool   `json:"healthy"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	TotalRounds         int    `json:"total_rounds"`
	TotalFailures       int    `json:"total_failures"`
	LastError           string `json:"last_error,omitempty"`
}

// getProviders is GET /v1/system/providers -- per-provider health from
// C2.2's Pool. 503 system_component_not_ready if this instance has no
// ChainPool configured (a deployment concern independent of the HTTP
// boundary itself -- see Server's own doc comment).
func (s *Server) getProviders(w http.ResponseWriter, r *http.Request) {
	if s.ChainPool == nil {
		writeAPIError(w, errSystemComponentNotReady)
		return
	}
	snapshot := s.ChainPool.ProviderHealthSnapshot()
	out := make([]providerHealthResponse, len(snapshot))
	for i, p := range snapshot {
		out[i] = providerHealthResponse{
			Name: p.Name, Healthy: p.Healthy, ConsecutiveFailures: p.ConsecutiveFailures,
			TotalRounds: p.TotalRounds, TotalFailures: p.TotalFailures, LastError: p.LastError,
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"providers": out})
}

type invariantsResponse struct {
	CursorLagBlocks                  *int64   `json:"cursor_lag_blocks,omitempty"`
	PendingFinalityCount             *int     `json:"pending_finality_count,omitempty"`
	OldestPendingCandidateAgeSeconds *float64 `json:"oldest_pending_candidate_age_seconds,omitempty"`
}

// getInvariants is GET /v1/system/invariants: cursor lag, pending-
// finality count, oldest pending candidate age -- the operator-facing
// numbers the build spec names. Each is independently omitted (not a
// 503 for the whole endpoint) if its own source (ChainPool, Tracker)
// isn't configured on this instance, rather than an all-or-nothing
// failure over a partial deployment.
func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	var resp invariantsResponse

	if s.ChainPool != nil {
		cursor, err := chain.LastScannedHeight(r.Context(), s.Pool)
		if err != nil {
			writeErr(w, err)
			return
		}
		tip, err := s.ChainPool.LatestBlockHeader(r.Context())
		if err != nil {
			slog.Error("httpapi: fetching chain tip for cursor lag failed", "error", err)
		} else {
			lag := int64(tip.Number.Uint64()) - int64(cursor)
			resp.CursorLagBlocks = &lag
		}
	}

	if s.Tracker != nil {
		pending := s.Tracker.PendingCount()
		resp.PendingFinalityCount = &pending

		if oldest, found := s.Tracker.OldestPendingDetectedAt(); found {
			age := time.Since(oldest).Seconds()
			resp.OldestPendingCandidateAgeSeconds = &age
		}
	}

	respondJSON(w, http.StatusOK, resp)
}
