package cache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

func testRequest() Request {
	return Request{
		Model: "llama-fast",
		Messages: []Message{
			{Role: "system", Content: "be nice"},
			{Role: "user", Content: "hello"},
		},
		Temperature: f64(0),
	}
}

const testScope = "tenant-1:key-1"

// --- eligibility ---------------------------------------------------------------

func TestEligible(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want bool
	}{
		{"explicit zero temperature is cacheable", Request{Temperature: f64(0)}, true},
		{"temperature at the 0.2 boundary is cacheable", Request{Temperature: f64(0.2)}, true},
		{"temperature above 0.2 bypasses", Request{Temperature: f64(0.21)}, false},
		{"temperature 1.0 bypasses", Request{Temperature: f64(1.0)}, false},
		{"omitted temperature normalizes to the OpenAI default 1.0, not Go's zero", Request{}, false},
		{"stream true bypasses even at temperature 0", Request{Temperature: f64(0), Stream: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Eligible(tc.req))
		})
	}
}

// --- canonicalization + key -----------------------------------------------------

func TestCanonicalBodySortsKeysPreservesArrayOrder(t *testing.T) {
	req := testRequest()
	req.MaxTokens = iptr(5)
	req.Stop = []string{"z", "a"}
	body, err := CanonicalBody(req)
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"max_tokens": 5,
		"messages": [
			{"content":"be nice","role":"system"},
			{"content":"hello","role":"user"}
		],
		"model": "llama-fast",
		"stop": ["z","a"],
		"temperature": 0
	}`, string(body))

	// Byte-level: keys appear in sorted order, and the stop array keeps its
	// original (unsorted) order — array order is semantically meaningful.
	assert.Equal(t,
		`{"max_tokens":5,"messages":[{"content":"be nice","role":"system"},{"content":"hello","role":"user"}],"model":"llama-fast","stop":["z","a"],"temperature":0}`,
		string(body))
}

func TestCanonicalBodyOmitsAbsentOptionals(t *testing.T) {
	body, err := CanonicalBody(Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	require.NoError(t, err)
	assert.NotContains(t, string(body), "temperature", "omitted optionals stay omitted, never re-encoded as 0 or null")
	assert.NotContains(t, string(body), "max_tokens")
	assert.NotContains(t, string(body), "stop")
}

func TestKeyStableAcrossWireVariations(t *testing.T) {
	// Two wire forms of the same request: different field order, whitespace,
	// and volatile headers. Parsed into the canonical Request they must yield
	// identical keys — canonicalization happens on the parsed form, so wire
	// noise cannot influence the key.
	wireA := `{"model":"llama-fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	wireB := "{\n  \"temperature\": 0.0,\n  \"messages\": [ {\"content\":\"hi\",\"role\":\"user\"} ],\n  \"model\": \"llama-fast\"\n}"

	parse := func(s string) Request {
		var w struct {
			Model       string   `json:"model"`
			Temperature *float64 `json:"temperature"`
			Messages    []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal([]byte(s), &w))
		req := Request{Model: w.Model, Temperature: w.Temperature}
		for _, m := range w.Messages {
			req.Messages = append(req.Messages, Message{Role: m.Role, Content: m.Content})
		}
		return req
	}

	bodyA, err := CanonicalBody(parse(wireA))
	require.NoError(t, err)
	bodyB, err := CanonicalBody(parse(wireB))
	require.NoError(t, err)
	assert.Equal(t, string(bodyA), string(bodyB), "field order and whitespace must not change the canonical body")
	assert.Equal(t, Key(testScope, bodyA), Key(testScope, bodyB))
}

func TestKeyDependsOnlyOnScopeAndBody(t *testing.T) {
	body, err := CanonicalBody(testRequest())
	require.NoError(t, err)

	// Volatile headers are structurally excluded: Key has no header inputs at
	// all, so "changing" Idempotency-Key or Authorization cannot move the key.
	for _, volatile := range []string{"idem-key-1", "idem-key-2", "Bearer sg_other_key"} {
		_ = volatile // not an input — that is the point
		assert.Equal(t, Key(testScope, body), Key(testScope, body))
	}

	assert.NotEqual(t, Key(testScope, body), Key("tenant-2:key-2", body),
		"a different tenant/api-key scope must never share entries")

	other := testRequest()
	other.Messages[1].Content = "different"
	otherBody, err := CanonicalBody(other)
	require.NoError(t, err)
	assert.NotEqual(t, Key(testScope, body), Key(testScope, otherBody))
}

func TestKeyMessageOrderMatters(t *testing.T) {
	a := Request{Model: "m", Temperature: f64(0), Messages: []Message{
		{Role: "user", Content: "first"}, {Role: "user", Content: "second"}}}
	b := Request{Model: "m", Temperature: f64(0), Messages: []Message{
		{Role: "user", Content: "second"}, {Role: "user", Content: "first"}}}
	bodyA, err := CanonicalBody(a)
	require.NoError(t, err)
	bodyB, err := CanonicalBody(b)
	require.NoError(t, err)
	assert.NotEqual(t, Key(testScope, bodyA), Key(testScope, bodyB),
		"message order is semantically meaningful and is never sorted away")
}

// --- storage (miniredis) --------------------------------------------------------

func newTestCache(t *testing.T, ttl time.Duration) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, ttl), mr
}

func TestCacheMissThenHit(t *testing.T) {
	c, _ := newTestCache(t, time.Minute)
	ctx := context.Background()
	body, err := CanonicalBody(testRequest())
	require.NoError(t, err)
	key := Key(testScope, body)

	got, err := c.Get(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, got, "first lookup is a miss")

	entry := Entry{StatusCode: 200, ContentType: "application/json; charset=utf-8", Body: []byte(`{"id":"chatcmpl-1"}`)}
	require.NoError(t, c.Put(ctx, key, entry))

	got, err = c.Get(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, got, "identical follow-up hits")
	assert.Equal(t, entry, *got)
}

func TestCacheEntryExpiresAfterTTL(t *testing.T) {
	c, mr := newTestCache(t, 30*time.Second)
	ctx := context.Background()
	key := Key(testScope, []byte(`{"model":"m"}`))
	require.NoError(t, c.Put(ctx, key, Entry{StatusCode: 200, Body: []byte(`{}`)}))

	mr.FastForward(31 * time.Second)

	got, err := c.Get(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, got, "entries expire after CACHE_TTL_SECONDS")
}

func TestCacheGetSurfacesCorruptEntries(t *testing.T) {
	c, mr := newTestCache(t, time.Minute)
	key := Key(testScope, []byte(`{"model":"m"}`))
	require.NoError(t, mr.Set(key, "not json"))

	_, err := c.Get(context.Background(), key)
	assert.Error(t, err, "corrupt entries surface as errors for the caller to fail open on")
}

func TestCacheGetSurfacesRedisErrors(t *testing.T) {
	c, mr := newTestCache(t, time.Minute)
	mr.Close()
	_, err := c.Get(context.Background(), "any")
	assert.Error(t, err, "a down Redis is an error, never a fabricated hit")
}
