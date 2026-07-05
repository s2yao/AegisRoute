// Command gateway-api is the AegisRoute gateway binary. It has three modes,
// selected by flag: -migrate applies embedded database migrations and exits;
// -seed inserts the idempotent demo data and exits; with no flag it serves the
// HTTP API with graceful shutdown. Keeping all three in one binary means
// migrations and seeding can never drift from the server they support.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/observability"
	"github.com/example/aegisroute/internal/redisstore"
	"github.com/example/aegisroute/internal/routing"
	"github.com/example/aegisroute/internal/seed"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to finish before the process exits.
const shutdownTimeout = 15 * time.Second

func main() {
	migrate := flag.Bool("migrate", false, "apply embedded database migrations and exit")
	seedMode := flag.Bool("seed", false, "seed the demo tenant, API key, backends, and routing policy, then exit")
	flag.Parse()

	ctx := context.Background()

	var err error
	switch {
	case *migrate:
		err = runMigrate(ctx)
	case *seedMode:
		err = runSeed(ctx)
	default:
		err = runServe(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runMigrate loads config, validates only what migrations need, and applies
// the embedded goose migrations to DATABASE_URL.
func runMigrate(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateForMigrate(); err != nil {
		return err
	}
	if err := db.RunMigrations(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	observability.NewLogger(cfg.LogLevel).Info("migrations applied")
	return nil
}

// runSeed loads config, validates the seed requirements, connects only to
// Postgres (seeding never needs Redis), and runs the idempotent seeder.
func runSeed(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateForSeed(); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := seed.Run(ctx, cfg, seedRepos(pool)); err != nil {
		return err
	}
	observability.NewLogger(cfg.LogLevel).Info("seed complete")
	return nil
}

// runServe brings up the full gateway: optional auto-migrate, the Postgres
// pool and Redis client, metrics, repositories, optional auto-seed, the chi
// router, and an http.Server shut down gracefully on SIGINT/SIGTERM.
func runServe(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateForServe(); err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel)

	if cfg.AutoMigrate {
		if err := db.RunMigrations(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
		logger.Info("auto-migrate applied")
	}

	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := redisstore.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	m := metrics.New()

	if cfg.AutoSeed {
		// ValidateForServe intentionally omits the seed-only inputs (DEV_API_KEY,
		// SEED_BACKEND_*_URL) so serving without auto-seed does not require them.
		// When auto-seed is on, validate them now so a misconfiguration fails
		// loudly at boot instead of silently seeding an unusable key and backends
		// with empty base URLs.
		if err := cfg.ValidateForSeed(); err != nil {
			return err
		}
		if err := seed.Run(ctx, cfg, seedRepos(pool)); err != nil {
			return err
		}
		logger.Info("auto-seed complete")
	}

	backendRepo := db.NewBackendRepo(pool)
	policyRepo := db.NewRoutingPolicyRepo(pool)

	// One breaker instance serves both the Selector (skip open circuits) and
	// the chat handler (report outcomes); its transitions drive the
	// aegisroute_circuit_breaker_state gauge.
	breaker := routing.NewBreaker(
		cfg.CBFailureThreshold,
		time.Duration(cfg.CBCooldownMS)*time.Millisecond,
		routing.WithStateListener(func(backend string, state models.CircuitState) {
			m.CircuitBreakerState.WithLabelValues(backend).Set(routing.CircuitStateGaugeValue(state))
		}),
	)
	selector := routing.NewSelector(backendRepo, policyRepo, breaker)
	inferenceClient := inference.New(inference.Config{
		Timeout:     time.Duration(cfg.BackendTimeoutMS) * time.Millisecond,
		MaxAttempts: cfg.RetryMaxAttempts,
		BackoffBase: time.Duration(cfg.RetryBaseMS) * time.Millisecond,
		BackoffMax:  time.Duration(cfg.RetryMaxMS) * time.Millisecond,
		Metrics:     m,
	})

	handler := api.NewRouter(api.Deps{
		Logger:        logger,
		Metrics:       m,
		KeyHashSecret: cfg.AppKeyHashSecret,
		AdminToken:    cfg.AdminToken,
		Keys:          db.NewAPIKeyRepo(pool),
		Backends:      backendRepo,
		Policies:      policyRepo,
		DBPinger:      pgPinger{pool: pool},
		RedisPinger:   redisPinger{client: rdb},
		Selector:      selector,
		Inference:     inferenceClient,
		Circuit:       breaker,
		Requests:      db.NewInferenceRequestRepo(pool),
	})

	return serve(ctx, logger, cfg.AppPort, handler)
}

// serve runs the HTTP server until SIGINT/SIGTERM, then drains in-flight
// requests within shutdownTimeout.
func serve(ctx context.Context, logger *slog.Logger, port int, handler http.Handler) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("gateway-api listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-signalCtx.Done():
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		logger.Info("gateway-api stopped")
		return nil
	}
}

// seedRepos builds the seeder's repository bundle from a pool. Shared by -seed
// and the auto-seed startup path.
func seedRepos(pool *pgxpool.Pool) seed.Repos {
	return seed.Repos{
		Tenants:  db.NewTenantRepo(pool),
		Keys:     db.NewAPIKeyRepo(pool),
		Backends: db.NewBackendRepo(pool),
		Policies: db.NewRoutingPolicyRepo(pool),
	}
}

// pgPinger and redisPinger adapt the package-level Ping functions to the tiny
// api.Pinger interface, so readiness stays decoupled from the concrete pool and
// client types.
type pgPinger struct{ pool *pgxpool.Pool }

func (p pgPinger) Ping(ctx context.Context) error { return db.Ping(ctx, p.pool) }

type redisPinger struct{ client *redis.Client }

func (p redisPinger) Ping(ctx context.Context) error { return redisstore.Ping(ctx, p.client) }
