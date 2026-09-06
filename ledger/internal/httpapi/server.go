// Package httpapi exposes C1 as a service. The other five components are
// clients of this HTTP API -- none of them import any other internal/*
// package of this module directly.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"ledger/internal/db"
	"ledger/internal/recon"
)

// Server holds everything a handler needs. Read-only handlers use Pool
// directly (it satisfies every internal package's Queryer interface via
// its embedded *pgxpool.Pool); write handlers that need atomicity across
// more than one call use db.Tx(ctx, s.Pool, ...).
type Server struct {
	Pool       *db.Pool
	Auth       AuthConfig
	ReconCfg   recon.Config
	Reconciler *recon.Reconciler
	Metrics    *Metrics
	BuildInfo  func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and /metrics
// are deliberately unauthenticated -- they're operational endpoints
// scraped by infrastructure (health checkers, Prometheus), not part of
// the service-to-service business API AUTH governs.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry, s.Pool.Pool, s.Reconciler)
	} else {
		registry.MustRegister(s.Metrics.EntriesTotal, s.Metrics.EntryErrorsTotal, s.Metrics.ReconDriftUnits)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.With(requireIdempotencyKey).Post("/entries", s.postEntry)
		r.Get("/entries", s.getEntryByKey)
		r.Get("/entries/{id}", s.getEntry)
		r.With(requireIdempotencyKey).Post("/entries/{id}/reversal", s.postReversal)

		r.With(requireIdempotencyKey).Post("/orders", s.postOrder)
		r.Get("/orders", s.getOrders)
		r.Get("/orders/{external_id}", s.getOrder)
		r.With(requireIdempotencyKey).Post("/orders/{external_id}/transitions", s.postTransition)
		r.With(requireIdempotencyKey).Post("/orders/{external_id}/reorg", s.postReorg)

		r.Get("/accounts/{code}/balance", s.getAccountBalance)
		r.Get("/balances", s.getBalances)
		r.Get("/trial-balance", s.getTrialBalance)

		r.With(requireIdempotencyKey).Post("/reconciliation/snapshots", s.postSnapshot)

		r.Get("/system/halt", s.getHalt)
		r.With(requireIdempotencyKey).Post("/system/halt", s.postHalt)
		r.Get("/system/invariants", s.getInvariants)
	})

	return router
}
