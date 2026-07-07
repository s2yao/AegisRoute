package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/cache"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/routing"
)

// maxChatBodyBytes caps the /v1/chat/completions request body at 1 MiB,
// enforced with http.MaxBytesReader so an oversized body stops reading at
// the limit instead of buffering unboundedly.
const maxChatBodyBytes = 1 << 20

// Response headers reporting the routing and cache decisions.
const (
	headerBackend       = "X-AegisRoute-Backend"
	headerRoutingPolicy = "X-AegisRoute-Routing-Policy"
	headerCache         = "X-AegisRoute-Cache"
)

// idempotencyCompleteTimeout bounds the Complete write once it is detached
// from the request's own lifetime.
const idempotencyCompleteTimeout = 5 * time.Second

// ChatMessage is one validated conversation turn: a known role and a
// non-empty string content.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the canonical parsed form of a valid completion request.
// Temperature and MaxTokens are pointers so "omitted" stays distinguishable
// from an explicit 0 — cache eligibility depends on that distinction. Stop
// is normalized: a bare string arrives as a one-element slice. Stream never
// appears: streaming requests are rejected during parsing.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Temperature *float64
	MaxTokens   *int
	Stop        []string
}

// allowedChatFields are the exact top-level request keys accepted, matched
// case-SENSITIVELY. encoding/json matches struct tags case-insensitively
// ("MODEL" would silently bind to model and even override it), so strictness
// is enforced by checking the raw key set ourselves; anything else — in
// particular output-affecting OpenAI fields we do not implement (tools,
// response_format, seed, …) and case variants — is a 400 rather than being
// silently dropped or aliased before the backend call.
var allowedChatFields = map[string]struct{}{
	"model":       {},
	"messages":    {},
	"temperature": {},
	"max_tokens":  {},
	"stream":      {},
	"stop":        {},
}

// chatError is a validation failure carrying exactly what httperror.Write
// needs.
type chatError struct {
	status  int
	code    string
	message string
}

// validRoles are the accepted message roles.
var validRoles = map[string]struct{}{
	"system":    {},
	"user":      {},
	"assistant": {},
}

// readChatBody drains the request body — read exactly once, capped at 1 MiB —
// returning the raw bytes. The idempotency request hash is computed from
// these exact bytes, before any parsing.
func readChatBody(w http.ResponseWriter, r *http.Request) ([]byte, *chatError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, &chatError{http.StatusRequestEntityTooLarge, httperror.CodeBadRequest,
				"request body must not exceed 1 MiB"}
		}
		return nil, badRequest("could not read request body")
	}
	return raw, nil
}

// decodeChatRequest strictly decodes and validates the raw body, returning
// the canonical ChatRequest or the error to write.
func decodeChatRequest(raw []byte) (*ChatRequest, *chatError) {
	dec := json.NewDecoder(bytes.NewReader(raw))

	// Decode to raw keys first so field names can be checked exactly (see
	// allowedChatFields), then unmarshal each known field individually.
	var rawFields map[string]json.RawMessage
	if err := dec.Decode(&rawFields); err != nil {
		return nil, classifyDecodeError(err)
	}
	if dec.More() {
		return nil, badRequest("request body must contain a single JSON object")
	}
	for key := range rawFields {
		if _, ok := allowedChatFields[key]; !ok {
			return nil, badRequest(fmt.Sprintf("unsupported field %q", key))
		}
	}

	var stream bool
	if cerr := unmarshalField(rawFields, "stream", &stream); cerr != nil {
		return nil, cerr
	}
	if stream {
		return nil, &chatError{http.StatusBadRequest, httperror.CodeUnsupportedStreaming,
			"streaming is not supported; set stream to false or omit it"}
	}

	var req ChatRequest
	if cerr := unmarshalField(rawFields, "model", &req.Model); cerr != nil {
		return nil, cerr
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, badRequest("model is required")
	}

	messages, cerr := parseMessages(rawFields["messages"])
	if cerr != nil {
		return nil, cerr
	}
	req.Messages = messages

	if cerr := unmarshalField(rawFields, "temperature", &req.Temperature); cerr != nil {
		return nil, cerr
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, badRequest("temperature must be between 0 and 2")
	}
	if cerr := unmarshalField(rawFields, "max_tokens", &req.MaxTokens); cerr != nil {
		return nil, cerr
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return nil, badRequest("max_tokens must be greater than 0")
	}
	stop, cerr := parseStop(rawFields["stop"])
	if cerr != nil {
		return nil, cerr
	}
	req.Stop = stop

	return &req, nil
}

