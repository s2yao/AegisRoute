package migrations_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/migrations"
)

// gooseName is the '<number>_<name>.sql' shape goose requires to order and
// version migrations; a misnamed file would be silently skipped at runtime.
var gooseName = regexp.MustCompile(`^[0-9]+_.+\.sql$`)

// TestEmbeddedMigrations guards the //go:embed wiring: a migration that isn't
// picked up by the embed pattern would only surface as a broken deploy.
func TestEmbeddedMigrations(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(names), 6, "expected at least 6 embedded migration files")

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			assert.Regexp(t, gooseName, name, "filename must match goose's <number>_<name>.sql shape")

			body, err := fs.ReadFile(migrations.FS, name)
			require.NoError(t, err)
			require.NotEmpty(t, body)

			content := string(body)
			assert.True(t, strings.Contains(content, "-- +goose Up"), "missing '-- +goose Up' section")
			assert.True(t, strings.Contains(content, "-- +goose Down"), "missing '-- +goose Down' section")
		})
	}
}
