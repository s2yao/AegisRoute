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

// fakeSelector returns candidates in order, honoring the exclude set so
// failover picks a fresh backend each call. It records the models requested,
// the exclude args, and how many times release ran.
type fakeSelector struct {
	selections []routing.Selection // candidate order
	err        error

	mu       sync.Mutex
	calls    []string
	excludes [][]uuid.UUID
	released int
}

func (f *fakeSelector) Select(_ context.Context, model string, exclude ...uuid.UUID) (routing.Selection, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, model)
	f.excludes = append(f.excludes, append([]uuid.UUID(nil), exclude...))
	if f.err != nil {
		return routing.Selection{}, nil, f.err
	}
	excluded := map[uuid.UUID]bool{}
	for _, id := range exclude {
		excluded[id] = true
	}
	for _, sel := range f.selections {
		if !excluded[sel.Backend.ID] {
			return sel, func() {
				f.mu.Lock()
				f.released++
				f.mu.Unlock()
			}, nil
		}
	}
	return routing.Selection{}, nil, routing.ErrNoCapacity
}

func (f *fakeSelector) releaseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// fakeInference returns a scripted response or error and records the last
// body and backend it was handed.
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

// inferenceFunc adapts a function to the InferenceDoer interface, for tests
// that need per-backend behavior (failover) or a panic.
type inferenceFunc func(context.Context, models.ModelBackend, []byte) (*inference.Response, error)

func (f inferenceFunc) Do(ctx context.Context, b models.ModelBackend, body []byte) (*inference.Response, error) {
	return f(ctx, b, body)
}

// fakeCircuit records reported outcomes.
type fakeCircuit struct {
	mu        sync.Mutex
	successes []string
	failures  []string
	canceled  []string
}

func (f *fakeCircuit) ReportSuccess(backend string)  { f.record(&f.successes, backend) }
func (f *fakeCircuit) ReportFailure(backend string)  { f.record(&f.failures, backend) }
func (f *fakeCircuit) ReportCanceled(backend string) { f.record(&f.canceled, backend) }

func (f *fakeCircuit) record(dst *[]string, backend string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, backend)
}

func (f *fakeCircuit) snapshot() (succ, fail, canc []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := func(s []string) []string { return append([]string(nil), s...) }
	return cp(f.successes), cp(f.failures), cp(f.canceled)
}

// fakeLedger is a synchronous LedgerRecorder: Record appends immediately so
// tests can assert right after the request (the real AsyncLedger is exercised
// in ledger_test.go).
type fakeLedger struct {
	mu   sync.Mutex
	rows []models.InferenceRequest
}

func (l *fakeLedger) Record(row models.InferenceRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
}

func (l *fakeLedger) all() []models.InferenceRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]models.InferenceRequest(nil), l.rows...)
}

// chatFixture bundles the chat fakes wired into a router.
type chatFixture struct {
	deps      api.Deps
	handler   http.Handler
	selector  *fakeSelector
	inference *fakeInference
	circuit   *fakeCircuit
	ledger    *fakeLedger
	backend   models.ModelBackend
}

const upstreamJSON = `{"id":"chatcmpl-abc","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`

// testBackend builds an enabled backend with the given name/priority.
func testBackend(name string, priority int) models.ModelBackend {
	return models.ModelBackend{
		ID:        uuid.New(),
		Name:      name,
		BaseURL:   "http://" + name + ":8081",
		ModelName: "llama-fast",
		Kind:      models.BackendKindMock,
		Enabled:   true, Priority: priority, Weight: 1, MaxInFlight: 4,
	}
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	backend := testBackend("mock-llm-fast", 10)
	f := &chatFixture{
		selector: &fakeSelector{selections: []routing.Selection{{
			Backend:    backend,
			PolicyName: "default",
			Strategy:   models.StrategyPriorityWeighted,
		}}},
		inference: &fakeInference{resp: &inference.Response{StatusCode: http.StatusOK, Body: []byte(upstreamJSON)}},
		circuit:   &fakeCircuit{},
		ledger:    &fakeLedger{},
		backend:   backend,
	}
	f.deps = testDeps()
	f.deps.Selector = f.selector
	f.deps.Inference = f.inference
	f.deps.Circuit = f.circuit
	f.deps.Ledger = f.ledger
	f.handler = api.NewRouter(f.deps)
	return f
}

