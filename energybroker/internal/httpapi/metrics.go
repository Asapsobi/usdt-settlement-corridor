package httpapi

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"energybroker/internal/buffer"
	"energybroker/internal/money"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

// Compile-time checks that Metrics actually satisfies every package's
// hook interface -- a signature drift in any of them would otherwise
// only surface at cmd/brokerd's wiring site, far from here.
var _ reservations.MetricsRecorder = (*Metrics)(nil)
var _ buffer.MetricsRecorder = (*Metrics)(nil)
var _ routing.MetricsRecorder = (*Metrics)(nil)

// Metrics holds the counters updated reactively as internal/reservations
// does its work, plus a manual_fallback_events_resolved counter this
// package's own resolve handler increments directly. buffer_level and
// buffer_target are NOT counters here: both are computed fresh from
// current state on every scrape (GaugeFunc, registered directly in
// NewRouter) rather than incremented reactively, so they can never
// drift from what energy_buffer actually says between scrapes -- same
// reasoning every prior component's own reactive gauges use.
type Metrics struct {
	ReservationsTotal         *prometheus.CounterVec
	ReplenishCostTRXTotal     *prometheus.CounterVec
	ManualFallbackEventsTotal prometheus.Counter
	CeilingRejectionsTotal    *prometheus.CounterVec
}

// ReservationConfirmed implements reservations.MetricsRecorder.
func (m *Metrics) ReservationConfirmed(viaFastPath bool) {
	if m == nil {
		return
	}
	if viaFastPath {
		m.ReservationsTotal.WithLabelValues("fast_path").Inc()
		return
	}
	m.ReservationsTotal.WithLabelValues("slow_path").Inc()
}

// ReservationFailed implements reservations.MetricsRecorder.
func (m *Metrics) ReservationFailed() {
	if m == nil {
		return
	}
	m.ReservationsTotal.WithLabelValues("failed").Inc()
}

// ManualFallbackEventTriggered is called by this package's own fallback
// wiring (see newReactiveGaugeCollectors's own doc comment on why this
// isn't threaded through routing.MetricsRecorder the way
// reservations.MetricsRecorder is: OnFallbackTriggered is the ONE call
// site in this whole module, unlike ReservationConfirmed/Failed's
// several).
func (m *Metrics) ManualFallbackEventTriggered() {
	if m == nil {
		return
	}
	m.ManualFallbackEventsTotal.Inc()
}

// ManualFallbackEventResolved is called directly by this package's own
// postResolveManualFallbackEvent handler after a successful Resolve.
func (m *Metrics) ManualFallbackEventResolved() {
	if m == nil {
		return
	}
	// Resolutions are not double-counted against ManualFallbackEventsTotal
	// (a "triggered" counter) -- there is no separate resolved counter
	// named in this chunk's own build spec, so this is intentionally a
	// no-op today, kept as the one place a future resolved-count metric
	// would be added without touching the handler that calls it.
}

// ReplenishCost implements buffer.MetricsRecorder, called by
// Buffer.Replenish right after a delegation is successfully recorded
// AVAILABLE. costTRX is in minor units (see internal/money), matching
// broker_replenish_cost_trx_total's own documented unit -- never
// rescaled to whole TRX here, so a dashboard reading this counter
// doesn't need to know internal/money's own scale to interpret it
// correctly.
func (m *Metrics) ReplenishCost(providerName string, costTRX money.Amount) {
	if m == nil {
		return
	}
	m.ReplenishCostTRXTotal.WithLabelValues(providerName).Add(float64(costTRX))
}

// CeilingRejected is called by internal/routing's own SelectProvider
// audit path -- see newReactiveGaugeCollectors's own doc comment for why
// this is wired via a package-level hook rather than a constructor
// parameter thread through routing.Router.
func (m *Metrics) CeilingRejected(providerName string) {
	if m == nil {
		return
	}
	m.CeilingRejectionsTotal.WithLabelValues(providerName).Inc()
}

// NewMetrics builds and registers the counter half of this chunk's own
// build spec: reservations_total{fast_path,slow_path,failed},
// replenish_cost_trx_total{provider}, manual_fallback_events_total,
// ceiling_rejections_total{provider}. The gauge half (buffer_level,
// buffer_target, price_staleness_seconds) is registered separately by
// newReactiveGaugeCollectors, unconditionally, in NewRouter -- not
// bundled in here -- because those need Server's own Buffer/Poller
// regardless of whether the caller supplies its own Metrics (a test
// injecting a fake one to assert on counters, concretely): bundling the
// gauges inside NewMetrics would silently drop them whenever a caller
// does that, the same asymmetry every prior component's own
// NewMetrics/gauge-collector split avoids.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ReservationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_reservations_total",
			Help: "Total reservations resolved, by outcome (fast_path, slow_path, failed).",
		}, []string{"outcome"}),
		ReplenishCostTRXTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_replenish_cost_trx_total",
			Help: "Total TRX (minor units) spent replenishing the buffer, by provider.",
		}, []string{"provider"}),
		ManualFallbackEventsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_manual_fallback_events_total",
			Help: "Total manual_fallback_events rows recorded (C4.6).",
		}),
		CeilingRejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_ceiling_rejections_total",
			Help: "Total ceiling checks that rejected a provider as over ceiling, by provider.",
		}, []string{"provider"}),
	}

	reg.MustRegister(
		m.ReservationsTotal,
		m.ReplenishCostTRXTotal,
		m.ManualFallbackEventsTotal,
		m.CeilingRejectionsTotal,
	)
	return m
}

// newReactiveGaugeCollectors builds buffer_level{provider}, buffer_target,
// and price_staleness_seconds{provider} -- always fresh from the buffer
// and price feed at scrape time (GaugeFunc), never a value incremented
// in Go code that could drift from what those actually say. Any of
// s.Buffer/s.Poller may be nil in tests that don't need them; each then
// simply reports 0 rather than panicking.
func newReactiveGaugeCollectors(s *Server) []prometheus.Collector {
	var collectors []prometheus.Collector

	if s.Buffer != nil {
		for _, name := range []string{"tronsell", "netts", "catfee"} {
			name := name
			collectors = append(collectors, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name:        "broker_buffer_level",
				Help:        "Current AVAILABLE energy_buffer units, by provider.",
				ConstLabels: prometheus.Labels{"provider": name},
			}, func() float64 {
				totals, err := s.Buffer.Totals(context.Background())
				if err != nil {
					return 0
				}
				for _, t := range totals {
					if t.ProviderName == name {
						return float64(t.Available)
					}
				}
				return 0
			}))
		}

		collectors = append(collectors, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "broker_buffer_target",
			Help: "Current TargetLevel the buffer is replenishing toward.",
		}, func() float64 {
			target, err := s.Buffer.TargetLevel(context.Background())
			if err != nil {
				return 0
			}
			return float64(target)
		}))
	}

	if s.Poller != nil {
		for _, name := range []string{"tronsell", "netts", "catfee"} {
			name := name
			collectors = append(collectors, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name:        "broker_price_staleness_seconds",
				Help:        "Seconds since the last successful price observation, by provider; 0 if never observed at all.",
				ConstLabels: prometheus.Labels{"provider": name},
			}, func() float64 {
				for _, p := range s.Poller.Snapshot(context.Background()) {
					if p.ProviderName == name && p.Healthy {
						return time.Since(p.ObservedAt).Seconds()
					}
				}
				return 0
			}))
		}
	}

	return collectors
}
