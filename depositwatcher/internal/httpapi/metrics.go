package httpapi

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/orphaned"
)

// Compile-time checks that Metrics actually satisfies both packages'
// hook interfaces -- a signature drift in either package would otherwise
// only surface at cmd/watcherd's wiring site, far from here.
var (
	_ finality.MetricsRecorder     = (*Metrics)(nil)
	_ ledgerclient.MetricsRecorder = (*Metrics)(nil)
)

// Metrics holds the counters updated reactively as finality.Tracker and
// ledgerclient.Client do their work -- Metrics implements both packages'
// small MetricsRecorder interfaces, so wiring it in at cmd/watcherd is
// the only place that needs to know both exist. orphaned_deposits_total
// and provider_agreement_failures_total are NOT here: both are computed
// fresh from current state on every scrape (a GaugeFunc, registered
// directly in NewMetrics) rather than incremented reactively, so they
// can never drift from what the database/Pool actually says between
// scrapes -- same reasoning C1's own halt_state/orders_by_state gauges
// use.
type Metrics struct {
	CandidatesDetectedTotal  prometheus.Counter
	CandidatesFinalizedTotal prometheus.Counter
	ReportsToLedgerTotal     *prometheus.CounterVec
}

// CandidateDetected implements finality.MetricsRecorder.
func (m *Metrics) CandidateDetected() {
	if m == nil {
		return
	}
	m.CandidatesDetectedTotal.Inc()
}

// CandidateFinalized implements finality.MetricsRecorder.
func (m *Metrics) CandidateFinalized() {
	if m == nil {
		return
	}
	m.CandidatesFinalizedTotal.Inc()
}

// ReportedToLedger implements ledgerclient.MetricsRecorder.
func (m *Metrics) ReportedToLedger(resultCode string) {
	if m == nil {
		return
	}
	m.ReportsToLedgerTotal.WithLabelValues(resultCode).Inc()
}

// NewMetrics builds and registers every metric the build spec names:
// candidates_detected_total, candidates_finalized_total,
// reports_to_ledger_total{code}, orphaned_deposits_total,
// provider_agreement_failures_total, and cursor_lag_blocks (registered
// separately in NewRouter, since it needs the full Server). pool and
// chainPool may be nil in tests that don't need the reactive gauges;
// both gauges then simply report 0 rather than panicking.
func NewMetrics(reg prometheus.Registerer, pool *db.Pool, chainPool *chain.Pool) *Metrics {
	m := &Metrics{
		CandidatesDetectedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watcher_candidates_detected_total",
			Help: "Total Transfer logs classified as trackable deposit candidates (deposit.detected).",
		}),
		CandidatesFinalizedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "watcher_candidates_finalized_total",
			Help: "Total candidates that reached finality (the finalized tag passed their height, re-verified).",
		}),
		ReportsToLedgerTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "watcher_reports_to_ledger_total",
			Help: "Total ReportDepositFinal attempts, by C1's response result code (ok, version_conflict, system_halted, illegal_transition, idempotency_conflict, network_error, get_order_failed, ...).",
		}, []string{"code"}),
	}

	reg.MustRegister(
		m.CandidatesDetectedTotal,
		m.CandidatesFinalizedTotal,
		m.ReportsToLedgerTotal,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "watcher_orphaned_deposits_total",
			Help: "Total orphaned_deposits rows (never deleted, so monotonic in practice) -- gap #3's capture-and-surface count.",
		}, func() float64 {
			if pool == nil {
				return 0
			}
			deposits, err := orphaned.List(context.Background(), pool, nil)
			if err != nil {
				return 0
			}
			return float64(len(deposits))
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "watcher_provider_agreement_failures_total",
			Help: "Sum, across all configured RPC providers, of rounds each was on the losing/erroring side of an agreement check (LatestFinalized or LogsAt).",
		}, func() float64 {
			if chainPool == nil {
				return 0
			}
			var total int
			for _, p := range chainPool.ProviderHealthSnapshot() {
				total += p.TotalFailures
			}
			return float64(total)
		}),
	)
	return m
}

// newCursorLagCollector reports how many blocks behind the chain tip the
// ingestion cursor is -- 0 if ChainPool isn't configured on this
// instance, rather than panicking. Registered directly against NewRouter's
// registry (not inside NewMetrics) because, unlike everything else in
// this file, it needs the full Server, not just Pool/ChainPool.
func newCursorLagCollector(s *Server) prometheus.Collector {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "watcher_cursor_lag_blocks",
		Help: "How many blocks behind the chain tip internal/chain's ingestion cursor is.",
	}, func() float64 {
		if s.ChainPool == nil {
			return 0
		}
		ctx := context.Background()
		cursor, err := chain.LastScannedHeight(ctx, s.Pool)
		if err != nil {
			return 0
		}
		tip, err := s.ChainPool.LatestBlockHeader(ctx)
		if err != nil {
			return 0
		}
		return float64(tip.Number.Uint64()) - float64(cursor)
	})
}
