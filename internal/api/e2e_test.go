package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/routing"
)

// This file wires the REAL routing.Selector, inference.Client, and
// routing.Breaker together — exactly as cmd/gateway-api does — against
// httptest backends, so the integration seams the handler unit tests stub out
// are protected from regression.

// e2eBackendStore satisfies routing.BackendStore (ListByModelEnabled).
type e2eBackendStore struct{ backends []models.ModelBackend }

func (s *e2eBackendStore) ListByModelEnabled(_ context.Context, model string) ([]models.ModelBackend, error) {
	var out []models.ModelBackend
	for _, b := range s.backends {
		if b.Enabled && b.ModelName == model {
			out = append(out, b)
		}
	}
	return out, nil
}

// e2ePolicyStore satisfies routing.PolicyStore, always forcing the in-memory
// fallback policy.
type e2ePolicyStore struct{}

func (e2ePolicyStore) GetForModel(context.Context, string) (models.RoutingPolicy, error) {
	// The real sentinel: the selector treats a not-found policy as the
	// in-memory fallback, so its errors.Is check stays honest.
	return models.RoutingPolicy{}, db.ErrNotFound
}

func buildE2E(t *testing.T, backends []models.ModelBackend, threshold int) (http.Handler, *routing.Breaker, *fakeLedger) {
	t.Helper()
	breaker := routing.NewBreaker(threshold, 50*time.Millisecond)
	selector := routing.NewSelector(&e2eBackendStore{backends}, e2ePolicyStore{}, breaker)
	client := inference.New(inference.Config{
		Timeout:     2 * time.Second,
		MaxAttempts: 2,
		BackoffBase: time.Millisecond,
		BackoffMax:  2 * time.Millisecond,
		Metrics:     metrics.New(),
	})
	ledger := &fakeLedger{}

	deps := testDeps()
	deps.Selector = selector
	deps.Inference = client
	deps.Circuit = breaker
	deps.Ledger = ledger
	return api.NewRouter(deps), breaker, ledger
}

func e2ePost(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validChatBody))
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	return do(t, h, req)
}

func e2eBackendAt(url, name string, priority int) models.ModelBackend {
	return models.ModelBackend{
		ID: uuid.New(), Name: name, BaseURL: url, ModelName: "llama-fast",
		Kind: models.BackendKindMock, Enabled: true, Priority: priority, Weight: 1, MaxInFlight: 32,
	}
}

func TestE2EHappyPath(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"model":"llama-fast"`)
		assert.NotContains(t, string(body), "stream", "the gateway never forwards stream")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","choices":[]}`))
	}))
	defer backend.Close()

	h, _, ledger := buildE2E(t, []models.ModelBackend{e2eBackendAt(backend.URL, "be-fast", 10)}, 5)

	rec := e2ePost(t, h)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "be-fast", rec.Header().Get("X-AegisRoute-Backend"))
	assert.Equal(t, "default", rec.Header().Get("X-AegisRoute-Routing-Policy"))
	assert.Contains(t, rec.Body.String(), "chatcmpl-x")
	assert.Equal(t, int64(1), hits.Load())
	assert.Len(t, ledger.all(), 1, "the completion is audited")
}

func TestE2EIntraRequestFailover(t *testing.T) {
	// Backend A always 503, B healthy: a SINGLE request must fail over to B and
	// succeed — the core gateway behavior added in this pass.
	var fastHits, cheapHits atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fastHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer fast.Close()
	cheap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cheapHits.Add(1)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-cheap","choices":[]}`))
	}))
	defer cheap.Close()

	h, _, _ := buildE2E(t, []models.ModelBackend{
		e2eBackendAt(fast.URL, "be-fast", 10),
		e2eBackendAt(cheap.URL, "be-cheap", 20),
	}, 5)

	rec := e2ePost(t, h)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "be-cheap", rec.Header().Get("X-AegisRoute-Backend"),
		"one request failed over from the dead backend to the healthy one")
	assert.Contains(t, rec.Body.String(), "chatcmpl-cheap")
	assert.Positive(t, fastHits.Load(), "the dead backend was tried first")
	assert.Equal(t, int64(1), cheapHits.Load())
}

func TestE2ECircuitOpensAndShedsLoad(t *testing.T) {
	// A single dead backend: after CB_FAILURE_THRESHOLD failed requests its
	// circuit opens, and further requests are shed by the selector without ever
	// reaching the backend again.
	var hits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	threshold := 3
	h, breaker, _ := buildE2E(t, []models.ModelBackend{e2eBackendAt(dead.URL, "be-dead", 10)}, threshold)

	for range threshold {
		rec := e2ePost(t, h)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	}
	require.Equal(t, models.CircuitStateOpen, breaker.State("be-dead"),
		"the circuit opens after the threshold of failed requests")

	hitsBefore := hits.Load()
	rec := e2ePost(t, h)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, hitsBefore, hits.Load(),
		"an open circuit sheds the request without hitting the backend")
}
