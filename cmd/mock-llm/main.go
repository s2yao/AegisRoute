// Command mock-llm is a deterministic fake OpenAI-compatible backend used by
// gateway-api and control-worker so routing and inference are demoable
// without a real model provider. Identical request bodies always produce
// identical completions (the content is derived from a hash of the body),
// and env knobs inject latency and failures to exercise the gateway's
// retry and circuit-breaker paths. Compose runs two instances: "fast" and
// "cheap".
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// mockCreated is the fixed "created" timestamp in every completion, keeping
// the entire response byte-for-byte deterministic for one request body.
const mockCreated = 1735689600 // 2025-01-01T00:00:00Z

// maxBodyBytes caps inbound request bodies; the gateway enforces 1 MiB, so
// the mock accepts the same.
const maxBodyBytes = 1 << 20

// shutdownTimeout bounds graceful drain on SIGINT/SIGTERM.
const shutdownTimeout = 10 * time.Second

// config holds the MOCK_* environment knobs.
type config struct {
	port        int           // MOCK_PORT
	latency     time.Duration // MOCK_LATENCY_MS: fixed delay per completion
	jitter      time.Duration // MOCK_JITTER_MS: extra uniform random delay
	failureRate float64       // MOCK_FAILURE_RATE: 0..1 fraction answered 503
	backendName string        // MOCK_BACKEND_NAME: identifies the instance
	modelName   string        // MOCK_MODEL_NAME: model echoed in completions
}

// loadConfig reads the MOCK_* variables, applying defaults for unset ones
// and rejecting malformed or out-of-range values.
func loadConfig() (config, error) {
	var errs []string
	cfg := config{
		port:        envInt("MOCK_PORT", 8081, &errs),
		latency:     time.Duration(envInt("MOCK_LATENCY_MS", 0, &errs)) * time.Millisecond,
		jitter:      time.Duration(envInt("MOCK_JITTER_MS", 0, &errs)) * time.Millisecond,
		failureRate: envFloat("MOCK_FAILURE_RATE", 0, &errs),
		backendName: envString("MOCK_BACKEND_NAME", "mock-llm"),
		modelName:   envString("MOCK_MODEL_NAME", "llama-fast"),
	}
	if cfg.port < 1 || cfg.port > 65535 {
		errs = append(errs, fmt.Sprintf("MOCK_PORT: must be a valid TCP port, got %d", cfg.port))
	}
	if cfg.latency < 0 {
		errs = append(errs, "MOCK_LATENCY_MS: must be >= 0")
	}
	if cfg.jitter < 0 {
		errs = append(errs, "MOCK_JITTER_MS: must be >= 0")
	}
	if cfg.failureRate < 0 || cfg.failureRate > 1 {
		errs = append(errs, fmt.Sprintf("MOCK_FAILURE_RATE: must be in [0,1], got %g", cfg.failureRate))
	}
	if len(errs) > 0 {
		return config{}, fmt.Errorf("mock-llm config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.port),
		Handler:           newHandler(cfg, rand.New(rand.NewSource(time.Now().UnixNano()))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("mock-llm listening",
			"addr", srv.Addr, "backend", cfg.backendName, "model", cfg.modelName,
			"latency_ms", cfg.latency.Milliseconds(), "jitter_ms", cfg.jitter.Milliseconds(),
			"failure_rate", cfg.failureRate)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		logger.Info("mock-llm stopped")
	}
}

// mockServer holds the handler state. rng drives failure injection and
// latency jitter and is mutex-guarded because handlers run concurrently.
type mockServer struct {
	cfg config

	mu  sync.Mutex
	rng *rand.Rand
}

// newHandler builds the mock's HTTP handler; tests wrap it in httptest.
func newHandler(cfg config, rng *rand.Rand) http.Handler {
	s := &mockServer{cfg: cfg, rng: rng}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /v1/chat/completions", s.completions)
	return mux
}

func (s *mockServer) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// chatRequest is the loosely parsed inbound completion request. The mock
// stays tolerant on purpose — strict validation is the gateway's job.
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func (s *mockServer) completions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "model and messages are required")
		return
	}

	if s.injectFailure() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "mock backend injected failure")
		return
	}
	s.simulateLatency(r.Context())

	// The completion is a pure function of the request bytes: same body in,
	// same body out. That determinism is what makes cache and idempotency
	// behavior observable in later stages.
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:8])
	content := fmt.Sprintf("mock completion %s from %s for model %s", digest, s.cfg.backendName, req.Model)

	promptTokens := 0
	for _, m := range req.Messages {
		promptTokens += len(strings.Fields(m.Content))
	}
	completionTokens := len(strings.Fields(content))

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + digest,
		"object":  "chat.completion",
		"created": mockCreated,
		"model":   s.cfg.modelName,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]string{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
}

// injectFailure draws against MOCK_FAILURE_RATE. Rates 0 and 1 are exact:
// never and always.
func (s *mockServer) injectFailure() bool {
	if s.cfg.failureRate <= 0 {
		return false
	}
	if s.cfg.failureRate >= 1 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < s.cfg.failureRate
}

// simulateLatency sleeps MOCK_LATENCY_MS plus up to MOCK_JITTER_MS, giving
// up early if the caller goes away.
func (s *mockServer) simulateLatency(ctx context.Context) {
	d := s.cfg.latency
	if s.cfg.jitter > 0 {
		s.mu.Lock()
		d += time.Duration(s.rng.Int63n(int64(s.cfg.jitter) + 1))
		s.mu.Unlock()
	}
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// writeOpenAIError emits an OpenAI-style error body, the shape clients of a
// real backend would see.
func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, errs *[]string) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not an integer: %q", key, v))
		return def
	}
	return n
}

func envFloat(key string, def float64, errs *[]string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not a number: %q", key, v))
		return def
	}
	return f
}
