// Package metrics sets up the Prometheus registry for this service.
//
// It uses its own registry (not the global one) so test components or other
// components cannot inject unwanted metrics, and so that every server instance
// has isolated metrics.
package metrics

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the metric collectors and their registry.
type Metrics struct {
	registry     *prometheus.Registry
	requests     *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	inFlight     prometheus.Gauge
	transactions *prometheus.CounterVec

	// shuttingDown marks that the pool is (about to be) closed; the pool gauges stop
	// reading statistics so they never touch a closed pool.
	shuttingDown atomic.Bool
}

// New builds the registry together with every service metric.
//
// pool may be nil (e.g. in tests) - pool metrics are then skipped.
func New(pool *pgxpool.Pool) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by method, route pattern, and status code.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Distribusi latensi request HTTP.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of requests currently being processed.",
		}),
		transactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wallet_transactions_total",
			Help: "Number of money movements by type and result (success, replayed, or an error code).",
		}, []string{"type", "result"}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests,
		m.duration,
		m.inFlight,
		m.transactions,
	)

	if pool != nil {
		m.registerPoolMetrics(pool)
	}

	return m
}

// Handler returns the http.Handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry returns the service registry, for example to register extra
// collectors or to make assertions in tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// MarkShuttingDown is called before the database pool is closed so the pool
// gauges stop reading statistics from an invalid pool.
func (m *Metrics) MarkShuttingDown() { m.shuttingDown.Store(true) }

func (m *Metrics) ObserveHTTP(method, route, status string, duration time.Duration) {
	m.requests.WithLabelValues(method, route, status).Inc()
	m.duration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) IncInFlight() { m.inFlight.Inc() }

func (m *Metrics) DecInFlight() { m.inFlight.Dec() }

// RecordTransaction records the outcome of one money movement. The result value
// is "success", "replayed", or an error code (e.g. "insufficient_funds").
func (m *Metrics) RecordTransaction(kind, result string) {
	m.transactions.WithLabelValues(kind, result).Inc()
}

func (m *Metrics) registerPoolMetrics(pool *pgxpool.Pool) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_pool_connections_total",
		Help: "Number of connections in the pool (all states).",
	}, func() float64 {
		return m.poolGauge(pool, func(s *pgxpool.Stat) float64 { return float64(s.TotalConns()) })
	}))

	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_pool_connections_idle",
		Help: "Number of idle connections in the pool.",
	}, func() float64 {
		return m.poolGauge(pool, func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) })
	}))

	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_pool_connections_acquired",
		Help: "Number of connections currently in use.",
	}, func() float64 {
		return m.poolGauge(pool, func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) })
	}))

	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_pool_max_connections",
		Help: "Maximum number of connections in the pool.",
	}, func() float64 {
		return m.poolGauge(pool, func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) })
	}))

	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "db_pool_acquire_total",
		Help: "Total connections successfully acquired from the pool.",
	}, func() float64 {
		return m.poolGauge(pool, func(s *pgxpool.Stat) float64 { return float64(s.AcquireCount()) })
	}))
}

func (m *Metrics) poolGauge(pool *pgxpool.Pool, extract func(*pgxpool.Stat) float64) float64 {
	if m.shuttingDown.Load() {
		return 0
	}
	return extract(pool.Stat())
}
