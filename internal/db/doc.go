// Package db owns the pgx pool, the embedded goose migrations, and the
// pgx-backed repositories; it is used by all binaries. Consumers declare
// their own small interfaces for just the methods they need, and the
// concrete repos here satisfy them implicitly, so no other package has to
// import pgx to talk to Postgres.
package db
