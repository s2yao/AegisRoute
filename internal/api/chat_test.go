package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/routing"
)

// --- chat fakes --------------------------------------------------------------

// fakeSelector returns a fixed Selection (or error) and records calls and
// releases.
type fakeSelector struct {
	selection routing.Selection
	err       error

	mu       sync.Mutex
	calls    []string
	released int
}

func (f *fakeSelector) Select(_ context.Context, model string) (routing.Selection, func(), error) {
	f.mu.Lock()
	f.calls = append(f.calls, model)
	f.mu.Unlock()
	if f.err != nil {
		return routing.Selection{}, nil, f.err
	}
	return f.selection, func() {
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
	}, nil
}

// fakeInference returns a scripted response or error and records the body
// and backend it was handed.
type fakeInference struct {
	resp *inference.Response
	err  error

	gotBackend models.ModelBackend
	gotBody    []byte
}

func (f *fakeInference) Do(_ context.Context, backend models.ModelBackend, body []byte) (*inference.Response, error) {
	f.gotBackend = backend
	f.gotBody = body
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// fakeCircuit records reported outcomes.
type fakeCircuit struct {
	successes []string
	failures  []string
	canceled  []string
}

func (f *fakeCircuit) ReportSuccess(backend string)  { f.successes = append(f.successes, backend) }
func (f *fakeCircuit) ReportFailure(backend string)  { f.failures = append(f.failures, backend) }
func (f *fakeCircuit) ReportCanceled(backend string) { f.canceled = append(f.canceled, backend) }

// fakeRequestLog records inserted ledger rows (and the liveness of the
// context each insert arrived on) and can be primed to fail.
type fakeRequestLog struct {
	rows    []models.InferenceRequest
	ctxErrs []error
	err     error
}

func (f *fakeRequestLog) Insert(ctx context.Context, row models.InferenceRequest) (models.InferenceRequest, error) {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.err != nil {
		return models.InferenceRequest{}, f.err
	}
	row.ID = uuid.New()
	f.rows = append(f.rows, row)
	return row, nil
}

// chatFixture bundles the chat fakes wired into a router.
type chatFixture struct {
	deps      api.Deps
	handler   http.Handler
	selector  *fakeSelector
	inference *fakeInference
	circuit   *fakeCircuit
	requests  *fakeRequestLog
	backend   models.ModelBackend
}

const upstreamJSON = `{"id":"chatcmpl-abc","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	backend := models.ModelBackend{
		ID:        uuid.New(),
		Name:      "mock-llm-fast",
		BaseURL:   "http://mock-llm-fast:8081",
		ModelName: "llama-fast",
		Kind:      models.BackendKindMock,
		Enabled:   true, Priority: 10, Weight: 1, MaxInFlight: 4,
	}
	f := &chatFixture{
		selector: &fakeSelector{selection: routing.Selection{
			Backend:    backend,
			PolicyName: "default",
			Strategy:   models.StrategyPriorityWeighted,
		}},
		inference: &fakeInference{resp: &inference.Response{StatusCode: http.StatusOK, Body: []byte(upstreamJSON)}},
		circuit:   &fakeCircuit{},
		requests:  &fakeRequestLog{},
		backend:   backend,
	}
	f.deps = testDeps()
	f.deps.Selector = f.selector
	f.deps.Inference = f.inference
	f.deps.Circuit = f.circuit
	f.deps.Requests = f.requests
	f.handler = api.NewRouter(f.deps)
	return f
}

// postChat sends an authenticated POST /v1/chat/completions with the body.
func (f *chatFixture) postChat(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	req.Header.Set("Content-Type", "application/json")
	return do(t, f.handler, req)
}

const validChatBody = `{"model":"llama-fast","messages":[{"role":"user","content":"hello"}]}`

// --- happy path --------------------------------------------------------------

func TestChatCompletionsHappyPath(t *testing.T) {
	f := newChatFixture(t)
	rec := f.postChat(t, validChatBody)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, upstreamJSON, rec.Body.String(), "the backend's OpenAI JSON is returned as-is")
	assert.Equal(t, "mock-llm-fast", rec.Header().Get("X-AegisRoute-Backend"))
	assert.Equal(t, "default", rec.Header().Get("X-AegisRoute-Routing-Policy"))
	assert.Empty(t, rec.Header().Get("X-AegisRoute-Cache"), "the cache header arrives in Stage 5, not before")

	assert.Equal(t, []string{"llama-fast"}, f.selector.calls)
	assert.Equal(t, 1, f.selector.released, "the in-flight slot is released exactly once")
	assert.Equal(t, []string{"mock-llm-fast"}, f.circuit.successes)
	assert.Empty(t, f.circuit.failures)
	assert.Equal(t, f.backend.ID, f.inference.gotBackend.ID, "the selected backend receives the call")
}

func TestChatCompletionsForwardsCanonicalBody(t *testing.T) {
	f := newChatFixture(t)
	rec := f.postChat(t, `{"model":"llama-fast","stream":false,"temperature":0.5,`+
		`"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}],"stop":"END"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var forwarded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.inference.gotBody, &forwarded))
	assert.NotContains(t, forwarded, "stream", "stream is never forwarded upstream")
	assert.NotContains(t, forwarded, "max_tokens", "omitted optionals stay omitted")
	assert.JSONEq(t, `0.5`, string(forwarded["temperature"]))
	assert.JSONEq(t, `["END"]`, string(forwarded["stop"]), "a bare stop string is normalized to a one-element array")
	assert.JSONEq(t, `"llama-fast"`, string(forwarded["model"]))
}

func TestChatCompletionsPersistsLedgerRow(t *testing.T) {
	f := newChatFixture(t)
	rec := f.postChat(t, validChatBody)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, f.requests.rows, 1)
	row := f.requests.rows[0]
	assert.Equal(t, models.RequestStatusSucceeded, row.Status)
	assert.Equal(t, "llama-fast", row.Model)
	require.NotNil(t, row.BackendID)
	assert.Equal(t, f.backend.ID, *row.BackendID)
	assert.Nil(t, row.CacheResult, "cache results do not exist until Stage 5")
	assert.GreaterOrEqual(t, row.LatencyMS, 0)
	assert.Len(t, row.RequestHash, 64, "SHA-256 hex of the canonical body")
	assert.NotEqual(t, uuid.Nil, row.TenantID)
	assert.NotEqual(t, uuid.Nil, row.APIKeyID)
}

