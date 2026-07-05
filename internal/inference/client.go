package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
)

// completionsPath is the OpenAI-compatible endpoint every backend serves.
const completionsPath = "/v1/chat/completions"

// Metric outcome labels for aegisroute_backend_requests_total. Every attempt
// (including each retry) counts once. "canceled" marks attempts that died
// because the caller's context ended — those say nothing about the backend
// and must not pollute the error outcomes.
const (
	outcomeSuccess        = "success"
	outcomeTransientError = "transient_error"
	outcomePermanentError = "permanent_error"
	outcomeCanceled       = "canceled"
)

// Response is a successful (2xx) upstream reply: the status and the fully
// read body. The body is already drained and closed by the client.
type Response struct {
	StatusCode int
	Body       []byte
}

// Error is the typed failure returned by Client.Do. Transient marks failures
// worth retrying (timeout, connection error, 502/503/504); everything else —
// 400/401/403/404 and any other non-2xx response — is permanent and is never
// retried. A call ended by the caller's own context is also non-transient
// (Err wraps the context error): it carries no verdict about the backend.
// Status is 0 when no HTTP response was received.
type Error struct {
	Backend   string
	Status    int
	Transient bool
	Err       error
}

func (e *Error) Error() string {
	kind := "permanent"
	if e.Transient {
		kind = "transient"
	}
	if e.Err != nil {
		return fmt.Sprintf("inference: backend %s: %s failure: %v", e.Backend, kind, e.Err)
	}
	return fmt.Sprintf("inference: backend %s: %s failure: upstream status %d", e.Backend, kind, e.Status)
}

func (e *Error) Unwrap() error { return e.Err }

// IsTransient reports whether err is an inference failure that a retry (or a
// different backend) might cure. It is the signal callers feed the circuit
// breaker: transient failures count against a backend, permanent upstream
// errors do not (the backend answered, so it is alive).
func IsTransient(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Transient
}

