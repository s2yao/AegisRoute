package observability_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/aegisroute/internal/observability"
)

func TestRedact(t *testing.T) {
	redacted := []string{
		"Authorization",
		"authorization",
		"Proxy-Authorization",
		"Cookie",
		"Set-Cookie",
		"X-Admin-Token",
		"x-admin-token",
		"X-Api-Key",
		"X-API-KEY",
		"X-Auth-Token",
		"Password",
		"APP_KEY_HASH_SECRET",
		"ADMIN_TOKEN",
		"DEV_API_KEY",
	}
	for _, name := range redacted {
		assert.True(t, observability.Redact(name), "expected Redact(%q) == true", name)
	}

	clear := []string{
		"Content-Type",
		"Accept",
		"User-Agent",
		"X-Request-ID",
		"X-AegisRoute-Backend",
		"Idempotency-Key",
		"APP_PORT",
		"LOG_LEVEL",
	}
	for _, name := range clear {
		assert.False(t, observability.Redact(name), "expected Redact(%q) == false", name)
	}
}

func TestRequestIDRoundTrip(t *testing.T) {
	ctx := observability.ContextWithRequestID(context.Background(), "abc-123")
	assert.Equal(t, "abc-123", observability.RequestIDFromContext(ctx))
}

func TestRequestIDMissing(t *testing.T) {
	assert.Equal(t, "", observability.RequestIDFromContext(context.Background()))
}

func TestNewLoggerLevels(t *testing.T) {
	ctx := context.Background()

	assert.True(t, observability.NewLogger("debug").Enabled(ctx, slog.LevelDebug))
	assert.False(t, observability.NewLogger("info").Enabled(ctx, slog.LevelDebug))
	assert.True(t, observability.NewLogger("info").Enabled(ctx, slog.LevelInfo))
	assert.False(t, observability.NewLogger("warn").Enabled(ctx, slog.LevelInfo))
	assert.False(t, observability.NewLogger("error").Enabled(ctx, slog.LevelWarn))
	assert.True(t, observability.NewLogger("ERROR").Enabled(ctx, slog.LevelError))

	// Unknown levels fall back to info.
	assert.True(t, observability.NewLogger("loud").Enabled(ctx, slog.LevelInfo))
	assert.False(t, observability.NewLogger("loud").Enabled(ctx, slog.LevelDebug))
}