func TestChatCompletionsLedgerFailureDoesNotFailRequest(t *testing.T) {
	f := newChatFixture(t)
	f.requests.err = errors.New("pg down")
	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a ledger write failure must never turn a served completion into an error")
}

// --- upstream failures -------------------------------------------------------

func TestChatCompletionsTransientUpstreamFailure(t *testing.T) {
	f := newChatFixture(t)
	f.inference.resp = nil
	f.inference.err = &inference.Error{Backend: "mock-llm-fast", Status: 503, Transient: true}

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "upstream_unavailable", env.Error.Code)

	assert.Equal(t, []string{"mock-llm-fast"}, f.circuit.failures,
		"a transient failure counts against the circuit")
	assert.Empty(t, f.circuit.successes)
	assert.Equal(t, 1, f.selector.released)

	require.Len(t, f.requests.rows, 1)
	assert.Equal(t, models.RequestStatusFailed, f.requests.rows[0].Status)
}

func TestChatCompletionsPermanentUpstreamFailure(t *testing.T) {
	f := newChatFixture(t)
	f.inference.resp = nil
	f.inference.err = &inference.Error{Backend: "mock-llm-fast", Status: 400, Transient: false}

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "upstream_unavailable", env.Error.Code)

	assert.Empty(t, f.circuit.failures,
		"a permanent upstream error proves the backend is alive — no circuit failure")
	assert.Equal(t, []string{"mock-llm-fast"}, f.circuit.successes)
	require.Len(t, f.requests.rows, 1)
	assert.Equal(t, models.RequestStatusFailed, f.requests.rows[0].Status)
}

func TestChatCompletionsClientCancellation(t *testing.T) {
	// The client disconnects mid-call: the fake inference cancels the request
	// context and fails. The circuit must get a verdict-free canceled report
	// (not a failure that could open it), and the audit row must still be
	// written — on a context detached from the dead request.
	f := newChatFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		cancel()
		return nil, &inference.Error{Backend: "mock-llm-fast", Transient: false, Err: context.Canceled}
	})
	f.handler = api.NewRouter(f.deps)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validChatBody)).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	rec := do(t, f.handler, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, []string{"mock-llm-fast"}, f.circuit.canceled)
	assert.Empty(t, f.circuit.failures, "a client disconnect must never count against the backend")
	assert.Empty(t, f.circuit.successes)

	require.Len(t, f.requests.rows, 1, "the ledger row survives the disconnect")
	assert.Equal(t, models.RequestStatusFailed, f.requests.rows[0].Status)
	require.Len(t, f.requests.ctxErrs, 1)
	assert.NoError(t, f.requests.ctxErrs[0], "the insert context must be detached from the canceled request")
}

// inferenceFunc adapts a function to the InferenceDoer interface.
type inferenceFunc func(context.Context, models.ModelBackend, []byte) (*inference.Response, error)

