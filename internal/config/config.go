package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// minKeyHashSecretBytes is the floor for APP_KEY_HASH_SECRET: a short HMAC
// secret weakens every stored API-key hash at once.
const minKeyHashSecretBytes = 32

// Config holds every environment-derived setting used by the three binaries.
// The comment on each field names its environment variable.
type Config struct {
	AppEnv   string // APP_ENV
	AppPort  int    // APP_PORT
	LogLevel string // LOG_LEVEL

	DatabaseURL string // DATABASE_URL
	RedisAddr   string // REDIS_ADDR
	RedisDB     int    // REDIS_DB

	AppKeyHashSecret string // APP_KEY_HASH_SECRET
	AdminToken       string // ADMIN_TOKEN
	DevAPIKey        string // DEV_API_KEY

	AutoMigrate bool // AEGISROUTE_AUTO_MIGRATE
	AutoSeed    bool // AEGISROUTE_AUTO_SEED

	RateLimitQPS          int // RATE_LIMIT_QPS
	CacheTTLSeconds       int // CACHE_TTL_SECONDS
	IdempotencyTTLSeconds int // IDEMPOTENCY_TTL_SECONDS

	BackendTimeoutMS int // BACKEND_TIMEOUT_MS
	RetryMaxAttempts int // RETRY_MAX_ATTEMPTS
	RetryBaseMS      int // RETRY_BASE_MS
	RetryMaxMS       int // RETRY_MAX_MS

	CBFailureThreshold int // CB_FAILURE_THRESHOLD
	CBCooldownMS       int // CB_COOLDOWN_MS

	WorkerConcurrency     int // WORKER_CONCURRENCY
	WorkerMetricsPort     int // WORKER_METRICS_PORT
	WorkerMaxItemAttempts int // WORKER_MAX_ITEM_ATTEMPTS

	StreamKey   string // STREAM_KEY
	StreamGroup string // STREAM_GROUP

	SeedBackendFastURL  string // SEED_BACKEND_FAST_URL
	SeedBackendCheapURL string // SEED_BACKEND_CHEAP_URL
}

