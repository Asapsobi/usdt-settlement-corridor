// Package httpapi is C6's own HTTP boundary: the customer-facing
// contract for the whole corridor -- quote, order, status, webhooks,
// and an isolated sandbox. Mirrors every prior component's own
// httpapi package in shape and discipline (service-to-service posture
// swapped for customer-facing API keys here, per C6.1), consistent
// error shapes, an OpenAPI spec every route must appear in (C6.8).
package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gateway/internal/c1client"
	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/db"
	"gateway/internal/orders"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
	"gateway/internal/sandbox"
)

// DefaultQuoteValidity is decision 5's own "90s price lock" default.
const DefaultQuoteValidity = 90 * time.Second

// Server holds everything a handler needs.
type Server struct {
	Pool        *db.Pool
	Customers   *customers.Store
	RateLimiter *ratelimit.Limiter
	Quotes      *quotes.Store
	Orders      *orders.Store
	Ledger      *c1client.Client
	Watcher     *c2client.Client
	Sandbox     *sandbox.Store
	// PendingAddress backs the orders_address_pending gauge (C6.8) --
	// typically the same *reconcile.Reconciler main.go already runs;
	// kept as this narrow interface so this package doesn't need to
	// import internal/reconcile. Nil means the gauge simply isn't
	// collected.
	PendingAddress PendingAddressGauge
	Metrics        *Metrics      // auto-initialized by NewRouter if left nil
	QuoteValidity  time.Duration // DefaultQuoteValidity if zero
	BuildInfo      func() (version, commit string)
}

func (s *Server) quoteValidity() time.Duration {
	if s.QuoteValidity <= 0 {
		return DefaultQuoteValidity
	}
	return s.QuoteValidity
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated -- operational endpoints
// scraped by infrastructure, not part of the customer-facing business
// API auth governs, same posture as every prior component.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry)
	} else {
		registry.MustRegister(s.Metrics.QuotesIssuedTotal, s.Metrics.OrdersCreatedTotal, s.Metrics.WebhookDeliveriesTotal,
			s.Metrics.WebhookRetryExhaustedTotal, s.Metrics.SandboxRequestsTotal)
	}
	for _, c := range newReactiveGaugeCollectors(s) {
		registry.MustRegister(c)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Get("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP)

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Customers, s.RateLimiter))

		r.With(requireProductionCustomer, requireActiveCustomer).Post("/quotes", s.postQuote)
		r.With(requireProductionCustomer, requireActiveCustomer, requireIdempotencyKey).Post("/orders", s.postOrder)
		// A suspended customer can still read order status -- C6.1's own
		// acceptance criterion -- so this route deliberately carries no
		// requireActiveCustomer.
		r.With(requireProductionCustomer).Get("/orders/{external_id}", s.getOrderStatus)

		// C6.7: a fully separate code path -- sandbox API keys only (never
		// interchangeable with production ones, invariant 4).
		r.With(requireSandboxCustomer).Post("/sandbox/orders", s.postSandboxOrder)
		r.With(requireSandboxCustomer).Get("/sandbox/orders/{external_id}", s.getSandboxOrderStatus)
		// C6.8 onward add routes here.
	})

	return router
}
