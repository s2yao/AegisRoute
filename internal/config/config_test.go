package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/config"
)

var allEnvKeys = []string{
	"APP_ENV", "APP_PORT", "LOG_LEVEL",
	"DATABASE_URL", "REDIS_ADDR", "REDIS_DB",
	"APP_KEY_HASH_SECRET", "ADMIN_TOKEN", "DEV_API_KEY",
	"AEGISROUTE_AUTO_MIGRATE", "AEGISROUTE_AUTO_SEED",
	"RATE_LIMIT_QPS", "CACHE_TTL_SECONDS", "IDEMPOTENCY_TTL_SECONDS",
	"BACKEND_TIMEOUT_MS", "RETRY_MAX_ATTEMPTS", "RETRY_BASE_MS", "RETRY_MAX_MS",
	"CB_FAILURE_THRESHOLD", "CB_COOLDOWN_MS",
	"WORKER_CONCURRENCY", "WORKER_METRICS_PORT", "WORKER_MAX_ITEM_ATTEMPTS",
	"STREAM_KEY", "STREAM_GROUP",
	"SEED_BACKEND_FAST_URL", "SEED_BACKEND_CHEAP_URL",
}

// clearEnv blanks every AegisRoute variable for the test's duration; Load
// treats empty as unset, so defaults apply regardless of the host env.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allEnvKeys {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "dev", cfg.AppEnv)
	assert.Equal(t, 8080, cfg.AppPort)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "", cfg.DatabaseURL)
	assert.Equal(t, "localhost:6379", cfg.RedisAddr)
	assert.Equal(t, 0, cfg.RedisDB)
	assert.Equal(t, "", cfg.AppKeyHashSecret)
	assert.False(t, cfg.AutoMigrate)
	assert.False(t, cfg.AutoSeed)
	assert.Equal(t, 20, cfg.RateLimitQPS)
	assert.Equal(t, 300, cfg.CacheTTLSeconds)
	assert.Equal(t, 86400, cfg.IdempotencyTTLSeconds)
	assert.Equal(t, 5000, cfg.BackendTimeoutMS)
	assert.Equal(t, 3, cfg.RetryMaxAttempts)
	assert.Equal(t, 50, cfg.RetryBaseMS)
	assert.Equal(t, 2000, cfg.RetryMaxMS)
	assert.Equal(t, 5, cfg.CBFailureThreshold)
	assert.Equal(t, 10000, cfg.CBCooldownMS)
	assert.Equal(t, 8, cfg.WorkerConcurrency)
	assert.Equal(t, 9100, cfg.WorkerMetricsPort)
	assert.Equal(t, 3, cfg.WorkerMaxItemAttempts)
	assert.Equal(t, "aegisroute:batch_jobs", cfg.StreamKey)
	assert.Equal(t, "aegisroute-workers", cfg.StreamGroup)
}

func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_PORT", "9999")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("AEGISROUTE_AUTO_MIGRATE", "true")
	t.Setenv("AEGISROUTE_AUTO_SEED", "true")
	t.Setenv("RATE_LIMIT_QPS", "7")
	t.Setenv("STREAM_KEY", "other:stream")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "test", cfg.AppEnv)
	assert.Equal(t, 9999, cfg.AppPort)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "postgres://u:p@localhost:5432/db", cfg.DatabaseURL)
	assert.Equal(t, 3, cfg.RedisDB)
	assert.True(t, cfg.AutoMigrate)
	assert.True(t, cfg.AutoSeed)
	assert.Equal(t, 7, cfg.RateLimitQPS)
	assert.Equal(t, "other:stream", cfg.StreamKey)
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_PORT", "not-a-port")
	t.Setenv("AEGISROUTE_AUTO_SEED", "yep")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_PORT")
	assert.Contains(t, err.Error(), "AEGISROUTE_AUTO_SEED")
}

// validServeConfig returns a Config that passes ValidateForServe; tests
// mutate single fields to check each rule.
func validServeConfig() *config.Config {
	return &config.Config{
		AppEnv:                "dev",
		AppPort:               8080,
		LogLevel:              "info",
		DatabaseURL:           "postgres://aegisroute:aegisroute@localhost:5432/aegisroute?sslmode=disable",
		RedisAddr:             "localhost:6379",
		RedisDB:               0,
		AppKeyHashSecret:      strings.Repeat("s", 32),
		AdminToken:            "dev_admin_token",
		DevAPIKey:             "sg_dev_key_123",
		RateLimitQPS:          20,
		CacheTTLSeconds:       300,
		IdempotencyTTLSeconds: 86400,
		BackendTimeoutMS:      5000,
		RetryMaxAttempts:      3,
		RetryBaseMS:           50,
		RetryMaxMS:            2000,
		CBFailureThreshold:    5,
		CBCooldownMS:          10000,
		WorkerConcurrency:     8,
		WorkerMetricsPort:     9100,
		WorkerMaxItemAttempts: 3,
		StreamKey:             "aegisroute:batch_jobs",
		StreamGroup:           "aegisroute-workers",
		SeedBackendFastURL:    "http://localhost:8081",
		SeedBackendCheapURL:   "http://localhost:8082",
	}
}

