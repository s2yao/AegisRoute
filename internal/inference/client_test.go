package inference

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
)

// trackingBody wraps a response body and records whether Close was called,
// so tests can prove no path leaks a body.
type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

// step is one scripted transport turn: either an HTTP status to answer with
// or an error to fail with.
type step struct {
	status int
	body   string
	err    error
}

// scriptedTransport plays back steps in order, recording every request and
// the tracking body it handed out. The last step repeats if the script runs
// out. Safe for concurrent use, though tests here call it sequentially.
type scriptedTransport struct {
	mu     sync.Mutex
	steps  []step
	calls  []*http.Request
	bodies []*trackingBody
}

func (t *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, req)
	s := t.steps[min(len(t.calls)-1, len(t.steps)-1)]
	if s.err != nil {
		return nil, s.err
	}
	body := &trackingBody{Reader: bytes.NewReader([]byte(s.body))}
	t.bodies = append(t.bodies, body)
	return &http.Response{
		StatusCode: s.status,
		Body:       body,
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (t *scriptedTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

func (t *scriptedTransport) assertAllBodiesClosed(test *testing.T) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, b := range t.bodies {
		assert.Truef(test, b.closed, "response body %d must be closed", i)
	}
}

// sleepRecorder captures backoff waits instead of sleeping.
type sleepRecorder struct {
	slept []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.slept = append(s.slept, d)
	return ctx.Err()
}

func testBackend() models.ModelBackend {
	return models.ModelBackend{Name: "mock-llm-fast", BaseURL: "http://mock-llm-fast:8081/"}
}

// newTestClient builds a Client over the scripted transport with fast,
// deterministic retry settings and a recorded sleep.
func newTestClient(t *scriptedTransport, rec *sleepRecorder) *Client {
	return New(Config{
		HTTPClient:  &http.Client{Transport: t},
		Timeout:     time.Second,
		MaxAttempts: 3,
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
		Metrics:     metrics.New(),
		Rand:        rand.New(rand.NewSource(1)),
		Sleep:       rec.sleep,
	})
}

func TestDoSucceedsAfterTransientRetry(t *testing.T) {
	transport := &scriptedTransport{steps: []step{
		{status: http.StatusServiceUnavailable, body: `{"error":"down"}`},
		{status: http.StatusOK, body: `{"id":"chatcmpl-1"}`},
	}}
	rec := &sleepRecorder{}
	c := newTestClient(transport, rec)

	resp, err := c.Do(context.Background(), testBackend(), []byte(`{"model":"m"}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"id":"chatcmpl-1"}`, string(resp.Body))
	assert.Equal(t, 2, transport.callCount(), "503 then 200 → exactly one retry")
	assert.Len(t, rec.slept, 1, "one backoff wait between the two attempts")
	transport.assertAllBodiesClosed(t)
}

func TestDoRetriesEachTransientStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		transport := &scriptedTransport{steps: []step{
			{status: status, body: "upstream sad"},
			{status: http.StatusOK, body: "ok"},
		}}
		c := newTestClient(transport, &sleepRecorder{})
		_, err := c.Do(context.Background(), testBackend(), nil)
		require.NoErrorf(t, err, "status %d must be retried", status)
		assert.Equal(t, 2, transport.callCount(), "status %d", status)
		transport.assertAllBodiesClosed(t)
	}
}

func TestDoNeverRetriesPermanentStatuses(t *testing.T) {
	// 400/401/403/404 are the spec'd never-retry statuses; 500 is also
	// permanent because the transient set is exactly timeout/conn/502/503/504.
	for _, status := range []int{400, 401, 403, 404, 500} {
		transport := &scriptedTransport{steps: []step{{status: status, body: "no"}}}
		c := newTestClient(transport, &sleepRecorder{})

		_, err := c.Do(context.Background(), testBackend(), nil)
		require.Errorf(t, err, "status %d", status)
		assert.Equal(t, 1, transport.callCount(), "status %d must not be retried", status)
		assert.False(t, IsTransient(err), "status %d is permanent", status)

		var infErr *Error
		require.ErrorAs(t, err, &infErr)
		assert.Equal(t, status, infErr.Status)
		assert.Equal(t, "mock-llm-fast", infErr.Backend)
		transport.assertAllBodiesClosed(t)
	}
}

func TestDoTransientExhaustionReturnsTypedError(t *testing.T) {
	transport := &scriptedTransport{steps: []step{{status: http.StatusServiceUnavailable, body: "down"}}}
	rec := &sleepRecorder{}
	c := newTestClient(transport, rec)

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	assert.True(t, IsTransient(err))
	assert.Equal(t, 3, transport.callCount(), "MaxAttempts=3 → three attempts")
	assert.Len(t, rec.slept, 2)
	transport.assertAllBodiesClosed(t)
}

