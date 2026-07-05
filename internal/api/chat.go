package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/routing"
)

// maxChatBodyBytes caps the /v1/chat/completions request body at 1 MiB,
// enforced with http.MaxBytesReader so an oversized body stops reading at
// the limit instead of buffering unboundedly.
const maxChatBodyBytes = 1 << 20

// Response headers reporting the routing decision. The X-AegisRoute-Cache
// header is deliberately absent until Stage 5.
const (
	headerBackend       = "X-AegisRoute-Backend"
	headerRoutingPolicy = "X-AegisRoute-Routing-Policy"
)

// ChatMessage is one validated conversation turn: a known role and a
// non-empty string content.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the canonical parsed form of a valid completion request.
// Temperature and MaxTokens are pointers so "omitted" stays distinguishable
// from an explicit 0 — Stage 5's cache-eligibility rules depend on that
// distinction. Stop is normalized: a bare string arrives as a one-element
// slice. Stream never appears: streaming requests are rejected during
// parsing.
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

// decodeChatRequest reads, strictly decodes, and validates the request body,
// returning the canonical ChatRequest or the error to write. It needs the
// ResponseWriter only to arm http.MaxBytesReader's connection handling.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (*ChatRequest, *chatError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	dec := json.NewDecoder(r.Body)

	// Decode to raw keys first so field names can be checked exactly (see
	// allowedChatFields), then unmarshal each known field individually.
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, classifyDecodeError(err)
	}
	if dec.More() {
		return nil, badRequest("request body must contain a single JSON object")
	}
	for key := range raw {
		if _, ok := allowedChatFields[key]; !ok {
			return nil, badRequest(fmt.Sprintf("unsupported field %q", key))
		}
	}

	var stream bool
	if cerr := unmarshalField(raw, "stream", &stream); cerr != nil {
		return nil, cerr
	}
	if stream {
		return nil, &chatError{http.StatusBadRequest, httperror.CodeUnsupportedStreaming,
			"streaming is not supported; set stream to false or omit it"}
	}

	var req ChatRequest
	if cerr := unmarshalField(raw, "model", &req.Model); cerr != nil {
		return nil, cerr
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, badRequest("model is required")
	}

	messages, cerr := parseMessages(raw["messages"])
	if cerr != nil {
		return nil, cerr
	}
	req.Messages = messages

	if cerr := unmarshalField(raw, "temperature", &req.Temperature); cerr != nil {
		return nil, cerr
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, badRequest("temperature must be between 0 and 2")
	}
	if cerr := unmarshalField(raw, "max_tokens", &req.MaxTokens); cerr != nil {
		return nil, cerr
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return nil, badRequest("max_tokens must be greater than 0")
	}
	stop, cerr := parseStop(raw["stop"])
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
// client-facing errors: the body-size cap to 413, a non-object body to a
// specific 400, anything else to a generic malformed-body 400. (Unknown and
// mistyped fields are handled after this decode, against the raw key set.)
func classifyDecodeError(err error) *chatError {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return &chatError{http.StatusRequestEntityTooLarge, httperror.CodeBadRequest,
			"request body must not exceed 1 MiB"}
	}
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

// chatCompletions is POST /v1/chat/completions: validate strictly, route to
// a backend, call it through the inference client, report the outcome to the
// circuit breaker, persist the ledger row, and return the backend's
// OpenAI-compatible JSON with the routing decision in response headers.
func chatCompletions(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			// Unreachable behind BearerAuth; guards against a future wiring bug.
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}

		req, cerr := decodeChatRequest(w, r)
		if cerr != nil {
			httperror.Write(w, r, cerr.status, cerr.code, cerr.message)
			return
		}

		// Build the upstream body before routing: once Select has reserved an
		// in-flight slot (and possibly the breaker's single half-open probe),
		// the only permissible next step is the backend call plus its outcome
		// report — no early return may sit between them.
		body, err := req.forwardBody()
		if err != nil {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"could not encode upstream request")
			return
		}

		selection, release, err := deps.Selector.Select(r.Context(), req.Model)
		if err != nil {
			writeSelectError(w, r, req.Model, err)
			return
		}
		defer release()

		start := time.Now()
		resp, err := deps.Inference.Do(r.Context(), selection.Backend, body)
		latencyMS := int(time.Since(start).Milliseconds())

		if err != nil {
			// Circuit verdicts: only transient failures count against the
			// backend; a permanent upstream error (e.g. 400) proves it is
			// alive. A dead request context outranks both — whatever error
			// came back, the client's departure is not evidence about the
			// backend, and a reserved half-open probe must be returned.
			switch {
			case r.Context().Err() != nil:
				deps.Circuit.ReportCanceled(selection.Backend.Name)
			case inference.IsTransient(err):
				deps.Circuit.ReportFailure(selection.Backend.Name)
			default:
				deps.Circuit.ReportSuccess(selection.Backend.Name)
			}
			recordInference(r.Context(), deps, principal, req.Model, selection,
				models.RequestStatusFailed, latencyMS, body)

			status := http.StatusBadGateway
			if inference.IsTransient(err) {
				status = http.StatusServiceUnavailable
			}
			httperror.Write(w, r, status, httperror.CodeUpstreamUnavailable,
				"upstream backend request failed")
			return
		}

		deps.Circuit.ReportSuccess(selection.Backend.Name)
		recordInference(r.Context(), deps, principal, req.Model, selection,
			models.RequestStatusSucceeded, latencyMS, body)

		w.Header().Set(headerBackend, selection.Backend.Name)
		w.Header().Set(headerRoutingPolicy, selection.PolicyName)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.Body)
	}
}

// writeSelectError maps Selector failures onto the error envelope: an
// unknown model is the client's mistake (404), exhausted capacity or open
// circuits are an upstream availability problem (503), anything else is
// internal.
func writeSelectError(w http.ResponseWriter, r *http.Request, model string, err error) {
	switch {
	case errors.Is(err, routing.ErrNoBackends):
		httperror.Write(w, r, http.StatusNotFound, httperror.CodeNotFound,
			fmt.Sprintf("model %q is not served by any backend", model))
	case errors.Is(err, routing.ErrNoCapacity):
		httperror.Write(w, r, http.StatusServiceUnavailable, httperror.CodeUpstreamUnavailable,
			"no backend is currently available for this model")
	default:
		httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
			"routing failed")
	}
}

// recordInferenceTimeout bounds the ledger insert once it is detached from
// the request's own lifetime.
const recordInferenceTimeout = 5 * time.Second

// recordInference appends the inference_requests ledger row. Persistence is
// best-effort: a ledger write failure is logged but never turns an otherwise
// served completion into a client-facing error. The insert runs on a context
// detached from the request's cancellation (request values, like the request
// id, are kept) — otherwise a client disconnect would erase the audit row
// for exactly the requests most worth auditing.
func recordInference(ctx context.Context, deps Deps, principal auth.Principal,
	model string, selection routing.Selection, status string, latencyMS int, body []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordInferenceTimeout)
	defer cancel()
	if latencyMS < 0 {
		latencyMS = 0
	}
	sum := sha256.Sum256(body)
	backendID := selection.Backend.ID
	_, err := deps.Requests.Insert(ctx, models.InferenceRequest{
		TenantID:    principal.TenantID,
		APIKeyID:    principal.APIKeyID,
		Model:       model,
		BackendID:   &backendID,
		Status:      status,
		LatencyMS:   latencyMS,
		RequestHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		deps.Logger.Error("failed to persist inference request",
			"backend", selection.Backend.Name, "status", status, "error", err)
	}
}
