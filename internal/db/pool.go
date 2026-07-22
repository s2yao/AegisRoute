package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/aegisroute/internal/config"
)

// Connect builds the shared pgx pool from configuration. It deliberately
// does not ping: connectivity is checked by the caller (via Ping) at the
// moment it matters, so constructing a pool never blocks or fails just
// because Postgres is momentarily unreachable.
func Connect(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("db: connect: DATABASE_URL is empty")
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	return pool, nil
}

// Ping is the explicit Postgres health check, used at startup and by
// readiness probes. Kept separate from Connect so callers control when a
// round trip to the database actually happens.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
