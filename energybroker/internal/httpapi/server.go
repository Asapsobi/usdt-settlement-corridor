// Package httpapi exposes C4 as a service. This chunk (C4.0) wires only
// /healthz -- the reservation contract C5 will call (§A) lands in a
// later chunk once there is a buffer for it to reserve against.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"energybroker/internal/db"
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
