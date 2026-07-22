package db

import (
	"context"
	"database/sql"
	"fmt"

	// goose drives migrations through database/sql, not pgxpool, so the
	// pgx stdlib adapter must be registered as a database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/example/aegisroute/migrations"
)

// RunMigrations applies every embedded goose migration to the database at
// databaseURL. It opens its own short-lived database/sql connection because
// goose runs on database/sql rather than pgxpool.
//
// goose.SetBaseFS mutates process-global goose state, which is why the
// migration tests never use t.Parallel().
func RunMigrations(ctx context.Context, databaseURL string) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("db: migrate: open: %w", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: migrate: set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("db: migrate: up: %w", err)
	}
	return nil
}
