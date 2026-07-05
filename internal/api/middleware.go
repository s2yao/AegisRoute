package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/observability"
)

// requestIDHeader is the conventional header used both to accept an inbound
// correlation id and to echo the effective one on every response.
const requestIDHeader = "X-Request-ID"

// credentialQueryParams are query-string names that may carry a credential.
// Presenting any of them is rejected with 400 because query strings leak into
// access logs, proxies, and browser history. Keys are stored lowercase and
// matched case-insensitively.
var credentialQueryParams = map[string]struct{}{
	"api_key":       {},
	"apikey":        {},
	"access_token":  {},
	"token":         {},
	"authorization": {},
	"admin_token":   {},
	"x_admin_token": {},
	"x-api-key":     {},
}

// recoverer is the outermost middleware: it converts any downstream panic into
// a 500 JSON error and logs the panic with its stack. It reads the effective
// request id from the response header (set by requestID, which runs just
// inside it) so the error body still carries a correlation id. The stack trace
// is logged, never returned to the client.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				logger.Error("panic recovered",
					"error", rec,
					"stack", string(debug.Stack()),
					"request_id", w.Header().Get(requestIDHeader),
				)
				req := r
				if id := w.Header().Get(requestIDHeader); id != "" {
					req = r.WithContext(observability.ContextWithRequestID(r.Context(), id))
				}
				httperror.Write(w, req, http.StatusInternalServerError,
					httperror.CodeInternal, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestID accepts an inbound X-Request-ID or generates a UUID when absent,
// then makes the effective id available on both the request context (for
// httperror and handlers) and the response header (for the client) on every
// path, success or error.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := observability.ContextWithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestLogger logs one structured line per request after it completes. It
// records only the chi route pattern (bounded cardinality), method, status,
// duration, and request id — never headers or bodies, so credentials can never
// reach the log.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			logger.Info("http request",
				"method", r.Method,
				"route", routePattern(r),
				"status", rec.status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", observability.RequestIDFromContext(r.Context()),
			)
		})
	}
}

// metricsMiddleware records request count and duration, labelled by the chi
// route pattern and method (and status, for the counter). Using the route
// pattern rather than the raw path keeps label cardinality bounded.
func metricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			route := routePattern(r)
			m.HTTPRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status())).Inc()
			m.HTTPRequestDurationSeconds.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		})
	}
}

// rejectQueryCredentials returns 400 when any credential-bearing query
// parameter is present, before route-scoped auth runs. This closes the query
// string as a credential channel for every route, public or authenticated.
func rejectQueryCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.URL.Query() {
			if _, bad := credentialQueryParams[strings.ToLower(name)]; bad {
				httperror.Write(w, r, http.StatusBadRequest, httperror.CodeBadRequest,
					"credentials must not be supplied in query parameters")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder wraps a ResponseWriter to remember the status code written, so
// logging and metrics can report it. It defaults to 200 because a handler that
// writes a body without calling WriteHeader has implicitly sent 200.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (rw *statusRecorder) WriteHeader(code int) {
	if rw.code == 0 {
		rw.code = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *statusRecorder) Write(b []byte) (int, error) {
	if rw.code == 0 {
		rw.code = http.StatusOK
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *statusRecorder) status() int {
	if rw.code == 0 {
		return http.StatusOK
	}
	return rw.code
}

// routePattern returns the matched chi route pattern (e.g.
// "/api/v1/backends/{id}"), or "unmatched" when routing did not reach a
// registered route (a 404, or a request short-circuited by earlier
// middleware). Reading it after next.ServeHTTP is what makes the pattern
// available, since chi fills it in while dispatching.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}

// writeJSON encodes v as the response body with the given status and the JSON
// content type. It is the single success-path writer, mirroring
// httperror.Write for the error path.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