// unmarshalField decodes one optional top-level field into dst, mapping a
// type mismatch to a client-facing 400. An absent or JSON-null field leaves
// dst at its zero value.
func unmarshalField(raw map[string]json.RawMessage, key string, dst any) *chatError {
	v, ok := raw[key]
	if !ok || string(v) == "null" {
		return nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return badRequest(fmt.Sprintf("invalid type for field %q", key))
	}
	return nil
}

// parseMessages strictly validates the messages array: it must be non-empty,
// every entry may carry exactly the keys "role" and "content"
// (case-sensitively — the stdlib's case-insensitive tag matching would let
// "Role" alias role), roles come from validRoles, and content is a non-empty
// string.
func parseMessages(raw json.RawMessage) ([]ChatMessage, *chatError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, badRequest("messages must be a non-empty array")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, badRequest(`invalid type for field "messages"`)
	}
	if len(entries) == 0 {
		return nil, badRequest("messages must be a non-empty array")
	}

	out := make([]ChatMessage, 0, len(entries))
	for i, entry := range entries {
		for key := range entry {
			if key != "role" && key != "content" {
				return nil, badRequest(fmt.Sprintf("unsupported field %q in messages[%d]", key, i))
			}
		}
		var m ChatMessage
		if v, ok := entry["role"]; ok {
			if err := json.Unmarshal(v, &m.Role); err != nil {
				return nil, badRequest(fmt.Sprintf("invalid type for messages[%d].role", i))
			}
		}
		if _, ok := validRoles[m.Role]; !ok {
			return nil, badRequest(fmt.Sprintf("messages[%d].role must be one of system, user, assistant", i))
		}
		if v, ok := entry["content"]; ok {
			if err := json.Unmarshal(v, &m.Content); err != nil {
				return nil, badRequest(fmt.Sprintf("invalid type for messages[%d].content", i))
			}
		}
		if m.Content == "" {
			return nil, badRequest(fmt.Sprintf("messages[%d].content must be a non-empty string", i))
		}
		out = append(out, m)
	}
	return out, nil
}

// parseStop accepts the two valid shapes of "stop" — a non-empty string or a
// non-empty array of non-empty strings — and normalizes both to a slice.
// Absent or JSON null means no stop sequences.
func parseStop(raw json.RawMessage) ([]string, *chatError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, badRequest("stop must not be an empty string")
		}
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, badRequest("stop must be a string or an array of strings")
	}
	if len(many) == 0 {
		return nil, badRequest("stop array must not be empty")
	}
	for i, s := range many {
		if s == "" {
			return nil, badRequest(fmt.Sprintf("stop[%d] must be a non-empty string", i))
		}
	}
	return many, nil
}

// classifyDecodeError maps failures of the initial raw decode to
// client-facing errors: a non-object body to a specific 400, anything else
// to a generic malformed-body 400. (The body-size cap is handled by
// readChatBody; unknown and mistyped fields against the raw key set.)
func classifyDecodeError(err error) *chatError {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return badRequest("request body must be a JSON object")
	}
	return badRequest("request body is not valid JSON")
}

func badRequest(message string) *chatError {
	return &chatError{http.StatusBadRequest, httperror.CodeBadRequest, message}
}

// forwardBody renders the canonical upstream request: exactly the validated
// fields, with omitted optionals staying omitted (never re-encoded as 0 or
// null) and stream never forwarded.
func (req *ChatRequest) forwardBody() ([]byte, error) {
	return json.Marshal(struct {
		Model       string        `json:"model"`
		Messages    []ChatMessage `json:"messages"`
		Temperature *float64      `json:"temperature,omitempty"`
		MaxTokens   *int          `json:"max_tokens,omitempty"`
		Stop        []string      `json:"stop,omitempty"`
	}{req.Model, req.Messages, req.Temperature, req.MaxTokens, req.Stop})
}

// cacheRequest maps the validated request onto the cache's canonical input.
// Stream is always false here — validation already rejected stream:true —
// which the cache re-checks as its own defensive guard.
func cacheRequest(req *ChatRequest) cache.Request {
	msgs := make([]cache.Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = cache.Message{Role: m.Role, Content: m.Content}
	}
	return cache.Request{
		Model:       req.Model,
		Messages:    msgs,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
	}
}

