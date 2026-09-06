// Package httpapi exposes C3 as a service. This chunk (C3.0) wires only
// /healthz -- the business endpoints (verdicts, hold queue, manual
// review) land in later chunks once there is something for them to
// expose.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"screening/internal/db"
)

// Server holds everything a handler needs.
type Server struct {
	Pool      *db.Pool
	BuildInfo func() (version, commit string)
}

// NewRouter builds the route table.
func NewRouter(s *Server) http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", s.healthzHandler)
	return router
}
