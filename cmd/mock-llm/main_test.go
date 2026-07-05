package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T, cfg config) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler(cfg, rand.New(rand.NewSource(1))))
	t.Cleanup(srv.Close)
	return srv
}

func defaultConfig() config {
	return config{backendName: "mock-llm-fast", modelName: "llama-fast"}
}

func post(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, b
}

const validBody = `{"model":"llama-fast","messages":[{"role":"user","content":"hello mock world"}]}`

func TestCompletionsDeterministic(t *testing.T) {
	srv := newTestServer(t, defaultConfig())

	resp1, body1 := post(t, srv, validBody)
	resp2, body2 := post(t, srv, validBody)
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, body1, body2, "identical input must produce byte-identical output")

	_, other := post(t, srv, `{"model":"llama-fast","messages":[{"role":"user","content":"different"}]}`)
	assert.NotEqual(t, body1, other, "different input must produce a different completion")
}

func TestCompletionsShapeAndUsage(t *testing.T) {
	srv := newTestServer(t, defaultConfig())
	resp, body := post(t, srv, validBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"))

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(body, &out))

	assert.True(t, strings.HasPrefix(out.ID, "chatcmpl-"), "id %q", out.ID)
	assert.Equal(t, "chat.completion", out.Object)
	assert.EqualValues(t, mockCreated, out.Created)
	assert.Equal(t, "llama-fast", out.Model)
	require.Len(t, out.Choices, 1)
	assert.Equal(t, "assistant", out.Choices[0].Message.Role)
	assert.NotEmpty(t, out.Choices[0].Message.Content)
	assert.Equal(t, "stop", out.Choices[0].FinishReason)
	assert.Equal(t, 3, out.Usage.PromptTokens, "three words in the prompt")
	assert.Positive(t, out.Usage.CompletionTokens)
	assert.Equal(t, out.Usage.PromptTokens+out.Usage.CompletionTokens, out.Usage.TotalTokens)
}

func TestCompletionsFailureRateOne(t *testing.T) {
	cfg := defaultConfig()
	cfg.failureRate = 1
	srv := newTestServer(t, cfg)

	for range 5 {
		resp, body := post(t, srv, validBody)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Contains(t, string(body), "injected failure")
	}
}

func TestCompletionsFailureRateZeroNeverFails(t *testing.T) {
	srv := newTestServer(t, defaultConfig())
	for range 5 {
		resp, _ := post(t, srv, validBody)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

func TestCompletionsRejectsBadRequests(t *testing.T) {
	srv := newTestServer(t, defaultConfig())
	cases := map[string]string{
		"invalid JSON":     `{not json`,
		"missing model":    `{"messages":[{"role":"user","content":"hi"}]}`,
		"missing messages": `{"model":"llama-fast"}`,
		"empty messages":   `{"model":"llama-fast","messages":[]}`,
	}
	for name, body := range cases {
		resp, respBody := post(t, srv, body)
		assert.Equalf(t, http.StatusBadRequest, resp.StatusCode, "%s", name)
		assert.Containsf(t, string(respBody), "error", "%s: OpenAI-style error body", name)
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, defaultConfig())
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "ok", out["status"])
}

func TestMethodAndPathRouting(t *testing.T) {
	srv := newTestServer(t, defaultConfig())

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	resp, err = http.Post(srv.URL+"/nope", "application/json", bytes.NewReader(nil))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, 8081, cfg.port)
	assert.Equal(t, float64(0), cfg.failureRate)
	assert.Equal(t, "mock-llm", cfg.backendName)
	assert.Equal(t, "llama-fast", cfg.modelName)

	t.Setenv("MOCK_FAILURE_RATE", "1.5")
	_, err = loadConfig()
	assert.Error(t, err, "failure rate above 1 is rejected")

	t.Setenv("MOCK_FAILURE_RATE", "0.5")
	t.Setenv("MOCK_PORT", "not-a-port")
	_, err = loadConfig()
	assert.Error(t, err, "non-integer port is rejected")
}