// rebuild rewires the router after a test overrides a dep on f.deps.
func (f *chatFixture) rebuild() { f.handler = api.NewRouter(f.deps) }

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
	assert.Equal(t, 1, f.selector.releaseCount(), "the in-flight slot is released exactly once")
	succ, fail, _ := f.circuit.snapshot()
	assert.Equal(t, []string{"mock-llm-fast"}, succ)
	assert.Empty(t, fail)
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

	rows := f.ledger.all()
	require.Len(t, rows, 1)
	row := rows[0]
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

// --- upstream failures -------------------------------------------------------

func TestChatCompletionsTransientUpstreamFailure(t *testing.T) {
	// Single backend that fails transiently: no failover target exists, so the
	// selector returns ErrNoCapacity on the retry and the request ends 503.
	f := newChatFixture(t)
	f.inference.resp = nil
	f.inference.err = &inference.Error{Backend: "mock-llm-fast", Status: 503, Transient: true}

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "upstream_unavailable", env.Error.Code)

	_, fail, _ := f.circuit.snapshot()
	assert.Equal(t, []string{"mock-llm-fast"}, fail, "a transient failure counts against the circuit")
	assert.Equal(t, 1, f.selector.releaseCount())

	rows := f.ledger.all()
	require.Len(t, rows, 1)
	assert.Equal(t, models.RequestStatusFailed, rows[0].Status)
}

func TestChatCompletionsPermanentUpstreamFailure(t *testing.T) {
	f := newChatFixture(t)
	f.inference.resp = nil
	f.inference.err = &inference.Error{Backend: "mock-llm-fast", Status: 400, Transient: false}

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, "upstream_unavailable", env.Error.Code)

	succ, fail, _ := f.circuit.snapshot()
	assert.Empty(t, fail, "a permanent upstream error proves the backend is alive — no circuit failure")
	assert.Equal(t, []string{"mock-llm-fast"}, succ)
	assert.Equal(t, []string{"llama-fast"}, f.selector.calls, "a permanent error never fails over")
	rows := f.ledger.all()
	require.Len(t, rows, 1)
	assert.Equal(t, models.RequestStatusFailed, rows[0].Status)
}

func TestChatCompletionsClientCancellation(t *testing.T) {
	// The client disconnects mid-call: inference cancels the request context
	// and fails. The circuit gets a verdict-free canceled report (not a failure
	// that could open it), no failover is attempted, and the audit row is still
	// recorded.
	f := newChatFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		cancel()
		return nil, &inference.Error{Backend: "mock-llm-fast", Transient: false, Err: context.Canceled}
	})
	f.rebuild()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validChatBody)).
		WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	rec := do(t, f.handler, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	succ, fail, canc := f.circuit.snapshot()
	assert.Equal(t, []string{"mock-llm-fast"}, canc)
	assert.Empty(t, fail, "a client disconnect must never count against the backend")
	assert.Empty(t, succ)
	assert.Equal(t, []string{"llama-fast"}, f.selector.calls, "cancellation ends the request, no failover")

	rows := f.ledger.all()
	require.Len(t, rows, 1, "the ledger row is recorded even when the client left")
	assert.Equal(t, models.RequestStatusFailed, rows[0].Status)
}

// --- intra-request failover (#1) ---------------------------------------------

