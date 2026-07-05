// Command gateway-api is the AegisRoute gateway binary. In Stage 2 it only
// implements -migrate; -seed and the default serve mode are deliberate stubs
// that arrive in Stage 3, so this file stays a thin mode dispatcher.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/observability"
)

func main() {
	migrate := flag.Bool("migrate", false, "apply embedded database migrations and exit")
	seed := flag.Bool("seed", false, "seed demo tenant, API key, and backends, then exit")
	flag.Parse()

	ctx := context.Background()

	switch {
	case *migrate:
		if err := runMigrate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *seed:
		fmt.Println("seed not implemented until Stage 3")
	default:
		fmt.Println("serve not implemented until Stage 3")
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
