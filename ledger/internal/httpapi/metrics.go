package httpapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"ledger/internal/halt"
	"ledger/internal/recon"
)

// Metrics holds the counters and histograms updated reactively by
// handlers as requests happen. The gauges that reflect current database
// state (halted or not, how many orders sit in each state) are NOT here
// -- they're registered directly as a GaugeFunc/custom Collector in
// NewMetrics, computed fresh on every scrape instead of maintained
// separately, so they can never drift from what the database actually
// says between scrapes.
type Metrics struct {
	EntriesTotal     prometheus.Counter
	EntryErrorsTotal *prometheus.CounterVec
	ReconDriftUnits  prometheus.Histogram
}

// NewMetrics builds and registers every metric the build spec names:
// entries_total, entry_errors_total (by code), halt_state (gauge),
// orders_by_state (gauge), recon_drift_units (histogram), and
// recon_lag_seconds. reconciler may be nil (some callers, like tests,
// don't need a live reconciler); in that case recon_lag_seconds always
// reports 0 rather than panicking.
func NewMetrics(reg prometheus.Registerer, pool *pgxpool.Pool, reconciler *recon.Reconciler) *Metrics {
	m := &Metrics{
		EntriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ledger_entries_total",
			Help: "Total journal entries successfully posted via POST /v1/entries.",
		}),
		EntryErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_entry_errors_total",
			Help: "Total POST /v1/entries failures, by stable error code.",
		}, []string{"code"}),
		ReconDriftUnits: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ledger_recon_drift_units",
			Help:    "Drift (observed_units - ledger_units) observed by each ingested reconciliation snapshot, in minor units.",
			Buckets: []float64{0, 1, 2, 5, 10, 100, 1000, 10000, 100000},
		}),
	}

	reg.MustRegister(
		m.EntriesTotal,
		m.EntryErrorsTotal,
		m.ReconDriftUnits,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ledger_halt_state",
			Help: "1 if the ledger is currently halted, 0 otherwise.",
		}, func() float64 {
			halted, err := halt.IsHalted(context.Background(), pool)
			if err != nil {
				slog.Error("metrics: checking halt state", "error", err)
				return 0
			}
			if halted {
				return 1
			}
			return 0
		}),
		newOrdersByStateCollector(pool),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ledger_recon_lag_seconds",
			Help: "Seconds since the reconciler's self-check ticker last woke up, regardless of whether it found anything to do.",
		}, func() float64 {
			if reconciler == nil {
				return 0
			}
			last := reconciler.LastTick()
			if last.IsZero() {
				return 0
			}
			return time.Since(last).Seconds()
		}),
	)

	return m
}