func TestDoRetriesTimeout(t *testing.T) {
	// A per-attempt timeout surfaces as an error from the transport (the
	// attempt context expires while the request is in flight).
	transport := &scriptedTransport{steps: []step{
		{err: context.DeadlineExceeded},
		{status: http.StatusOK, body: "ok"},
	}}
	c := newTestClient(transport, &sleepRecorder{})

	resp, err := c.Do(context.Background(), testBackend(), nil)
	require.NoError(t, err, "a timed-out attempt must be retried")
	assert.Equal(t, "ok", string(resp.Body))
	assert.Equal(t, 2, transport.callCount())
}

func TestDoAttemptTimeoutEnforced(t *testing.T) {
	// The transport honors the request context, simulating a hung backend;
	// the per-attempt timeout must cancel it and the client must retry.
	hung := &hangingTransport{}
	rec := &sleepRecorder{}
	c := New(Config{
		HTTPClient:  &http.Client{Transport: hung},
		Timeout:     10 * time.Millisecond,
		MaxAttempts: 2,
		BackoffBase: time.Millisecond,
		BackoffMax:  time.Millisecond,
		Rand:        rand.New(rand.NewSource(1)),
		Sleep:       rec.sleep,
	})

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	assert.True(t, IsTransient(err))
	assert.Equal(t, 2, hung.calls, "each hung attempt times out and is retried until attempts run out")
}

// hangingTransport blocks until the request context is done.
type hangingTransport struct{ calls int }

func (h *hangingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.calls++
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestDoBackoffWithinBounds(t *testing.T) {
	transport := &scriptedTransport{steps: []step{{status: http.StatusServiceUnavailable}}}
	rec := &sleepRecorder{}
	base, max := 50*time.Millisecond, 120*time.Millisecond
	c := New(Config{
		HTTPClient:  &http.Client{Transport: transport},
		Timeout:     time.Second,
		MaxAttempts: 5,
		BackoffBase: base,
		BackoffMax:  max,
		Rand:        rand.New(rand.NewSource(99)),
		Sleep:       rec.sleep,
	})

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	require.Len(t, rec.slept, 4)

	// Full jitter: each wait is uniform in [0, min(max, base·2ⁿ⁻¹)].
	ceilings := []time.Duration{base, 100 * time.Millisecond, max, max}
	for i, d := range rec.slept {
		assert.GreaterOrEqualf(t, d, time.Duration(0), "backoff %d", i)
		assert.LessOrEqualf(t, d, ceilings[i], "backoff %d must stay under its exponential ceiling", i)
	}
}

func TestSleepContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	err := sleepContext(ctx, time.Hour)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second,
		"cancellation must abort the backoff wait, not sit out the timer")
}

func TestDoCancellationDuringBackoffAborts(t *testing.T) {
	// The injected sleep simulates a cancel arriving mid-backoff (the real
	// sleepContext's behavior is covered above): Do must stop retrying and
	// surface the context error as a NON-transient failure — the caller
	// leaving is not a verdict about the backend.
	transport := &scriptedTransport{steps: []step{{status: http.StatusServiceUnavailable}}}
	c := New(Config{
		HTTPClient:  &http.Client{Transport: transport},
		Timeout:     time.Second,
		MaxAttempts: 5,
		BackoffBase: time.Millisecond,
		BackoffMax:  time.Millisecond,
		Rand:        rand.New(rand.NewSource(1)),
		Sleep: func(context.Context, time.Duration) error {
			return context.Canceled
		},
	})

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	assert.False(t, IsTransient(err), "caller cancellation must not read as a backend failure")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, transport.callCount(), "no attempt may follow an aborted backoff")
	transport.assertAllBodiesClosed(t)
}

