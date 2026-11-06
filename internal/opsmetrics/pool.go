package opsmetrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/solidDoWant/toolgate/internal/version"
)

// Pool states.
var poolStates = []string{"idle", "in_use", "max"}

// PoolStats is the source of the connection pool numbers; *pgxpool.Pool
// satisfies it.
type PoolStats interface {
	Stat() *pgxpool.Stat
}

// PoolCollector exports the statistics of a connection pool at scrape time.
func PoolCollector(pool PoolStats) prometheus.Collector {
	return &poolCollector{
		pool:        pool,
		connections: prometheus.NewDesc("toolgate_db_pool_connections", "Connections of the database pool, by state.", []string{"state"}, nil),
		wait:        prometheus.NewDesc("toolgate_db_pool_wait_seconds_total", "Seconds spent waiting for a connection when the pool had none idle.", nil, nil),
		acquires:    prometheus.NewDesc("toolgate_db_pool_acquire_total", "Connections taken from the database pool.", nil, nil),
	}
}

type poolCollector struct {
	pool                        PoolStats
	connections, wait, acquires *prometheus.Desc
}

func (c *poolCollector) Describe(descs chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{c.connections, c.wait, c.acquires} {
		descs <- desc
	}
}

func (c *poolCollector) Collect(metrics chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	values := map[string]float64{
		"idle":   float64(stat.IdleConns()),
		"in_use": float64(stat.AcquiredConns()),
		"max":    float64(stat.MaxConns()),
	}

	for _, state := range poolStates {
		metrics <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, values[state], state)
	}

	metrics <- prometheus.MustNewConstMetric(c.wait, prometheus.CounterValue, stat.EmptyAcquireWaitTime().Seconds())

	metrics <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(stat.AcquireCount()))
}

// BuildInfo returns the gauge toolgate_build_info, set to 1, labelled with
// the build version.
func BuildInfo() prometheus.Collector {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "toolgate_build_info",
		Help: "Always 1. The version label is the version of this build.",
	}, []string{"version"})
	gauge.WithLabelValues(version.Get()).Set(1)

	return gauge
}
