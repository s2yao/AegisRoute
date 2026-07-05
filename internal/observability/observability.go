package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process-wide JSON slog.Logger writing to stderr.
// Unknown level strings fall back to info.
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

type requestIDKey struct{}

// ContextWithRequestID returns a child context carrying the request id.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request id stored by ContextWithRequestID,
// or "" when none is set.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// redactMarkers are matched as case-insensitive substrings so both header
// spellings ("X-Api-Key") and env spellings ("APP_KEY_HASH_SECRET",
// "DEV_API_KEY") are caught.
var redactMarkers = []string{
	"authorization",
	"cookie",
	"token",
	"secret",
	"password",
	"api-key",
	"api_key",
	"x-admin-token",
}

// Redact reports whether a header or variable name is secret-bearing and
// must never have its value logged.
func Redact(name string) bool {
	n := strings.ToLower(name)
	for _, marker := range redactMarkers {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}