// chatCompletions is POST /v1/chat/completions. The precedence is exact and
// documented in docs/design-decisions.md:
//
//	read raw body once → hash raw bytes → parse + validate →
//	idempotency replay/conflict lookup → rate limit (new work only) →
//	idempotency begin pending → cache lookup →
//	route → inference (with failover) → cache store → ledger → complete.
//
// Invalid requests never create pending records; completed replays are never
// rate-limited; and once a pending record is opened, every response path —
// success, cache hit, or error — completes it.
func chatCompletions(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			// Unreachable behind BearerAuth; guards against a future wiring bug.
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}

		raw, cerr := readChatBody(w, r)
		if cerr != nil {
			httperror.Write(w, r, cerr.status, cerr.code, cerr.message)
			return
		}
		req, cerr := decodeChatRequest(raw)
		if cerr != nil {
			// Invalid requests return before any idempotency record exists.
			httperror.Write(w, r, cerr.status, cerr.code, cerr.message)
			return
		}
		body, err := req.forwardBody()
		if err != nil {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"could not encode upstream request")
			return
		}

		scope := idempotency.Scope(principal.TenantID, principal.APIKeyID, r.Method, routePattern(r))
		idemKey := strings.TrimSpace(r.Header.Get(idempotency.Header))
		rawHash := sha256Hex(raw)

		// Idempotency replay/conflict lookup runs BEFORE rate limiting: a
		// completed replay is served for free, and a conflict is answered
		// without consuming the caller's budget.
		if idemKey != "" {
			dec, err := deps.Idempotency.Check(r.Context(), scope, idemKey, rawHash)
			if err != nil {
				deps.Logger.Error("idempotency lookup failed", "error", err)
				httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
					"idempotency lookup failed")
				return
			}
			if applyIdempotencyDecision(w, r, dec) {
				return
			}
		}

		// Rate limit only genuinely new work.
		if !allowRate(w, r, deps, principal) {
			return
		}

		// Open the pending record. A lost race (another request inserted or
		// completed the record between Check and Begin) folds back into
		// replay or conflict.
		var recordID *uuid.UUID
		if idemKey != "" {
			dec, err := deps.Idempotency.Begin(r.Context(), scope, idemKey, rawHash)
			if err != nil {
				deps.Logger.Error("idempotency begin failed", "error", err)
				httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
					"idempotency begin failed")
				return
			}
			if dec.Action == idempotency.ActionStarted {
				id := dec.RecordID
				recordID = &id
			} else if applyIdempotencyDecision(w, r, dec) {
				return
			}
		}
		// From here on every response goes through respondChat/respondError,
		// which completes an opened record first — no early return may skip it.

		// Cache lookup. The cache and idempotency are separate mechanisms with
		// separate keys: the cache keys on tenant/api-key identity plus the
		// canonicalized parsed body, so semantically equal requests hit even
		// under different Idempotency-Keys.
		cacheScope := principal.TenantID.String() + ":" + principal.APIKeyID.String()
		eligible := cache.Eligible(cacheRequest(req))
		cacheResult := models.CacheResultBypass
		var cacheKey string
		if eligible {
			canonical, err := cache.CanonicalBody(cacheRequest(req))
			if err != nil {
				respondError(w, r, deps, recordID, http.StatusInternalServerError,
					httperror.CodeInternal, "could not canonicalize request")
				return
			}
			cacheKey = cache.Key(cacheScope, canonical)
			entry, err := deps.Cache.Get(r.Context(), cacheKey)
			switch {
			case err != nil:
				// Fail open: a broken cache degrades to a miss, never an error.
				deps.Logger.Warn("cache lookup failed; treating as miss", "error", err)
				cacheResult = models.CacheResultMiss
			case entry != nil:
				deps.Metrics.CacheEventsTotal.WithLabelValues(models.CacheResultHit).Inc()
				// HIT: no backend call. The ledger row records the hit with a
				// null backend; the response reuses only the stored body and
				// content type — the X-Request-ID on the response is the
				// current request's (set by middleware), never a stored one.
				recordInference(deps, principal, req.Model, nil, models.CacheResultHit,
					models.RequestStatusSucceeded, 0, body)
				respondChat(w, r, deps, recordID, entry.StatusCode, map[string]string{
					"Content-Type": entryContentType(entry),
					headerCache:    "HIT",
				}, entry.Body)
				return
			default:
				cacheResult = models.CacheResultMiss
			}
		}
		deps.Metrics.CacheEventsTotal.WithLabelValues(cacheResult).Inc()

		// Route → inference with intra-request failover, bounded by the
		// inference budget so trying N backends can never overrun the server's
		// write deadline.
		ctx := r.Context()
		if deps.InferenceBudget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, deps.InferenceBudget)
			defer cancel()
		}

		var tried []uuid.UUID
		for {
			selection, release, selErr := deps.Selector.Select(ctx, req.Model, tried...)
			if selErr != nil {
				status, code, msg := selectErrorParts(req.Model, selErr, len(tried) > 0)
				respondError(w, r, deps, recordID, status, code, msg)
				return
			}
			tried = append(tried, selection.Backend.ID)

			resp, doErr, failover := callBackend(ctx, deps, principal, req.Model, body,
				selection, release, cacheResult)
			if doErr == nil {
				const contentType = "application/json; charset=utf-8"
				if eligible {
					// Store only after a 2xx from the backend (callBackend
					// returns success only for 2xx) — the remaining eligibility
					// conditions were checked before the lookup.
					if err := deps.Cache.Put(r.Context(), cacheKey, cache.Entry{
						StatusCode:  http.StatusOK,
						ContentType: contentType,
						Body:        resp.Body,
					}); err != nil {
						deps.Logger.Warn("cache store failed", "error", err)
					}
				}
				respondChat(w, r, deps, recordID, http.StatusOK, map[string]string{
					"Content-Type":      contentType,
					headerBackend:       selection.Backend.Name,
					headerRoutingPolicy: selection.PolicyName,
					headerCache:         strings.ToUpper(cacheResult),
				}, resp.Body)
				return
			}
			// Fail over only on a transient failure that left budget; every
			// other outcome (permanent error, cancellation, exhausted budget)
			// is terminal.
			if failover && ctx.Err() == nil {
				continue
			}
			status, code, msg := upstreamErrorParts(doErr)
			respondError(w, r, deps, recordID, status, code, msg)
			return
		}
	}
}