// Config carries everything New needs. HTTPClient, Metrics, Rand, and Sleep
// are injection points for tests; leaving them nil selects production
// defaults.
type Config struct {
	// HTTPClient issues the requests. Default: a plain &http.Client{}. Per-
	// attempt timeouts come from Timeout via context, not http.Client.Timeout.
	HTTPClient *http.Client
	// Timeout bounds one attempt (BACKEND_TIMEOUT_MS).
	Timeout time.Duration
	// MaxAttempts is the total number of tries including the first
	// (RETRY_MAX_ATTEMPTS).
	MaxAttempts int
	// BackoffBase and BackoffMax bound the exponential backoff between
	// retries (RETRY_BASE_MS / RETRY_MAX_MS): before retry n the client
	// sleeps a full-jitter duration in [0, min(BackoffMax, BackoffBase·2ⁿ⁻¹)].
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// Metrics receives per-attempt backend counters and durations; nil skips
	// instrumentation.
	Metrics *metrics.Metrics
	// Rand drives the jitter draw. Default: a time-seeded source.
	Rand *rand.Rand
	// Sleep waits between retries and must return early with the context's
	// error when ctx is done. Default: a timer-based wait. Tests inject a
	// recorder to assert backoff bounds without real sleeping.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client executes one outbound inference call against a chosen backend with
// a per-attempt timeout and bounded retries. It is shared by the completion
// handler and the batch worker — the two talk to backends through this same
// type, never through an HTTP hop between our own services. Safe for
// concurrent use.
type Client struct {
	http        *http.Client
	timeout     time.Duration
	maxAttempts int
	backoffBase time.Duration
	backoffMax  time.Duration
	metrics     *metrics.Metrics
	sleep       func(ctx context.Context, d time.Duration) error

	mu  sync.Mutex // rand.Rand is not thread-safe
	rng *rand.Rand
}

// New builds a Client from cfg, filling nil injection points with defaults.
func New(cfg Config) *Client {
	c := &Client{
		http:        cfg.HTTPClient,
		timeout:     cfg.Timeout,
		maxAttempts: cfg.MaxAttempts,
		backoffBase: cfg.BackoffBase,
		backoffMax:  cfg.BackoffMax,
		metrics:     cfg.Metrics,
		rng:         cfg.Rand,
		sleep:       cfg.Sleep,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.maxAttempts < 1 {
		c.maxAttempts = 1
	}
	if c.rng == nil {
		c.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	return c
}

// Do POSTs body to the backend's /v1/chat/completions, retrying only
// transient failures (timeout, connection error, 502/503/504) up to
// MaxAttempts with exponential backoff and full jitter. Permanent failures
// (any other non-2xx, notably 400/401/403/404) return immediately. Context
// cancellation aborts both in-flight attempts and backoff waits and is
// reported as a non-transient Error wrapping the context error. Every
// response body is fully drained and closed on every path.
func (c *Client) Do(ctx context.Context, backend models.ModelBackend, body []byte) (*Response, error) {
	url := strings.TrimRight(backend.BaseURL, "/") + completionsPath

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, c.backoff(attempt-1)); err != nil {
				// The caller's context died during backoff: not a backend
				// verdict, so the error is deliberately non-transient.
				return nil, &Error{Backend: backend.Name, Transient: false, Err: err}
			}
		}

		resp, err := c.attempt(ctx, backend, url, body)
		if err == nil {
			return resp, nil
		}
		if !IsTransient(err) {
			return nil, err
		}
		lastErr = err

		// A transient failure caused by the caller's context ending is not
		// retryable: the request as a whole is over.
		if ctx.Err() != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// attempt performs one HTTP call. The response body is always read to
// completion and closed here, so no path leaks a connection.
func (c *Client) attempt(ctx context.Context, backend models.ModelBackend, url string, body []byte) (*Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// A malformed base_url is a configuration bug, not a backend outage.
		return nil, &Error{Backend: backend.Name, Transient: false, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		// Timeouts and connection errors surface here; both are transient —
		// unless the *caller's* context is what died. A per-attempt timeout
		// expires only attemptCtx and stays a backend verdict; a dead parent
		// context is the client going away and says nothing about the
		// backend, so it must not count as a failure anywhere.
		if ctx.Err() != nil {
			c.observe(backend.Name, outcomeCanceled, start)
			return nil, &Error{Backend: backend.Name, Transient: false, Err: ctx.Err()}
		}
		c.observe(backend.Name, outcomeTransientError, start)
		return nil, &Error{Backend: backend.Name, Transient: true, Err: err}
	}
	respBody, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		if ctx.Err() != nil {
			c.observe(backend.Name, outcomeCanceled, start)
			return nil, &Error{Backend: backend.Name, Status: resp.StatusCode, Transient: false, Err: ctx.Err()}
		}
		c.observe(backend.Name, outcomeTransientError, start)
		return nil, &Error{Backend: backend.Name, Status: resp.StatusCode, Transient: true, Err: readErr}
	}
	if closeErr != nil {
		c.observe(backend.Name, outcomeTransientError, start)
		return nil, &Error{Backend: backend.Name, Status: resp.StatusCode, Transient: true, Err: closeErr}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		c.observe(backend.Name, outcomeSuccess, start)
		return &Response{StatusCode: resp.StatusCode, Body: respBody}, nil
	case resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout:
		c.observe(backend.Name, outcomeTransientError, start)
		return nil, &Error{Backend: backend.Name, Status: resp.StatusCode, Transient: true}
	default:
		c.observe(backend.Name, outcomePermanentError, start)
		return nil, &Error{Backend: backend.Name, Status: resp.StatusCode, Transient: false}
	}
}

// backoff returns the full-jitter wait before the retry that follows the
// n-th failed attempt (n ≥ 1): uniform in [0, min(BackoffMax, BackoffBase·2ⁿ⁻¹)].
func (c *Client) backoff(n int) time.Duration {
	if c.backoffBase <= 0 {
		return 0
	}
	ceil := c.backoffMax
	// Guard the shift: past 62 bits (or on overflow to <= 0) the exponential
	// has long exceeded any sane BackoffMax.
	if shift := n - 1; shift < 63 {
		if exp := c.backoffBase << shift; exp > 0 && exp < ceil {
			ceil = exp
		}
	}
	if ceil <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Duration(c.rng.Int63n(int64(ceil) + 1))
}

// observe records one attempt's outcome and duration when metrics are wired.
func (c *Client) observe(backend, outcome string, start time.Time) {
	if c.metrics == nil {
		return
	}
	c.metrics.BackendRequestsTotal.WithLabelValues(backend, outcome).Inc()
	c.metrics.BackendRequestDurationSeconds.WithLabelValues(backend).Observe(time.Since(start).Seconds())
}

// sleepContext waits d or until ctx is done, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
