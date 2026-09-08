package httpapi

import "net/http"

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
// Same distinction as every prior component's own readyz.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
