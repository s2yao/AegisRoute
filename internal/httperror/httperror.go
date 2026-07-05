package httperror

import (
	"encoding/json"
	"net/http"

	"github.com/example/aegisroute/internal/observability"
)

// Named error codes — the only values ever emitted in APIError.Code.
const (
	CodeUnauthorized         = "unauthorized"
	CodeBadRequest           = "bad_request"
	CodeNotFound             = "not_found"
	CodeConflict             = "conflict"
	CodeRateLimited          = "rate_limited"
	CodeUnsupportedStreaming = "unsupported_streaming"
	CodeInternal             = "internal"
	CodeUpstreamUnavailable  = "upstream_unavailable"
)

// APIError is the body of every non-2xx response, wrapped under "error".
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type envelope struct {
	Error APIError `json:"error"`
}

// Write emits the one true error shape with the given status, reading the
// request id from the request context.
func Write(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Error: APIError{
		Code:      code,
		Message:   message,
		RequestID: observability.RequestIDFromContext(r.Context()),
	}})
}
