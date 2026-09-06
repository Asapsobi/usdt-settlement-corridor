package httpapi

import (
	"net/http"
	"time"

	"screening/internal/discovery"
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

// readyzHandler additionally checks the database is actually reachable
// -- healthz says the process is up, readyz says it can do its job.
// Same distinction as C1.8's and C2.9's own readyz.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type queueStatsResponse struct {
	DepthByStatus           map[string]int `json:"depth_by_status"`
	OldestPendingAgeSeconds *float64       `json:"oldest_pending_age_seconds"`
}

// getSystemQueue is GET /v1/system/queue -- queue depth by status, plus
// the oldest PENDING row's age, always computed fresh (never a value
// that could drift from what screening_queue actually says).
func (s *Server) getSystemQueue(w http.ResponseWriter, r *http.Request) {
	stats, err := discovery.Stats(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp := queueStatsResponse{DepthByStatus: make(map[string]int, len(stats.DepthByStatus))}
	for status, count := range stats.DepthByStatus {
		resp.DepthByStatus[string(status)] = count
	}
	if stats.OldestPendingEnqueuedAt != nil {
		age := time.Since(*stats.OldestPendingEnqueuedAt).Seconds()
		resp.OldestPendingAgeSeconds = &age
	}
	respondJSON(w, http.StatusOK, resp)
}