// applyIdempotencyDecision handles the non-proceed idempotency outcomes,
// reporting true when it wrote a response. Replays reuse only the stored
// status, whitelisted headers, and body — the X-Request-ID header stays the
// CURRENT request's (already set by middleware; stored headers never carry
// one, and any that slipped in is skipped again here).
func applyIdempotencyDecision(w http.ResponseWriter, r *http.Request, dec idempotency.Decision) bool {
	switch dec.Action {
	case idempotency.ActionReplay:
		for k, v := range dec.Stored.Headers {
			if strings.EqualFold(k, "X-Request-ID") {
				continue
			}
			w.Header().Set(k, v)
		}
		w.WriteHeader(dec.Stored.Status)
		_, _ = w.Write(dec.Stored.Body)
		return true
	case idempotency.ActionConflictBody:
		httperror.Write(w, r, http.StatusConflict, httperror.CodeConflict,
			"Idempotency-Key was reused with a different request body")
		return true
	case idempotency.ActionInProgress:
		httperror.Write(w, r, http.StatusConflict, httperror.CodeConflict,
			"request is already in progress")
		return true
	default:
		return false
	}
}

// respondChat sends the final response, first completing an opened
// idempotency record with the same status/whitelisted headers/body so a
// retry with the same key replays exactly what this request answered —
// including error envelopes (deterministic retries; see
// docs/design-decisions.md). The completion runs on a context detached from
// the request so a client disconnect cannot strand the record pending.
func respondChat(w http.ResponseWriter, r *http.Request, deps Deps, recordID *uuid.UUID,
	status int, headers map[string]string, body []byte) {
	if recordID != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyCompleteTimeout)
		defer cancel()
		if err := deps.Idempotency.Complete(ctx, *recordID, status, headers, body); err != nil {
			// Best-effort: the response is still served; the stranded pending
			// record is reclaimed after its lock lapses.
			deps.Logger.Error("failed to complete idempotency record",
				"record_id", *recordID, "status", status, "error", err)
		}
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// respondError renders the canonical error envelope through httperror.Write —
// the single error writer — into a capture buffer, so the exact same bytes
// both complete an opened idempotency record and reach the client.
func respondError(w http.ResponseWriter, r *http.Request, deps Deps, recordID *uuid.UUID,
	status int, code, message string) {
	cw := &captureWriter{header: http.Header{}}
	httperror.Write(cw, r, status, code, message)
	respondChat(w, r, deps, recordID, cw.status,
		map[string]string{"Content-Type": cw.header.Get("Content-Type")}, cw.buf.Bytes())
}

// captureWriter buffers a response written through http.ResponseWriter so it
// can be persisted before being sent.
type captureWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}

