// Command control-worker is the asynchronous half of AegisRoute: it consumes
// job-level messages from the Redis Stream, processes batch items with a
// bounded pool against the same backends and stores the gateway uses (shared
// internal/routing + internal/inference — never an HTTP hop through
// gateway-api), and serves its own /healthz and /metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/observability"
	"github.com/example/aegisroute/internal/redisstore"
	"github.com/example/aegisroute/internal/routing"
	"github.com/example/aegisroute/internal/worker"
)

// shutdownTimeout bounds how long graceful shutdown waits for the metrics
// server to drain after the worker loops have stopped.
const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run wires the whole worker: config → logger → Postgres pool → Redis client
// → metrics → queue, selector, inference client, job store, worker → the
// consume/reclaim/outbox loops plus the health/metrics server, shut down
// gracefully on SIGINT/SIGTERM.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateForWorker(); err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel)

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

	// Same wiring as the gateway: one breaker shared by the selector (skip
	// open circuits) and the worker's outcome reports, feeding the state
	// gauge. The worker process keeps its own independent circuit state —
	// max_in_flight and breaker state are per-process by design.
	breaker := routing.NewBreaker(
		cfg.CBFailureThreshold,
		time.Duration(cfg.CBCooldownMS)*time.Millisecond,
		routing.WithStateListener(func(backend string, state models.CircuitState) {
			m.CircuitBreakerState.WithLabelValues(backend).Set(routing.CircuitStateGaugeValue(state))
		}),
	)
	selector := routing.NewSelector(db.NewBackendRepo(pool), db.NewRoutingPolicyRepo(pool), breaker)
	inferenceClient := inference.New(inference.Config{
		Timeout:     time.Duration(cfg.BackendTimeoutMS) * time.Millisecond,
		MaxAttempts: cfg.RetryMaxAttempts,
		BackoffBase: time.Duration(cfg.RetryBaseMS) * time.Millisecond,
		BackoffMax:  time.Duration(cfg.RetryMaxMS) * time.Millisecond,
		Metrics:     m,
	})

	queue := redisstore.NewStreamQueue(rdb, redisstore.StreamsFromConfig(cfg), redisstore.ConsumerName())
	w := worker.New(worker.Deps{
		Queue:     queue,
		Store:     db.NewJobRepo(pool),
		Selector:  selector,
		Inference: inferenceClient,
		Circuit:   breaker,
		Logger:    logger,
		Metrics:   m,
	}, worker.Config{
		Concurrency:     cfg.WorkerConcurrency,
		MaxItemAttempts: cfg.WorkerMaxItemAttempts,
	})

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Health/metrics endpoint on WORKER_METRICS_PORT. /healthz is liveness
	// only — a Redis or Postgres outage degrades the loops (they retry), it
	// does not make the process dead.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.WorkerMetricsPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("control-worker metrics listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	workerDone := make(chan struct{})
	go func() {
		logger.Info("control-worker consuming",
			"stream", cfg.StreamKey, "group", cfg.StreamGroup,
			"concurrency", cfg.WorkerConcurrency, "max_item_attempts", cfg.WorkerMaxItemAttempts)
		_ = w.Run(signalCtx)
		close(workerDone)
	}()

	select {
	case err := <-serveErr:
		// The metrics server dying is fatal: an unobservable worker should
		// restart rather than run blind.
		stop()
		<-workerDone
		return fmt.Errorf("metrics server: %w", err)
	case <-signalCtx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// Loops first (they observe signalCtx), then the HTTP server.
	<-workerDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("control-worker stopped")
	return nil
}
