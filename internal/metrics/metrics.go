package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns the process registry and every aegisroute_* collector.
// Features increment via an injected *Metrics; nothing touches the global
// default registry.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequestsTotal             *prometheus.CounterVec
	HTTPRequestDurationSeconds    *prometheus.HistogramVec
	BackendRequestsTotal          *prometheus.CounterVec
	BackendRequestDurationSeconds *prometheus.HistogramVec
	CacheEventsTotal              *prometheus.CounterVec
	RateLimitedTotal              prometheus.Counter
	BatchJobsCreatedTotal         prometheus.Counter
	BatchItemsProcessedTotal      *prometheus.CounterVec
	WorkerFailuresTotal           prometheus.Counter
	CircuitBreakerState           *prometheus.GaugeVec
}

// New builds a fresh registry and registers all collectors into it.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_http_requests_total",
			Help: "HTTP requests served, by chi route pattern, method, and status.",
		}, []string{"route", "method", "status"}),
		HTTPRequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "aegisroute_http_request_duration_seconds",
			Help: "HTTP request duration in seconds, by chi route pattern and method.",
		}, []string{"route", "method"}),
		BackendRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_backend_requests_total",
			Help: "Upstream backend calls, by backend name and outcome.",
		}, []string{"backend", "outcome"}),
		BackendRequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "aegisroute_backend_request_duration_seconds",
			Help: "Upstream backend call duration in seconds, by backend name.",
		}, []string{"backend"}),
		CacheEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_cache_events_total",
			Help: "Response cache lookups, by result (hit|miss|bypass).",
		}, []string{"result"}),
		RateLimitedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aegisroute_rate_limited_total",
			Help: "Requests rejected by the rate limiter.",
		}),
		BatchJobsCreatedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aegisroute_batch_jobs_created_total",
			Help: "Batch jobs accepted and published to the stream.",
		}),
		BatchItemsProcessedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_batch_items_processed_total",
			Help: "Batch items processed by the worker, by outcome.",
		}, []string{"outcome"}),
		WorkerFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aegisroute_worker_failures_total",
			Help: "Unexpected worker-level failures.",
		}),
		CircuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aegisroute_circuit_breaker_state",
			Help: "Circuit breaker state per backend (0=closed, 1=half-open, 2=open).",
		}, []string{"backend"}),
	}
	m.registry.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDurationSeconds,
		m.BackendRequestsTotal,
		m.BackendRequestDurationSeconds,
		m.CacheEventsTotal,
		m.RateLimitedTotal,
		m.BatchJobsCreatedTotal,
		m.BatchItemsProcessedTotal,
		m.WorkerFailuresTotal,
		m.CircuitBreakerState,
	)
	return m
}

// Handler serves this instance's registry (and only it) for Prometheus
// scrapes.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
