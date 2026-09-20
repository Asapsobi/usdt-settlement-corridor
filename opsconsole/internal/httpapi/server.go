// Package httpapi is the ops console's own HTTP boundary: routes plus
// html/template rendering. See docs/03-build/ops-console-build-prompts.md
// for the chunk each file implements.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"opsconsole/internal/auditlog"
	"opsconsole/internal/opclient"
	"opsconsole/internal/session"
)

// Operator is one entry from OC_OPERATORS.
type Operator struct {
	Username    string
	BcryptHash  string
	DisplayName string
}

// Server holds every dependency the console's handlers need: the six
// downstream clients, the session signer, the operator directory, and
// the audit log.
type Server struct {
	Ledger     *opclient.LedgerClient
	Watcher    *opclient.WatcherClient
	Screening  *opclient.ScreeningClient
	Broker     *opclient.BrokerClient
	Dispatcher *opclient.DispatcherClient
	S1         *opclient.S1Client
	Gateway    *opclient.GatewayClient  // may be nil -- OC_GATEWAY_* is optional, unlike every other service
	Proofrun   *opclient.ProofrunClient // may be nil -- OC_PROOFRUN_BASE_URL is optional, same reasoning

	Operators []Operator
	Sessions  *session.Signer
	Audit     *auditlog.Log
	AuditPath string // the file Audit itself writes to -- getAudit's own read view (OC.8) tails this directly

	// StuckOrderMinutes is OC_STUCK_ORDER_MINUTES -- how long a
	// dispatching order may sit with no reservation and no dispatch
	// record before getAlerts' own orphaned-order check flags it.
	// Defaults to 30 if zero.
	StuckOrderMinutes int

	Templates *Templates
	BuildInfo func() (version, commit string)
}

func (s *Server) findOperator(username string) (Operator, bool) {
	for _, op := range s.Operators {
		if op.Username == username {
			return op, true
		}
	}
	return Operator{}, false
}

// NewRouter builds the full route table. /healthz and /metrics are
// unauthenticated, same reasoning as every sibling service. Everything
// else requires a valid session (requireSession, auth.go).
func NewRouter(s *Server) http.Handler {
	if s.Templates == nil {
		s.Templates = MustLoadTemplates()
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	router := chi.NewRouter()
	router.Get("/healthz", s.healthzHandler)
	router.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	router.Get("/login", s.getLogin)
	router.Post("/login", s.postLogin)
	router.Post("/logout", s.postLogout)

	router.Group(func(r chi.Router) {
		r.Use(s.requireSession)

		r.Get("/", s.getHome)
		r.Get("/partial/home", s.getHomePartial)

		r.Get("/orders", s.getOrders)
		r.Get("/orders/{external_id}", s.getOrderDetail)
		r.Get("/alerts", s.getAlerts)
		r.Get("/wallets", s.getWallets)

		r.Get("/ledger/halt", s.getLedgerHalt)
		r.Post("/ledger/halt/set", s.postLedgerHaltSet)
		r.Post("/ledger/halt/clear", s.postLedgerHaltClear)

		r.Get("/watcher/cursor", s.getWatcherCursor)
		r.Post("/watcher/cursor", s.postWatcherCursor)
		r.Get("/watcher/sweep", s.getWatcherSweep)

		r.Get("/broker/reservations", s.getBrokerReservations)
		r.Get("/broker/reservations/{id}/reconcile", s.getBrokerReconcileForm)
		r.Post("/broker/reservations/{id}/reconcile", s.postBrokerReconcile)
		r.Get("/broker/fallback-events", s.getBrokerFallbackEvents)
		r.Post("/broker/fallback-events/{id}/resolve", s.postBrokerFallbackResolve)
		r.Get("/broker/providers", s.getBrokerProviders)
		r.Post("/broker/providers", s.postBrokerProviders)

		r.Get("/screening/holds", s.getScreeningHolds)
		r.Post("/screening/holds/{id}/release", s.postScreeningHoldRelease)
		r.Post("/screening/holds/{id}/reject", s.postScreeningHoldReject)

		r.Get("/dispatcher/slots", s.getDispatcherSlots)
		r.Post("/dispatcher/slots/{id}/retire", s.postDispatcherSlotRetire)

		r.Get("/s1/approvals", s.getS1Approvals)
		r.Post("/s1/approvals/{id}/approve", s.postS1Approve)
		r.Post("/s1/approvals/{id}/reject", s.postS1Reject)

		r.Get("/sandbox/orders", s.getSandboxOrders)

		r.Get("/manual/payout", s.getManualFlow)
		r.Post("/manual/payout", s.postManualFlow)

		r.Get("/audit", s.getAudit)
	})

	return router
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","version":"` + version + `","commit":"` + commit + `"}`))
}
