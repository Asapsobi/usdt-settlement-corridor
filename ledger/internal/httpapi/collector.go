package httpapi

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// ordersByStateCollector implements prometheus.Collector directly,
// rather than a GaugeVec kept updated some other way, because the set of
// label values (which states currently have any orders) is itself
// dynamic. A GaugeFunc can't carry a "state" label at all; this computes
// the whole distribution fresh on every scrape with one query.
type ordersByStateCollector struct {
	pool *pgxpool.Pool
	desc *prometheus.Desc
}

func newOrdersByStateCollector(pool *pgxpool.Pool) *ordersByStateCollector {
	return &ordersByStateCollector{
		pool: pool,
		desc: prometheus.NewDesc("ledger_orders_by_state", "Count of orders currently in each state.", []string{"state"}, nil),
	}
}

func (c *ordersByStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *ordersByStateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	rows, err := c.pool.Query(ctx, "SELECT state, count(*) FROM orders GROUP BY state")
	if err != nil {
		slog.Error("metrics: querying orders_by_state", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			slog.Error("metrics: scanning orders_by_state row", "error", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), state)
	}
	if err := rows.Err(); err != nil {
		slog.Error("metrics: iterating orders_by_state rows", "error", err)
	}
}