func TestValidateForMigrate(t *testing.T) {
	t.Run("passes with only DATABASE_URL", func(t *testing.T) {
		cfg := &config.Config{DatabaseURL: "postgres://u:p@localhost:5432/db"}
		assert.NoError(t, cfg.ValidateForMigrate())
	})
	t.Run("rejects missing DATABASE_URL", func(t *testing.T) {
		cfg := &config.Config{}
		err := cfg.ValidateForMigrate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DATABASE_URL")
	})
}

func TestValidateForSeed(t *testing.T) {
	t.Run("passes with seed fields only, ignoring serve-only vars", func(t *testing.T) {
		cfg := &config.Config{
			DatabaseURL:         "postgres://u:p@localhost:5432/db",
			AppKeyHashSecret:    strings.Repeat("s", 32),
			DevAPIKey:           "sg_dev_key_123",
			SeedBackendFastURL:  "http://localhost:8081",
			SeedBackendCheapURL: "http://localhost:8082",
			// RedisAddr, AdminToken, ports etc. deliberately zero: seed
			// must not require them.
		}
		assert.NoError(t, cfg.ValidateForSeed())
	})
	t.Run("rejects missing DEV_API_KEY", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.DevAPIKey = ""
		err := cfg.ValidateForSeed()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DEV_API_KEY")
	})
	t.Run("rejects short APP_KEY_HASH_SECRET", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AppKeyHashSecret = strings.Repeat("s", 31)
		err := cfg.ValidateForSeed()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "APP_KEY_HASH_SECRET")
	})
	t.Run("rejects non-absolute backend URL", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.SeedBackendFastURL = "not-a-url"
		err := cfg.ValidateForSeed()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SEED_BACKEND_FAST_URL")
	})
	t.Run("rejects missing backend URL", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.SeedBackendCheapURL = ""
		err := cfg.ValidateForSeed()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SEED_BACKEND_CHEAP_URL")
	})
}

func TestValidateForServe(t *testing.T) {
	t.Run("passes with full valid config", func(t *testing.T) {
		assert.NoError(t, validServeConfig().ValidateForServe())
	})
	t.Run("rejects empty APP_KEY_HASH_SECRET", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AppKeyHashSecret = ""
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "APP_KEY_HASH_SECRET")
	})
	t.Run("rejects short APP_KEY_HASH_SECRET", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AppKeyHashSecret = strings.Repeat("s", 31)
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "APP_KEY_HASH_SECRET")
	})
	t.Run("accepts exactly 32-byte APP_KEY_HASH_SECRET", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AppKeyHashSecret = strings.Repeat("s", 32)
		assert.NoError(t, cfg.ValidateForServe())
	})
	t.Run("rejects zero RATE_LIMIT_QPS", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.RateLimitQPS = 0
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RATE_LIMIT_QPS")
	})
	t.Run("rejects invalid APP_PORT", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AppPort = 0
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "APP_PORT")
	})
	t.Run("rejects RETRY_MAX_MS below RETRY_BASE_MS", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.RetryBaseMS = 500
		cfg.RetryMaxMS = 100
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RETRY_MAX_MS")
	})
	t.Run("rejects unknown LOG_LEVEL", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.LogLevel = "loud"
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LOG_LEVEL")
	})
	t.Run("rejects missing ADMIN_TOKEN", func(t *testing.T) {
		cfg := validServeConfig()
		cfg.AdminToken = ""
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ADMIN_TOKEN")
	})
	t.Run("rejects a retry chain that cannot fit the inference budget", func(t *testing.T) {
		// 3 attempts × 20s already exceeds the 28s budget before backoff.
		cfg := validServeConfig()
		cfg.BackendTimeoutMS = 20000
		cfg.RetryMaxAttempts = 3
		err := cfg.ValidateForServe()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inference budget")
	})
	t.Run("accepts a retry chain that fits the budget", func(t *testing.T) {
		// Defaults: 3×5s + 2×2s = 19s, under the 28s budget.
		assert.NoError(t, validServeConfig().ValidateForServe())
	})
}

func TestInferenceBudget(t *testing.T) {
	// The handler's total inference budget must sit strictly under the server
	// write deadline so the response still has time to reach the client.
	assert.Less(t, (&config.Config{}).InferenceBudget(), config.ServerWriteTimeout)
	assert.Positive(t, (&config.Config{}).InferenceBudget())
}
