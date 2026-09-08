package httpapi

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"dispatcher/internal/slots"
)

// Metrics holds the counters this package's own handlers increment
// directly. slot_balance and batch_queue_depth are NOT counters here:
// both are computed fresh from C1's own real balance / the queue table
// at scrape time (via the two Collector types below), so neither can
// ever drift from what actually exists between scrapes -- same reasoning
// every prior component's own reactive gauges use.
type Metrics struct {
	DispatchesTotal                  *prometheus.CounterVec
	BroadcastAttemptsTotal           prometheus.Counter
	DuplicateBroadcastPreventedTotal prometheus.Counter
}

// NewMetrics builds and registers dispatches_total{outcome},
// broadcast_attempts_total, duplicate_broadcast_prevented_total.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		DispatchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dispatcher_dispatches_total",
			Help: "Total orders dispatched, by outcome (dispatching, settled, held, failed).",
		}, []string{"outcome"}),
		BroadcastAttemptsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dispatcher_broadcast_attempts_total",
			Help: "Total Broadcast calls made, regardless of outcome.",
		}),
		DuplicateBroadcastPreventedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dispatcher_duplicate_broadcast_prevented_total",
			Help: "Total Broadcast calls that found an existing terminal (BROADCAST or later) row and skipped re-broadcasting -- invariant 1's own proof, surfaced live.",
		}),
	}
	reg.MustRegister(m.DispatchesTotal, m.BroadcastAttemptsTotal, m.DuplicateBroadcastPreventedTotal)
	return m
}

// newReactiveGaugeCollectors builds slot_balance{slot_id} and
// batch_queue_depth -- always fresh at scrape time.
func newReactiveGaugeCollectors(s *Server) []prometheus.Collector {
	return []prometheus.Collector{
		newSlotBalanceCollector(s),
		newBatchQueueDepthCollector(s),
	}
}

// slotBalanceCollector and batchQueueDepthCollector are their own
// prometheus.Collector implementations rather than GaugeFunc: slot_balance
// needs one gauge PER slot (a dynamic set, discovered at scrape time),
// which GaugeFunc's single-value shape can't express directly.
type slotBalanceCollector struct {
	server *Server
	desc   *prometheus.Desc
}

func newSlotBalanceCollector(s *Server) *slotBalanceCollector {
	return &slotBalanceCollector{
		server: s,
		desc:   prometheus.NewDesc("dispatcher_slot_balance", "Current asset:tron:slot:<id> balance, by slot.", []string{"slot_id"}, nil),
	}
}

func (c *slotBalanceCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *slotBalanceCollector) Collect(ch chan<- prometheus.Metric) {
	if c.server.Slots == nil || c.server.Ledger == nil {
		return
	}
	ctx := context.Background()
	for _, status := range []slots.Status{slots.StatusActive, slots.StatusRetiring} {
		list, err := c.server.Slots.List(ctx, status)
		if err != nil {
			continue
		}
		for _, slot := range list {
			balance, err := c.server.Ledger.GetAccountBalance(ctx, fmt.Sprintf("asset:tron:slot:%d", slot.ID))
			if err != nil {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(balance), fmt.Sprintf("%d", slot.ID))
		}
	}
}

type batchQueueDepthCollector struct {
	server *Server
	desc   *prometheus.Desc
}

func newBatchQueueDepthCollector(s *Server) *batchQueueDepthCollector {
	return &batchQueueDepthCollector{
		server: s,
		desc:   prometheus.NewDesc("dispatcher_batch_queue_depth", "Current count of QUEUED Sweep-tier orders awaiting a cut batch.", nil, nil),
	}
}

func (c *batchQueueDepthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *batchQueueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	if c.server.Dispatcher == nil || c.server.Dispatcher.Batches == nil {
		return
	}
	queued, err := c.server.Dispatcher.Batches.ListQueued(context.Background(), 100000)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(len(queued)))
}
