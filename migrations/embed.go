// Package migrations is the schema source of truth for AegisRoute: every
// table lives in the goose SQL files here, embedded into the binary so that
// 'gateway-api -migrate' needs no files on disk.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
