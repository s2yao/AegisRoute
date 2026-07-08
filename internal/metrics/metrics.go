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

	// Stage-8 observability additions (all additive):
	ChatCompletionDurationSeconds  *prometheus.HistogramVec // by cache outcome
	BackendRetriesTotal            *prometheus.CounterVec   // outbound retries
	CircuitShortCircuitsTotal      *prometheus.CounterVec   // skipped open circuits
	CircuitBreakerTransitionsTotal *prometheus.CounterVec   // state changes, by target
	BackendInFlight                *prometheus.GaugeVec     // live semaphore holders
}

// latencyBuckets are fine-grained buckets tuned for a low-latency gateway:
// cache HITs are sub-millisecond and gateway overhead is a few ms, so the
// default prometheus.DefBuckets (which jump .005 → .01 → .025 → ...) place the
// p95/p99 estimate in a coarse bucket and read wrong on a dashboard. These add
// resolution below 25ms while still covering slow backend calls up to 10s.
var latencyBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// New builds a fresh registry and registers all collectors into it.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_http_requests_total",
			Help: "HTTP requests served, by chi route pattern, method, and status.",
		}, []string{"route", "method", "status"}),
		HTTPRequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aegisroute_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by chi route pattern and method.",
			Buckets: latencyBuckets,
		}, []string{"route", "method"}),
		BackendRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_backend_requests_total",
			Help: "Upstream backend calls, by backend name and outcome.",
		}, []string{"backend", "outcome"}),
		BackendRequestDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aegisroute_backend_request_duration_seconds",
			Help:    "Upstream backend call duration in seconds, by backend name.",
			Buckets: latencyBuckets,
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
		ChatCompletionDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aegisroute_chat_completion_duration_seconds",
			Help:    "End-to-end /v1/chat/completions duration in seconds, by cache outcome (hit|miss|bypass).",
			Buckets: latencyBuckets,
		}, []string{"cache"}),
		BackendRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_backend_retries_total",
			Help: "Outbound backend call retries (attempts after the first), by backend.",
		}, []string{"backend"}),
		CircuitShortCircuitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_circuit_breaker_short_circuits_total",
			Help: "Requests NOT sent to a backend because its circuit was open, by backend.",
		}, []string{"backend"}),
		CircuitBreakerTransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aegisroute_circuit_breaker_transitions_total",
			Help: "Circuit breaker state transitions, by backend and target state (closed|half_open|open).",
		}, []string{"backend", "to"}),
		BackendInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aegisroute_backend_in_flight",
			Help: "Current in-flight requests per backend (max_in_flight semaphore holders).",
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
		m.ChatCompletionDurationSeconds,
		m.BackendRetriesTotal,
		m.CircuitShortCircuitsTotal,
		m.CircuitBreakerTransitionsTotal,
		m.BackendInFlight,
	)
	return m
}

// Handler serves this instance's registry (and only it) for Prometheus
// scrapes.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