func (f inferenceFunc) Do(ctx context.Context, b models.ModelBackend, body []byte) (*inference.Response, error) {
	return f(ctx, b, body)
}

// --- routing failures --------------------------------------------------------

func TestChatCompletionsRoutingErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unknown model", routing.ErrNoBackends, http.StatusNotFound, "not_found"},
		{"no capacity", routing.ErrNoCapacity, http.StatusServiceUnavailable, "upstream_unavailable"},
		{"store failure", errors.New("pg down"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newChatFixture(t)
			f.selector.err = tc.err
			rec := f.postChat(t, validChatBody)
			assert.Equal(t, tc.wantStatus, rec.Code)
			env := decodeError(t, rec.Body)
			assert.Equal(t, tc.wantCode, env.Error.Code)
			assert.Empty(t, f.requests.rows, "no ledger row before a backend was chosen")
		})
	}
}

// --- validation --------------------------------------------------------------

func TestChatCompletionsValidation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"stream true", `{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "unsupported_streaming"},
		{"missing model", `{"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"blank model", `{"model":"  ","messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"missing messages", `{"model":"m"}`,
			http.StatusBadRequest, "bad_request"},
		{"empty messages", `{"model":"m","messages":[]}`,
			http.StatusBadRequest, "bad_request"},
		{"bad role", `{"model":"m","messages":[{"role":"robot","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"empty content", `{"model":"m","messages":[{"role":"user","content":""}]}`,
			http.StatusBadRequest, "bad_request"},
		{"non-string content", `{"model":"m","messages":[{"role":"user","content":[{"type":"text"}]}]}`,
			http.StatusBadRequest, "bad_request"},
		{"temperature too low", `{"model":"m","temperature":-0.1,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"temperature too high", `{"model":"m","temperature":2.1,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"max_tokens zero", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"max_tokens negative", `{"model":"m","max_tokens":-5,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"unknown top-level field", `{"model":"m","tools":[],"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"unknown message field", `{"model":"m","messages":[{"role":"user","content":"x","name":"bob"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"case-variant top-level field", `{"MODEL":"m","messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"case-variant stream field", `{"model":"m","Stream":true,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"case-variant message field", `{"model":"m","messages":[{"Role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"non-boolean stream", `{"model":"m","stream":"yes","messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"empty stop string", `{"model":"m","stop":"","messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"empty stop array", `{"model":"m","stop":[],"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"stop array with empty string", `{"model":"m","stop":["a",""],"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"stop wrong type", `{"model":"m","stop":42,"messages":[{"role":"user","content":"x"}]}`,
			http.StatusBadRequest, "bad_request"},
		{"malformed JSON", `{"model":`,
			http.StatusBadRequest, "bad_request"},
		{"not an object", `[1,2,3]`,
			http.StatusBadRequest, "bad_request"},
		{"trailing garbage", validChatBody + `{"again":true}`,
			http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newChatFixture(t)
			rec := f.postChat(t, tc.body)
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			env := decodeError(t, rec.Body)
			assert.Equal(t, tc.wantCode, env.Error.Code)
			assert.NotEmpty(t, env.Error.RequestID)
			assert.Empty(t, f.selector.calls, "invalid requests must never reach routing")
			assert.Empty(t, f.requests.rows)
		})
	}
}

func TestChatCompletionsValidBoundaryValues(t *testing.T) {
	// temperature 0 and 2 and explicit max_tokens are all inside the range;
	// pointers keep the explicit 0 distinguishable from omitted.
	f := newChatFixture(t)
	rec := f.postChat(t, `{"model":"m","temperature":0,"max_tokens":1,`+
		`"messages":[{"role":"user","content":"x"}],"stop":["a","b"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var forwarded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.inference.gotBody, &forwarded))
	assert.JSONEq(t, `0`, string(forwarded["temperature"]), "explicit 0 is forwarded, not dropped")

	f2 := newChatFixture(t)
	rec = f2.postChat(t, `{"model":"m","temperature":2,"messages":[{"role":"user","content":"x"}]}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestChatCompletionsBodyOverOneMiB(t *testing.T) {
	f := newChatFixture(t)
	huge := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"%s"}]}`,
		strings.Repeat("a", maxBodyProbeSize))
	rec := f.postChat(t, huge)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "bad_request", env.Error.Code)
	assert.Empty(t, f.selector.calls)
}

// maxBodyProbeSize pads the content field past the 1 MiB cap.
const maxBodyProbeSize = 1 << 20

func TestChatCompletionsRequiresBearerAuth(t *testing.T) {
	f := newChatFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validChatBody))
	rec := do(t, f.handler, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "unauthorized", env.Error.Code)
}
