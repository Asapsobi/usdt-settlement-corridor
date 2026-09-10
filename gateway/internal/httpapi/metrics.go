package httpapi

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the counters this package's own handlers increment
// directly -- C6.8's own list: quotes_issued_total,
// orders_created_total, webhook_deliveries_total{result},
// webhook_retry_exhausted_total, sandbox_requests_total{trigger}.
// orders_address_pending is NOT a counter here: it mirrors C6.4's own
// reconcile.Reconciler.PendingCount(), computed fresh at scrape time
// via a Collector (below) so it can never drift from the reconciler's
// own live state between scrapes -- same reasoning every prior
// component's own reactive gauges use.
type Metrics struct {
	QuotesIssuedTotal          prometheus.Counter
	OrdersCreatedTotal         prometheus.Counter
	WebhookDeliveriesTotal     *prometheus.CounterVec
	WebhookRetryExhaustedTotal prometheus.Counter
	SandboxRequestsTotal       *prometheus.CounterVec
}

// NewMetrics builds and registers every counter above.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		QuotesIssuedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_quotes_issued_total",
			Help: "Total quotes issued via POST /v1/quotes.",
		}),
		OrdersCreatedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_orders_created_total",
			Help: "Total orders created via POST /v1/orders.",
		}),
		WebhookDeliveriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_webhook_deliveries_total",
			Help: "Total webhook delivery attempts, by result (success, failure).",
		}, []string{"result"}),
		WebhookRetryExhaustedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gateway_webhook_retry_exhausted_total",
			Help: "Total webhook deliveries that exhausted every retry attempt.",
		}),
		SandboxRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_sandbox_requests_total",
			Help: "Total sandbox order requests, by trigger.",
		}, []string{"trigger"}),
	}
	reg.MustRegister(m.QuotesIssuedTotal, m.OrdersCreatedTotal, m.WebhookDeliveriesTotal, m.WebhookRetryExhaustedTotal, m.SandboxRequestsTotal)
	return m
}

// DeliveryAttempted and RetryExhausted satisfy webhooks.MetricsRecorder
// directly -- the same "the concrete type already has the shape a
// sibling package's own interface needs" pattern used throughout this
// module (c1client.Client/webhooks.OrderPoller, customers.Store/
// webhooks.CustomerLookup).
func (m *Metrics) DeliveryAttempted(success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	m.WebhookDeliveriesTotal.WithLabelValues(result).Inc()
}

func (m *Metrics) RetryExhausted() {
	m.WebhookRetryExhaustedTotal.Inc()
}

// PendingAddressGauge is the one call newReactiveGaugeCollectors needs
// from C6.4's own reconciler -- kept as a narrow interface here rather
// than importing gateway/internal/reconcile directly, so this package
// doesn't need to know about reconcile.Reconciler's full shape.
type PendingAddressGauge interface {
	PendingCount() int64
}

// newReactiveGaugeCollectors builds orders_address_pending -- always
// fresh at scrape time, straight from the reconciler's own last tick.
func newReactiveGaugeCollectors(s *Server) []prometheus.Collector {
	return []prometheus.Collector{newPendingAddressCollector(s)}
}

type pendingAddressCollector struct {
	server *Server
	desc   *prometheus.Desc
}

func newPendingAddressCollector(s *Server) *pendingAddressCollector {
	return &pendingAddressCollector{
		server: s,
		desc:   prometheus.NewDesc("gateway_orders_address_pending", "Current count of gateway_orders rows stuck in address_pending, per C6.4's own reconciler.", nil, nil),
	}
}

func (c *pendingAddressCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *pendingAddressCollector) Collect(ch chan<- prometheus.Metric) {
	if c.server.PendingAddress == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(c.server.PendingAddress.PendingCount()))
}
