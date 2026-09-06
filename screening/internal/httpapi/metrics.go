package httpapi

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"screening/internal/db"
	"screening/internal/discovery"
	"screening/internal/pipeline"
	"screening/internal/rescreen"
	"screening/internal/verdict"
)

// Compile-time checks that Metrics actually satisfies every package's
// hook interface -- a signature drift in any of them would otherwise
// only surface at cmd/screend's wiring site, far from here.
var (
	_ pipeline.MetricsRecorder = (*Metrics)(nil)
	_ rescreen.MetricsRecorder = (*Metrics)(nil)
)

// Metrics holds the counters updated reactively as internal/pipeline
// and internal/rescreen do their work, plus holds_released_total /
// holds_rejected_total (incremented directly by this package's own
// handlers, since Release/Reject are only ever called from here in this
// chunk). queue_depth and queue_oldest_pending_age_seconds are NOT
// counters here: both are computed fresh from current state on every
// scrape (GaugeFunc, registered directly in NewMetrics) rather than
// incremented reactively, so they can never drift from what
// screening_queue actually says between scrapes -- same reasoning C1's
// own halt_state/orders_by_state gauges and C2's own cursor_lag_blocks
// gauge use.
type Metrics struct {
	VerdictsTotal                   *prometheus.CounterVec
	HoldsOpenedTotal                prometheus.Counter
	HoldsReleasedTotal              prometheus.Counter
	HoldsRejectedTotal              prometheus.Counter
	RescreenFlagsTotal              prometheus.Counter
	ScreeningVendorUnavailableTotal prometheus.Counter
}

// VendorUnavailable implements pipeline.MetricsRecorder.
func (m *Metrics) VendorUnavailable() {
	if m == nil {
		return
	}
	m.ScreeningVendorUnavailableTotal.Inc()
}

// VerdictReported implements pipeline.MetricsRecorder.
func (m *Metrics) VerdictReported(classification verdict.Classification) {
	if m == nil {
		return
	}
	m.VerdictsTotal.WithLabelValues(classification.String()).Inc()
}

// HoldOpened implements pipeline.MetricsRecorder.
func (m *Metrics) HoldOpened() {
	if m == nil {
		return
	}
	m.HoldsOpenedTotal.Inc()
}

// FlagRecorded implements rescreen.MetricsRecorder.
func (m *Metrics) FlagRecorded() {
	if m == nil {
		return
	}
	m.RescreenFlagsTotal.Inc()
}

// HoldReleased and HoldRejected are called directly by this package's
// own holds_handlers.go after a successful Release/Reject -- there is
// no MetricsRecorder interface for these in internal/holds itself,
// since (in this chunk) Release/Reject are only ever invoked from here;
// threading an optional Metrics dependency through internal/holds's own
// signatures for a single call site each would be dependency-injection
// for its own sake.
func (m *Metrics) HoldReleased() {
	if m == nil {
		return
	}
	m.HoldsReleasedTotal.Inc()
}

func (m *Metrics) HoldRejected() {
	if m == nil {
		return
	}
	m.HoldsRejectedTotal.Inc()
}

// NewMetrics builds and registers the counter half of C3.8's own build
// spec: verdicts_total{classification}, holds_opened_total,
// holds_released_total, holds_rejected_total, rescreen_flags_total, and
// screening_vendor_unavailable_total. The gauge half (queue_depth,
// queue_oldest_pending_age_seconds) is registered separately by
// newQueueGaugeCollectors, unconditionally, in NewRouter -- not bundled
// in here -- because those need Server.Pool regardless of whether the
// caller supplies its own Metrics (a test injecting a fake one to
// assert on counters, concretely): bundling the gauges inside NewMetrics
// would silently drop them whenever a caller does that, the same
// asymmetry C2's own NewMetrics/newCursorLagCollector split avoids.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		VerdictsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "screening_verdicts_total",
			Help: "Total verdicts successfully reported to C1, by classification (pass, hold).",
		}, []string{"classification"}),
		HoldsOpenedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "screening_holds_opened_total",
			Help: "Total holds opened by the automatic pipeline for a Hold classification.",
		}),
		HoldsReleasedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "screening_holds_released_total",
			Help: "Total holds resolved as RELEASED via manual review.",
		}),
		HoldsRejectedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "screening_holds_rejected_total",
			Help: "Total holds resolved as REJECTED via manual review.",
		}),
		RescreenFlagsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "screening_rescreen_flags_total",
			Help: "Total rescreen_flags rows recorded by C3.7's re-screen job.",
		}),
		ScreeningVendorUnavailableTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "screening_vendor_unavailable_total",
			Help: "Total exhausted-retries events against the screening provider, regardless of OutagePolicy.",
		}),
	}

	reg.MustRegister(
		m.VerdictsTotal,
		m.HoldsOpenedTotal,
		m.HoldsReleasedTotal,
		m.HoldsRejectedTotal,
		m.RescreenFlagsTotal,
		m.ScreeningVendorUnavailableTotal,
	)
	return m
}

// newQueueGaugeCollectors builds the reactive queue_depth{status} and
// queue_oldest_pending_age_seconds gauges -- always fresh from
// screening_queue at scrape time (a GaugeFunc), never a value
// incremented in Go code that could drift from what the table actually
// says. pool may be nil in tests that don't need them; each then simply
// reports 0 rather than panicking.
func newQueueGaugeCollectors(pool *db.Pool) []prometheus.Collector {
	queueDepth := func(status discovery.Status) func() float64 {
		return func() float64 {
			if pool == nil {
				return 0
			}
			stats, err := discovery.Stats(context.Background(), pool)
			if err != nil {
				return 0
			}
			return float64(stats.DepthByStatus[status])
		}
	}

	return []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "screening_queue_depth",
			Help:        "Current screening_queue row count, by status.",
			ConstLabels: prometheus.Labels{"status": "PENDING"},
		}, queueDepth(discovery.Pending)),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "screening_queue_depth",
			Help:        "Current screening_queue row count, by status.",
			ConstLabels: prometheus.Labels{"status": "SCREENING"},
		}, queueDepth(discovery.Screening)),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "screening_queue_depth",
			Help:        "Current screening_queue row count, by status.",
			ConstLabels: prometheus.Labels{"status": "DONE"},
		}, queueDepth(discovery.Done)),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "screening_queue_oldest_pending_age_seconds",
			Help: "Age of the longest-waiting PENDING screening_queue row, in seconds; 0 if none are pending.",
		}, func() float64 {
			if pool == nil {
				return 0
			}
			stats, err := discovery.Stats(context.Background(), pool)
			if err != nil || stats.OldestPendingEnqueuedAt == nil {
				return 0
			}
			return time.Since(*stats.OldestPendingEnqueuedAt).Seconds()
		}),
	}
}
