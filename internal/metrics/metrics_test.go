package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/metrics"
)

func TestNewNeverPanicsOrDoubleRegisters(t *testing.T) {
	require.NotPanics(t, func() {
		m1 := metrics.New()
		m2 := metrics.New()
		require.NotNil(t, m1)
		require.NotNil(t, m2)
		// Isolated registries: incrementing one must not affect the other.
		m1.RateLimitedTotal.Inc()
	})
}

func TestHandlerServesRegisteredMetrics(t *testing.T) {
	m := metrics.New()
	m.HTTPRequestsTotal.WithLabelValues("/healthz", "GET", "200").Inc()
	m.RateLimitedTotal.Inc()
	m.CacheEventsTotal.WithLabelValues("hit").Inc()
	m.BatchItemsProcessedTotal.WithLabelValues("succeeded").Inc()
	m.CircuitBreakerState.WithLabelValues("mock-llm-fast").Set(2)
	m.BackendRequestsTotal.WithLabelValues("mock-llm-fast", "success").Inc()
	m.BackendRequestDurationSeconds.WithLabelValues("mock-llm-fast").Observe(0.05)
	m.HTTPRequestDurationSeconds.WithLabelValues("/healthz", "GET").Observe(0.01)
	m.BatchJobsCreatedTotal.Inc()
	m.WorkerFailuresTotal.Inc()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Labels render alphabetically in the exposition format.
	assert.Contains(t, body, `aegisroute_http_requests_total{method="GET",route="/healthz",status="200"} 1`)
	assert.Contains(t, body, "aegisroute_rate_limited_total 1")
	assert.Contains(t, body, `aegisroute_cache_events_total{result="hit"} 1`)
	assert.Contains(t, body, `aegisroute_batch_items_processed_total{outcome="succeeded"} 1`)
	assert.Contains(t, body, `aegisroute_circuit_breaker_state{backend="mock-llm-fast"} 2`)
	assert.Contains(t, body, `aegisroute_backend_requests_total{backend="mock-llm-fast",outcome="success"} 1`)
	assert.Contains(t, body, `aegisroute_backend_request_duration_seconds_count{backend="mock-llm-fast"} 1`)
	assert.Contains(t, body, `aegisroute_http_request_duration_seconds_count{method="GET",route="/healthz"} 1`)
	assert.Contains(t, body, "aegisroute_batch_jobs_created_total 1")
	assert.Contains(t, body, "aegisroute_worker_failures_total 1")
}

func TestRegistriesAreIsolated(t *testing.T) {
	m1 := metrics.New()
	m2 := metrics.New()
	m1.RateLimitedTotal.Inc()

	rec := httptest.NewRecorder()
	m2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Contains(t, rec.Body.String(), "aegisroute_rate_limited_total 0")
}