func TestDoTransportErrorWithDeadCallerContextIsCanceledNotTransient(t *testing.T) {
	// When the caller's own context is what killed the in-flight request,
	// the error must be non-transient (no retry, no circuit failure) even
	// though it surfaced as a transport error.
	ctx, cancel := context.WithCancel(context.Background())
	c := New(Config{
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
		Timeout:     time.Second,
		MaxAttempts: 5,
		BackoffBase: time.Millisecond,
		BackoffMax:  time.Millisecond,
		Rand:        rand.New(rand.NewSource(1)),
	})

	_, err := c.Do(ctx, testBackend(), nil)
	require.Error(t, err)
	assert.False(t, IsTransient(err))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBackoffZeroBaseYieldsZeroWait(t *testing.T) {
	transport := &scriptedTransport{steps: []step{{status: http.StatusServiceUnavailable}}}
	rec := &sleepRecorder{}
	c := New(Config{
		HTTPClient:  &http.Client{Transport: transport},
		Timeout:     time.Second,
		MaxAttempts: 3,
		BackoffBase: 0, // documented ceiling min(max, 0·2ⁿ⁻¹) is zero
		BackoffMax:  time.Hour,
		Rand:        rand.New(rand.NewSource(1)),
		Sleep:       rec.sleep,
	})

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	require.Len(t, rec.slept, 2)
	for i, d := range rec.slept {
		assert.Zerof(t, d, "backoff %d with zero base must be zero, not up to BackoffMax", i)
	}
}

func TestDoNoRetryAfterCallerContextEnds(t *testing.T) {
	// The transport cancels the caller's context while answering 503; the
	// retry loop must notice the dead context and stop after this attempt,
	// returning the transient failure it observed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &scriptedTransport{steps: []step{{status: http.StatusServiceUnavailable}}}
	rec := &sleepRecorder{}
	c := New(Config{
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := transport.RoundTrip(req)
			cancel()
			return resp, err
		})},
		Timeout:     time.Second,
		MaxAttempts: 5,
		BackoffBase: time.Millisecond,
		BackoffMax:  time.Millisecond,
		Rand:        rand.New(rand.NewSource(1)),
		Sleep:       rec.sleep,
	})

	_, err := c.Do(ctx, testBackend(), nil)
	require.Error(t, err)
	assert.True(t, IsTransient(err))
	assert.Equal(t, 1, transport.callCount())
	assert.Empty(t, rec.slept, "a dead context must not even enter backoff")
	transport.assertAllBodiesClosed(t)
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDoJoinsURLWithoutDoubleSlash(t *testing.T) {
	transport := &scriptedTransport{steps: []step{{status: http.StatusOK, body: "ok"}}}
	c := newTestClient(transport, &sleepRecorder{})

	_, err := c.Do(context.Background(), testBackend(), nil) // BaseURL has a trailing slash
	require.NoError(t, err)
	require.Equal(t, 1, transport.callCount())
	call := transport.calls[0]
	assert.Equal(t, "http://mock-llm-fast:8081/v1/chat/completions", call.URL.String())
	assert.Equal(t, http.MethodPost, call.Method)
	assert.Equal(t, "application/json", call.Header.Get("Content-Type"))
}

func TestDoForwardsRequestBody(t *testing.T) {
	transport := &scriptedTransport{steps: []step{{status: http.StatusOK, body: "ok"}}}
	c := newTestClient(transport, &sleepRecorder{})

	payload := []byte(`{"model":"llama-fast","messages":[]}`)
	_, err := c.Do(context.Background(), testBackend(), payload)
	require.NoError(t, err)
	sent, err := io.ReadAll(transport.calls[0].Body)
	require.NoError(t, err)
	assert.Equal(t, payload, sent)
}

func TestIsTransientOnForeignError(t *testing.T) {
	assert.False(t, IsTransient(errors.New("plain")))
	assert.False(t, IsTransient(nil))
}

// capClient builds a Client with a tiny response cap for the size-limit tests.
func capClient(t *scriptedTransport, maxBytes int64) *Client {
	return New(Config{
		HTTPClient:       &http.Client{Transport: t},
		Timeout:          time.Second,
		MaxAttempts:      3,
		BackoffBase:      time.Millisecond,
		BackoffMax:       2 * time.Millisecond,
		MaxResponseBytes: maxBytes,
		Metrics:          metrics.New(),
		Rand:             rand.New(rand.NewSource(1)),
		Sleep:            func(context.Context, time.Duration) error { return nil },
	})
}

func TestDoRejectsOversizedResponse(t *testing.T) {
	// Body of 11 bytes against a 10-byte cap: rejected as a permanent error,
	// not retried (the retry would just overrun again).
	transport := &scriptedTransport{steps: []step{{status: http.StatusOK, body: "12345678901"}}}
	c := capClient(transport, 10)

	_, err := c.Do(context.Background(), testBackend(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
	assert.False(t, IsTransient(err), "an oversized body is permanent, so it is not retried")
	assert.Equal(t, 1, transport.callCount(), "no retry on an oversized response")
	transport.assertAllBodiesClosed(t)
}

func TestDoAcceptsResponseAtExactlyTheCap(t *testing.T) {
	// Exactly at the cap is fine — only strictly larger is rejected.
	transport := &scriptedTransport{steps: []step{{status: http.StatusOK, body: "1234567890"}}}
	c := capClient(transport, 10)

	resp, err := c.Do(context.Background(), testBackend(), nil)
	require.NoError(t, err)
	assert.Equal(t, "1234567890", string(resp.Body))
	transport.assertAllBodiesClosed(t)
}

func TestNewDefaultsResponseCap(t *testing.T) {
	c := New(Config{})
	assert.Equal(t, int64(DefaultMaxResponseBytes), c.maxResponseBytes,
		"an unset cap falls back to the 10 MiB default, never unbounded")
}
