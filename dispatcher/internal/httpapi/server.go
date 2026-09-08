// Package httpapi exposes C5 as a service, mirroring C1.8's, C2.9's,
// C3.8's, and C4.8's shape and discipline: service-to-service bearer
// auth, a stable error code per condition, an OpenAPI spec every route
// must appear in.
//
// POST /dispatch does only the synchronous, already-idempotent-and-cheap
// part of a dispatch (slot selection under caps, EnterDispatching's own
// E2 conversion entry) and returns as soon as the order is committed to
// `dispatching` -- it does NOT block on construction, signing,
// broadcast, or finality, which can legitimately take minutes and have
// no place inside one HTTP request/response cycle. Carrying an order the
// rest of the way (BuildTransfer -> Broadcast -> ConfirmFinality) is a
// background worker's job; wiring that worker loop into cmd/dispatchd is
// its own task, same as cmd/brokerd's own production wiring was built
// separately from C4's HTTP boundary.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/slots"
)

// Server holds everything a handler needs.
type Server struct {
	Pool       *db.Pool
	Auth       AuthConfig
	Dispatcher *dispatch.Dispatcher
	Slots      *slots.Store
	// Ledger is the same concrete client Dispatcher.Ledger wraps,
	// held here too because slot selection needs GetAccountBalance
	// (slots.BalanceReader), a method dispatch.LedgerClient's own
	// narrower interface doesn't expose.
	Ledger    *ledgerclient.Client
	SlotCaps  slots.Caps
	Metrics   *Metrics
	BuildInfo func() (version, commit string)
}

// NewRouter builds the full route table. /healthz, /readyz, and
// /metrics are deliberately unauthenticated -- operational endpoints
// scraped by infrastructure, not part of the service-to-service business
// API AUTH governs, same posture as every prior component.
func NewRouter(s *Server) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if s.Metrics == nil {
		s.Metrics = NewMetrics(registry)
	} else {
		registry.MustRegister(s.Metrics.DispatchesTotal, s.Metrics.BroadcastAttemptsTotal, s.Metrics.DuplicateBroadcastPreventedTotal)
	}
	for _, c := range newReactiveGaugeCollectors(s) {
		registry.MustRegister(c)
	}

	router := chi.NewRouter()
	router.Use(corsMiddleware)

	router.Get("/healthz", s.healthzHandler)
	router.Get("/readyz", s.readyzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(s.Auth))

		r.Post("/dispatch", s.postDispatch)
		r.Get("/dispatch/{order_id}", s.getDispatch)

		r.Get("/slots", s.getSlots)
		r.Post("/slots/{id}/retire", s.postRetireSlot)

		r.Get("/system/invariants", s.getSystemInvariants)
	})

	return router
}
