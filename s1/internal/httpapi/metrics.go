package httpapi

import (
	"github.com/prometheus/client_golang/prometheus"

	"s1/internal/requests"
)

// Metrics holds every Prometheus collector this package reports.
// Constructed once by NewRouter (or injected, for tests) -- optional in
// the sense that a nil *Server.Metrics is filled in automatically, never
// a caller obligation.
type Metrics struct {
	SigningRequestsTotal *prometheus.CounterVec
	ApprovalsTotal       *prometheus.CounterVec
}

// NewMetrics registers and returns a fresh Metrics against registry.
func NewMetrics(registry *prometheus.Registry) *Metrics {
	m := &Metrics{
		SigningRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "s1_signing_requests_total",
			Help: "Total RequestSignature calls, by resulting status.",
		}, []string{"status"}),
		ApprovalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "s1_approvals_total",
			Help: "Total approve/reject decisions recorded.",
		}, []string{"decision"}),
	}
	registry.MustRegister(m.SigningRequestsTotal, m.ApprovalsTotal)
	return m
}

func (m *Metrics) recordSigningRequest(status requests.Status) {
	if m == nil {
		return
	}
	m.SigningRequestsTotal.WithLabelValues(string(status)).Inc()
}

func (m *Metrics) recordApproval(decision string) {
	if m == nil {
		return
	}
	m.ApprovalsTotal.WithLabelValues(decision).Inc()
}
