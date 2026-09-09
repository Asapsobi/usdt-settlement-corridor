// Package httpapi is the proof run driver's own tiny HTTP surface: two
// routes (docs/03-build/mvp-proof-run-plan.md's own §2(b)), no auth, no
// rate limiting, no idempotency-key middleware -- this is a supervised
// proof-run tool, not a customer-facing service. See internal/driver's
// own doc comment for what's deliberately not built here.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"proofrun/internal/driver"
)

// Server holds this driver's one real dependency.
type Server struct {
	Driver *driver.Driver
}

// NewRouter builds the full route table.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.healthz)
	r.Route("/v1", func(r chi.Router) {
		r.Post("/payouts", s.postPayout)
		r.Get("/payouts/{external_id}", s.getPayout)
	})
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type apiError struct {
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	slog.Error("proofrun: request failed", "status", status, "error", err)
	respondJSON(w, status, apiError{Message: err.Error()})
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
