package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultTemperature is OpenAI's default when temperature is omitted. The
// eligibility rule normalizes an omitted temperature to this value — never
// to Go's zero value — so "omitted" is NOT cacheable while an explicit 0 is.
const defaultTemperature = 1.0

// maxCacheableTemperature is the highest effective temperature the cache
// accepts: above it, outputs are too nondeterministic to be worth replaying.
const maxCacheableTemperature = 0.2

// keyPrefix namespaces cache entries in Redis.
const keyPrefix = "aegisroute:cache:"

// Request is the output-affecting subset of a completion request — the only
// inputs that may influence a cached response. Unsupported output-affecting
// fields cannot leak into caching because this type simply cannot express
// them (the defensive guard on top of Stage 4's strict validation is
// structural). Temperature and MaxTokens stay pointers so "omitted" is
// distinguishable from an explicit 0.
type Request struct {
	Model       string
	Messages    []Message
	Temperature *float64
	MaxTokens   *int
	Stop        []string
	Stream      bool
}

// Message is one conversation turn.
type Message struct {
	Role    string
	Content string
}

// Eligible reports whether the request qualifies for the response cache:
// stream must be false (defensive — validation already rejects stream:true)
// and the effective temperature (omitted → the OpenAI default 1.0) must be
// at or below 0.2. Only the caller knows the remaining conditions (2xx
// backend response, validation passed).
func Eligible(req Request) bool {
	if req.Stream {
		return false
	}
	t := defaultTemperature
	if req.Temperature != nil {
		t = *req.Temperature
	}
	return t <= maxCacheableTemperature
}

// canonicalRequest fixes the canonical JSON: object keys in sorted order
// (max_tokens, messages, model, stop, temperature), omitted optionals
// omitted, and array order preserved exactly as sent — message order and
// stop order are semantically meaningful and are never sorted.
type canonicalRequest struct {
	MaxTokens   *int               `json:"max_tokens,omitempty"`
	Messages    []canonicalMessage `json:"messages"`
	Model       string             `json:"model"`
	Stop        []string           `json:"stop,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

// canonicalMessage keeps message keys sorted (content before role).
type canonicalMessage struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}

// CanonicalBody renders the canonical JSON for the request: the stable byte
// form that makes semantically equal requests — regardless of wire-level
// field order, whitespace, or volatile headers — map to one cache key.
func CanonicalBody(req Request) ([]byte, error) {
	msgs := make([]canonicalMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = canonicalMessage{Content: m.Content, Role: m.Role}
	}
	body, err := json.Marshal(canonicalRequest{
		MaxTokens:   req.MaxTokens,
		Messages:    msgs,
		Model:       req.Model,
		Stop:        req.Stop,
		Temperature: req.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: canonicalize request: %w", err)
	}
	return body, nil
}

// Key derives the cache key: sha256 over the caller's scope (tenant/API-key
// identity) and the canonical body, separated by a NUL so no (scope, body)
// pair can collide with another by shifting bytes across the boundary.
// Volatile inputs — Idempotency-Key, X-Request-ID, Authorization, any
// header — are excluded structurally: they are simply not inputs.
func Key(scope string, canonicalBody []byte) string {
	h := sha256.New()
	h.Write([]byte(scope))
	h.Write([]byte{0})
	h.Write(canonicalBody)
	return keyPrefix + hex.EncodeToString(h.Sum(nil))
}

// Entry is one cached response: the minimal safe subset that may be
// replayed. Volatile headers are never stored — on a HIT the handler sets
// the current request's X-Request-ID and X-AegisRoute-Cache itself.
type Entry struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Body        []byte `json:"body"`
}

// Cache stores eligible completion responses in Redis with a fixed TTL.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

// New builds a Cache over rdb with entries expiring after ttl
// (CACHE_TTL_SECONDS).
func New(rdb *redis.Client, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, ttl: ttl}
}

// Get returns the entry stored under key, or (nil, nil) on a miss. Errors
// (Redis down, corrupt entry) are returned for the caller to log and treat
// as a miss — the cache must never take the serving path down with it.
func (c *Cache) Get(ctx context.Context, key string) (*Entry, error) {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("cache: corrupt entry: %w", err)
	}
	return &e, nil
}

// Put stores the entry under key for the configured TTL.
func (c *Cache) Put(ctx context.Context, key string, e Entry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("cache: marshal entry: %w", err)
	}
	if err := c.rdb.Set(ctx, key, raw, c.ttl).Err(); err != nil {
		return fmt.Errorf("cache: put: %w", err)
	}
	return nil
}
