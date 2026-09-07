// Package httpapi exposes C4 as a service, mirroring C1.8's, C2.9's, and
// C3.8's shape and discipline: service-to-service bearer auth, a stable
// error code per condition, an OpenAPI spec every route must appear in.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/pricing"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

// Server holds everything a handler needs.
type Server struct {
	Pool         *db.Pool
	Auth         AuthConfig
	Reservations *reservations.Service
	Buffer       *buffer.Buffer
	Router       *routing.Router
	Poller       *pricing.Poller
	Weights      routing.RoutingWeights // echoed back by GET /v1/system/prices, alongside Ceiling
	Ceiling      float64
	Metrics      *Metrics
	BuildInfo    func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated -- operational endpoints
// scraped by infrastructure, not part of the service-to-service business
// API AUTH governs, same posture as C1.8/C2.9/C3.8.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry)
	} else {
		registry.MustRegister(
			s.Metrics.ReservationsTotal, s.Metrics.ReplenishCostTRXTotal,
			s.Metrics.ManualFallbackEventsTotal, s.Metrics.CeilingRejectionsTotal,
		)
	}
	for _, c := range newReactiveGaugeCollectors(s) {
		registry.MustRegister(c)
	}
	if s.Router != nil {
		s.Router.SetMetrics(s.Metrics)
	}
	if s.Buffer != nil {
		s.Buffer.SetMetrics(s.Metrics)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.Post("/reservations", s.postReservation)
		r.Get("/reservations/{id}", s.getReservation)

		r.Get("/buffer", s.getBuffer)

		r.Get("/manual-fallback-events", s.getManualFallbackEvents)
		r.Post("/manual-fallback-events/{id}/resolve", s.postResolveManualFallbackEvent)

		r.Get("/system/prices", s.getSystemPrices)
		r.Get("/system/invariants", s.getSystemInvariants)
	})

	return router
}
