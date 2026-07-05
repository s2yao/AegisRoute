package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned by every Get*/Update method when no row matches,
// so consumers test for absence with errors.Is and never import pgx.
var ErrNotFound = errors.New("db: not found")

// mapNotFound translates the two driver-level "no rows" sentinels into
// ErrNotFound and passes every other error through unchanged. Each repo
// routes its row scans through this helper so the not-found contract holds
// uniformly across the package.
func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
