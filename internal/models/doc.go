// Package models holds the shared domain types mirroring the schema in
// migrations/; used by every binary. Keeping one package as the single
// source of struct definitions and status vocabularies means the server,
// worker, and seeder can never drift on what a column is allowed to hold.
package models
