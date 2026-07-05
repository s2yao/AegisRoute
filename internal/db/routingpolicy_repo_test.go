package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNormalizeJSONB is Docker-free: it pins the pure normalization that lets
// the COALESCE default fire for any contentless Config, not just literal nil.
func TestNormalizeJSONB(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		assert.Nil(t, normalizeJSONB(nil))
	})
	t.Run("empty becomes nil", func(t *testing.T) {
		assert.Nil(t, normalizeJSONB(json.RawMessage{}))
	})
	t.Run("whitespace becomes nil", func(t *testing.T) {
		assert.Nil(t, normalizeJSONB(json.RawMessage(" \t\n ")))
	})
	t.Run("content passes through unchanged", func(t *testing.T) {
		in := json.RawMessage(`{"a":1}`)
		assert.Equal(t, in, normalizeJSONB(in))
	})
	t.Run("malformed content still passes through", func(t *testing.T) {
		// Genuine (if invalid) content is the caller's to fix; normalize must
		// not swallow it into the default.
		in := json.RawMessage(`{not json`)
		assert.Equal(t, in, normalizeJSONB(in))
	})
}