// Load reads all AegisRoute environment variables, applying defaults where a
// variable is unset or empty. It fails only on malformed values (e.g. a
// non-integer port); presence requirements belong to the per-mode validators.
func Load() (*Config, error) {
	var errs []string
	cfg := &Config{
		AppEnv:   getString("APP_ENV", "dev"),
		AppPort:  getInt("APP_PORT", 8080, &errs),
		LogLevel: getString("LOG_LEVEL", "info"),

		DatabaseURL: getString("DATABASE_URL", ""),
		RedisAddr:   getString("REDIS_ADDR", "localhost:6379"),
		RedisDB:     getInt("REDIS_DB", 0, &errs),

		AppKeyHashSecret: getString("APP_KEY_HASH_SECRET", ""),
		AdminToken:       getString("ADMIN_TOKEN", ""),
		DevAPIKey:        getString("DEV_API_KEY", ""),

		AutoMigrate: getBool("AEGISROUTE_AUTO_MIGRATE", false, &errs),
		AutoSeed:    getBool("AEGISROUTE_AUTO_SEED", false, &errs),

		RateLimitQPS:          getInt("RATE_LIMIT_QPS", 20, &errs),
		CacheTTLSeconds:       getInt("CACHE_TTL_SECONDS", 300, &errs),
		IdempotencyTTLSeconds: getInt("IDEMPOTENCY_TTL_SECONDS", 86400, &errs),

		BackendTimeoutMS: getInt("BACKEND_TIMEOUT_MS", 5000, &errs),
		RetryMaxAttempts: getInt("RETRY_MAX_ATTEMPTS", 3, &errs),
		RetryBaseMS:      getInt("RETRY_BASE_MS", 50, &errs),
		RetryMaxMS:       getInt("RETRY_MAX_MS", 2000, &errs),

		CBFailureThreshold: getInt("CB_FAILURE_THRESHOLD", 5, &errs),
		CBCooldownMS:       getInt("CB_COOLDOWN_MS", 10000, &errs),

		WorkerConcurrency:     getInt("WORKER_CONCURRENCY", 8, &errs),
		WorkerMetricsPort:     getInt("WORKER_METRICS_PORT", 9100, &errs),
		WorkerMaxItemAttempts: getInt("WORKER_MAX_ITEM_ATTEMPTS", 3, &errs),

		StreamKey:   getString("STREAM_KEY", "aegisroute:batch_jobs"),
		StreamGroup: getString("STREAM_GROUP", "aegisroute-workers"),

		SeedBackendFastURL:  getString("SEED_BACKEND_FAST_URL", ""),
		SeedBackendCheapURL: getString("SEED_BACKEND_CHEAP_URL", ""),
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// ValidateForMigrate checks the minimum needed to run database migrations:
// only DATABASE_URL, so migrations never fail on unrelated runtime variables.
func (c *Config) ValidateForMigrate() error {
	var errs []string
	requireNonEmpty(&errs, "DATABASE_URL", c.DatabaseURL)
	return join(errs)
}

// ValidateForSeed checks what the idempotent demo seeder needs: the database,
// the key-hashing secret, the demo API key, and both mock backend URLs.
func (c *Config) ValidateForSeed() error {
	var errs []string
	requireNonEmpty(&errs, "DATABASE_URL", c.DatabaseURL)
	checkKeyHashSecret(&errs, c.AppKeyHashSecret)
	requireNonEmpty(&errs, "DEV_API_KEY", c.DevAPIKey)
	requireAbsoluteURL(&errs, "SEED_BACKEND_FAST_URL", c.SeedBackendFastURL)
	requireAbsoluteURL(&errs, "SEED_BACKEND_CHEAP_URL", c.SeedBackendCheapURL)
	return join(errs)
}

// ValidateForServe checks the full runtime configuration used by the HTTP
// server and the worker.
func (c *Config) ValidateForServe() error {
	var errs []string
	requireNonEmpty(&errs, "DATABASE_URL", c.DatabaseURL)
	requireNonEmpty(&errs, "REDIS_ADDR", c.RedisAddr)
	if c.RedisDB < 0 {
		errs = append(errs, "REDIS_DB: must be >= 0")
	}
	checkKeyHashSecret(&errs, c.AppKeyHashSecret)
	requireNonEmpty(&errs, "ADMIN_TOKEN", c.AdminToken)
	requirePort(&errs, "APP_PORT", c.AppPort)
	requirePort(&errs, "WORKER_METRICS_PORT", c.WorkerMetricsPort)
	requireLogLevel(&errs, c.LogLevel)
	requirePositive(&errs, "RATE_LIMIT_QPS", c.RateLimitQPS)
	requirePositive(&errs, "CACHE_TTL_SECONDS", c.CacheTTLSeconds)
	requirePositive(&errs, "IDEMPOTENCY_TTL_SECONDS", c.IdempotencyTTLSeconds)
	requirePositive(&errs, "BACKEND_TIMEOUT_MS", c.BackendTimeoutMS)
	requirePositive(&errs, "RETRY_MAX_ATTEMPTS", c.RetryMaxAttempts)
	requirePositive(&errs, "RETRY_BASE_MS", c.RetryBaseMS)
	requirePositive(&errs, "RETRY_MAX_MS", c.RetryMaxMS)
	if c.RetryMaxMS >= 1 && c.RetryBaseMS >= 1 && c.RetryMaxMS < c.RetryBaseMS {
		errs = append(errs, "RETRY_MAX_MS: must be >= RETRY_BASE_MS")
	}
	requirePositive(&errs, "CB_FAILURE_THRESHOLD", c.CBFailureThreshold)
	requirePositive(&errs, "CB_COOLDOWN_MS", c.CBCooldownMS)
	requirePositive(&errs, "WORKER_CONCURRENCY", c.WorkerConcurrency)
	requirePositive(&errs, "WORKER_MAX_ITEM_ATTEMPTS", c.WorkerMaxItemAttempts)
	requireNonEmpty(&errs, "STREAM_KEY", c.StreamKey)
	requireNonEmpty(&errs, "STREAM_GROUP", c.StreamGroup)
	return join(errs)
}

func getString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int, errs *[]string) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not an integer: %q", key, v))
		return def
	}
	return n
}

func getBool(key string, def bool, errs *[]string) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not a boolean: %q", key, v))
		return def
	}
	return b
}

// Validation messages name the variable but never echo its value, so a
// misplaced secret can't leak through an error string.
func requireNonEmpty(errs *[]string, name, v string) {
	if v == "" {
		*errs = append(*errs, name+": required")
	}
}

func checkKeyHashSecret(errs *[]string, v string) {
	if v == "" {
		*errs = append(*errs, "APP_KEY_HASH_SECRET: required")
		return
	}
	if len(v) < minKeyHashSecretBytes {
		*errs = append(*errs, fmt.Sprintf(
			"APP_KEY_HASH_SECRET: must be at least %d bytes, got %d", minKeyHashSecretBytes, len(v)))
	}
}

func requireAbsoluteURL(errs *[]string, name, v string) {
	if v == "" {
		*errs = append(*errs, name+": required")
		return
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		*errs = append(*errs, name+": must be an absolute URL (scheme://host)")
	}
}

func requirePort(errs *[]string, name string, v int) {
	if v < 1 || v > 65535 {
		*errs = append(*errs, fmt.Sprintf("%s: must be a valid TCP port (1-65535), got %d", name, v))
	}
}

func requirePositive(errs *[]string, name string, v int) {
	if v < 1 {
		*errs = append(*errs, fmt.Sprintf("%s: must be > 0, got %d", name, v))
	}
}

func requireLogLevel(errs *[]string, v string) {
	switch strings.ToLower(v) {
	case "debug", "info", "warn", "error":
	default:
		*errs = append(*errs, "LOG_LEVEL: must be one of debug, info, warn, error")
	}
}

func join(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
}