func TestChatCompletionsFailsOverOnTransient(t *testing.T) {
	// Backend A fails transiently, B succeeds: the same request must succeed on
	// B, reporting a failure for A and a success for B, releasing both slots.
	fast := testBackend("be-fast", 10)
	cheap := testBackend("be-cheap", 20)
	f := newChatFixture(t)
	f.selector.selections = []routing.Selection{
		{Backend: fast, PolicyName: "default", Strategy: models.StrategyPriorityWeighted},
		{Backend: cheap, PolicyName: "default", Strategy: models.StrategyPriorityWeighted},
	}
	f.deps.Inference = inferenceFunc(func(_ context.Context, b models.ModelBackend, _ []byte) (*inference.Response, error) {
		if b.Name == "be-fast" {
			return nil, &inference.Error{Backend: b.Name, Status: 503, Transient: true}
		}
		return &inference.Response{StatusCode: 200, Body: []byte(upstreamJSON)}, nil
	})
	f.rebuild()

	rec := f.postChat(t, validChatBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "be-cheap", rec.Header().Get("X-AegisRoute-Backend"), "the response came from the failover backend")

	succ, fail, _ := f.circuit.snapshot()
	assert.Equal(t, []string{"be-fast"}, fail, "the failing backend is reported failed")
	assert.Equal(t, []string{"be-cheap"}, succ, "the failover backend is reported succeeded")
	assert.Equal(t, 2, f.selector.releaseCount(), "both reserved slots are released")

	require.Len(t, f.selector.excludes, 2)
	assert.Empty(t, f.selector.excludes[0], "first Select excludes nothing")
	assert.Equal(t, []uuid.UUID{fast.ID}, f.selector.excludes[1], "the retry excludes the failed backend")

	rows := f.ledger.all()
	require.Len(t, rows, 2, "both attempts are audited")
	assert.Equal(t, models.RequestStatusFailed, rows[0].Status)
	assert.Equal(t, models.RequestStatusSucceeded, rows[1].Status)
}

func TestChatCompletionsFailoverExhausted(t *testing.T) {
	// Every backend fails transiently: after trying all of them the selector
	// runs out of fresh candidates and the request ends 503.
	fast := testBackend("be-fast", 10)
	cheap := testBackend("be-cheap", 20)
	f := newChatFixture(t)
	f.selector.selections = []routing.Selection{
		{Backend: fast, PolicyName: "default", Strategy: models.StrategyPriorityWeighted},
		{Backend: cheap, PolicyName: "default", Strategy: models.StrategyPriorityWeighted},
	}
	f.deps.Inference = inferenceFunc(func(_ context.Context, b models.ModelBackend, _ []byte) (*inference.Response, error) {
		return nil, &inference.Error{Backend: b.Name, Status: 503, Transient: true}
	})
	f.rebuild()

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "upstream_unavailable", decodeError(t, rec.Body).Error.Code)

	_, fail, _ := f.circuit.snapshot()
	assert.ElementsMatch(t, []string{"be-fast", "be-cheap"}, fail, "both backends were tried and reported failed")
	assert.Equal(t, 2, f.selector.releaseCount(), "every reserved slot is released")
	assert.Len(t, f.ledger.all(), 2)
}

// --- probe/slot cleanup on panic (#4) ----------------------------------------

func TestChatCompletionsInferencePanicCleansUp(t *testing.T) {
	// If the inference call panics, the recover middleware returns 500, but the
	// reserved in-flight slot and the circuit's half-open probe must still be
	// released so the backend can't get stuck.
	f := newChatFixture(t)
	f.deps.Inference = inferenceFunc(func(context.Context, models.ModelBackend, []byte) (*inference.Response, error) {
		panic("backend client blew up")
	})
	f.rebuild()

	rec := f.postChat(t, validChatBody)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal", decodeError(t, rec.Body).Error.Code)

	assert.Equal(t, 1, f.selector.releaseCount(), "the in-flight slot is freed even on panic")
	_, _, canc := f.circuit.snapshot()
	assert.Equal(t, []string{"mock-llm-fast"}, canc,
		"the half-open probe is released verdict-free on panic, not left reserved")
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
			assert.Empty(t, f.ledger.all(), "no ledger row before a backend was chosen")
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
			assert.Empty(t, f.ledger.all())
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
