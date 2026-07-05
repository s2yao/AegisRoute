package models_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/models"
)

// invalidInputs are rejected by every enum: a plausible-looking typo and
// the empty string (a zero-value enum must never pass IsValid).
var invalidInputs = []string{"bogus", ""}

func TestJobStatus(t *testing.T) {
	valid := []models.JobStatus{
		models.JobStatusQueued,
		models.JobStatusRunning,
		models.JobStatusSucceeded,
		models.JobStatusPartiallyFailed,
		models.JobStatusFailed,
	}
	for _, v := range valid {
		t.Run("valid "+v.String(), func(t *testing.T) {
			assert.True(t, v.IsValid())
			// String must round-trip through Parse back to the same constant.
			got, err := models.ParseJobStatus(v.String())
			require.NoError(t, err)
			assert.Equal(t, v, got)
		})
	}
	for _, s := range invalidInputs {
		t.Run("invalid "+s, func(t *testing.T) {
			assert.False(t, models.JobStatus(s).IsValid())
			_, err := models.ParseJobStatus(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"`+s+`"`)
		})
	}
}

func TestItemStatus(t *testing.T) {
	valid := []models.ItemStatus{
		models.ItemStatusQueued,
		models.ItemStatusRunning,
		models.ItemStatusSucceeded,
		models.ItemStatusFailed,
	}
	for _, v := range valid {
		t.Run("valid "+v.String(), func(t *testing.T) {
			assert.True(t, v.IsValid())
			got, err := models.ParseItemStatus(v.String())
			require.NoError(t, err)
			assert.Equal(t, v, got)
		})
	}
	for _, s := range invalidInputs {
		t.Run("invalid "+s, func(t *testing.T) {
			assert.False(t, models.ItemStatus(s).IsValid())
			_, err := models.ParseItemStatus(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"`+s+`"`)
		})
	}
}

func TestBackendKind(t *testing.T) {
	valid := []models.BackendKind{
		models.BackendKindOpenAICompatible,
		models.BackendKindMock,
	}
	for _, v := range valid {
		t.Run("valid "+v.String(), func(t *testing.T) {
			assert.True(t, v.IsValid())
			got, err := models.ParseBackendKind(v.String())
			require.NoError(t, err)
			assert.Equal(t, v, got)
		})
	}
	for _, s := range invalidInputs {
		t.Run("invalid "+s, func(t *testing.T) {
			assert.False(t, models.BackendKind(s).IsValid())
			_, err := models.ParseBackendKind(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"`+s+`"`)
		})
	}
}

func TestCircuitState(t *testing.T) {
	valid := []models.CircuitState{
		models.CircuitStateClosed,
		models.CircuitStateOpen,
		models.CircuitStateHalfOpen,
	}
	for _, v := range valid {
		t.Run("valid "+v.String(), func(t *testing.T) {
			assert.True(t, v.IsValid())
			got, err := models.ParseCircuitState(v.String())
			require.NoError(t, err)
			assert.Equal(t, v, got)
		})
	}
	for _, s := range invalidInputs {
		t.Run("invalid "+s, func(t *testing.T) {
			assert.False(t, models.CircuitState(s).IsValid())
			_, err := models.ParseCircuitState(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"`+s+`"`)
		})
	}
}

// TestEnumStringValues pins the literal database values so a refactor of a
// constant can never silently change what is written to a CHECK-constrained
// column.
func TestEnumStringValues(t *testing.T) {
	assert.Equal(t, "partially_failed", models.JobStatusPartiallyFailed.String())
	assert.Equal(t, "openai_compatible", models.BackendKindOpenAICompatible.String())
	assert.Equal(t, "half_open", models.CircuitStateHalfOpen.String())
	assert.Equal(t, "queued", models.ItemStatusQueued.String())
}
