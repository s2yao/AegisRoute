package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned by every Get*/Update method when no row matches,
// so consumers test for absence with errors.Is and never import pgx.
var ErrNotFound = errors.New("db: not found")

// uniqueViolationCode is the Postgres SQLSTATE for a unique-constraint
// violation (23505).
const uniqueViolationCode = "23505"

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation, letting handlers map a duplicate name/hash Insert to 409 Conflict
// without importing pgx themselves.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode
}

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