// callBackend performs one backend attempt and always leaves the shared state
// consistent: it frees the in-flight slot and releases the circuit breaker's
// reserved half-open probe even if inference panics (the recover middleware
// then turns the panic into a 500). It returns the successful response, or the
// error plus whether the caller should fail over to another backend — true
// only for a transient failure.
func callBackend(ctx context.Context, deps Deps, principal auth.Principal,
	model string, body []byte, selection routing.Selection, release func(),
	cacheResult string) (*inference.Response, error, bool) {
	backend := selection.Backend.Name
	backendID := selection.Backend.ID
	defer release() // free the in-flight slot even if Do panics

	// Guarantee a circuit report. On the normal path an explicit report sets
	// reported=true first, making this a no-op; on a panic before that, it
	// releases the half-open probe verdict-free so the backend cannot get
	// stuck un-probeable.
	reported := false
	defer func() {
		if !reported {
			deps.Circuit.ReportCanceled(backend)
		}
	}()

	start := time.Now()
	resp, err := deps.Inference.Do(ctx, selection.Backend, body)
	latencyMS := int(time.Since(start).Milliseconds())

	if err == nil {
		deps.Circuit.ReportSuccess(backend)
		reported = true
		recordInference(deps, principal, model, &backendID, cacheResult,
			models.RequestStatusSucceeded, latencyMS, body)
		return resp, nil, false
	}

	switch {
	case ctx.Err() != nil:
		// The operation context ended (client gone or budget exhausted): not a
		// verdict about the backend, and the request is over — no failover.
		deps.Circuit.ReportCanceled(backend)
		reported = true
		recordInference(deps, principal, model, &backendID, cacheResult,
			models.RequestStatusFailed, latencyMS, body)
		return nil, err, false
	case inference.IsTransient(err):
		deps.Circuit.ReportFailure(backend)
		reported = true
		recordInference(deps, principal, model, &backendID, cacheResult,
			models.RequestStatusFailed, latencyMS, body)
		return nil, err, true
	default:
		// Permanent upstream error (e.g. 400): the backend is alive, so it is a
		// circuit success, and failing over is pointless — peers serving the
		// same model reject the same request.
		deps.Circuit.ReportSuccess(backend)
		reported = true
		recordInference(deps, principal, model, &backendID, cacheResult,
			models.RequestStatusFailed, latencyMS, body)
		return nil, err, false
	}
}

// selectErrorParts maps a Selector failure onto the error envelope. Only a
// genuinely unknown model (no failover attempted yet) is the client's mistake
// (404); exhausted capacity, open circuits, or candidates spent by failover
// are an upstream availability problem (503); a store error is internal (500).
func selectErrorParts(model string, err error, failedOver bool) (int, string, string) {
	switch {
	case errors.Is(err, routing.ErrNoBackends) && !failedOver:
		return http.StatusNotFound, httperror.CodeNotFound,
			fmt.Sprintf("model %q is not served by any backend", model)
	case errors.Is(err, routing.ErrNoBackends), errors.Is(err, routing.ErrNoCapacity):
		return http.StatusServiceUnavailable, httperror.CodeUpstreamUnavailable,
			"no backend is currently available for this model"
	default:
		return http.StatusInternalServerError, httperror.CodeInternal, "routing failed"
	}
}

// upstreamErrorParts renders a backend-call failure: a transient failure is a
// 503 (retry later), anything else a 502.
func upstreamErrorParts(err error) (int, string, string) {
	status := http.StatusBadGateway
	if inference.IsTransient(err) {
		status = http.StatusServiceUnavailable
	}
	return status, httperror.CodeUpstreamUnavailable, "upstream backend request failed"
}

// recordInference hands the inference_requests audit row to the async ledger.
// Recording is fire-and-forget and off the request hot path: it never blocks
// the response and a persistence failure never turns a served completion into
// a client-facing error. backendID is nil on a cache hit (no backend was
// called); cacheResult is always one of hit|miss|bypass from Stage 5 on. The
// row's request_hash stays the canonical-body hash (aligned with the cache
// identity); the idempotency layer hashes the raw bytes separately.
func recordInference(deps Deps, principal auth.Principal, model string,
	backendID *uuid.UUID, cacheResult string, status string, latencyMS int, canonicalBody []byte) {
	if latencyMS < 0 {
		latencyMS = 0
	}
	result := cacheResult
	deps.Ledger.Record(models.InferenceRequest{
		TenantID:    principal.TenantID,
		APIKeyID:    principal.APIKeyID,
		Model:       model,
		BackendID:   backendID,
		CacheResult: &result,
		Status:      status,
		LatencyMS:   latencyMS,
		RequestHash: sha256Hex(canonicalBody),
	})
}

// entryContentType returns the cached entry's content type, defaulting to
// JSON for entries stored without one.
func entryContentType(e *cache.Entry) string {
	if e.ContentType == "" {
		return "application/json; charset=utf-8"
	}
	return e.ContentType
}

// sha256Hex is the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
