package jobs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
)

func TestValidJobTransition(t *testing.T) {
	// The full queued → running → terminal table, exhaustively. Every pair
	// not listed as valid must be rejected, including self-transitions and
	// any move out of a terminal state.
	valid := map[[2]models.JobStatus]bool{
		{models.JobStatusQueued, models.JobStatusRunning}:          true,
		{models.JobStatusRunning, models.JobStatusSucceeded}:       true,
		{models.JobStatusRunning, models.JobStatusPartiallyFailed}: true,
		{models.JobStatusRunning, models.JobStatusFailed}:          true,
	}
	all := []models.JobStatus{
		models.JobStatusQueued, models.JobStatusRunning, models.JobStatusSucceeded,
		models.JobStatusPartiallyFailed, models.JobStatusFailed,
	}
	for _, from := range all {
		for _, to := range all {
			want := valid[[2]models.JobStatus{from, to}]
			assert.Equalf(t, want, jobs.ValidJobTransition(from, to),
				"ValidJobTransition(%s, %s)", from, to)
		}
	}
}

func TestValidItemTransition(t *testing.T) {
	valid := map[[2]models.ItemStatus]bool{
		{models.ItemStatusQueued, models.ItemStatusRunning}:    true,
		{models.ItemStatusQueued, models.ItemStatusFailed}:     true, // claim-time exhaustion
		{models.ItemStatusRunning, models.ItemStatusSucceeded}: true,
		{models.ItemStatusRunning, models.ItemStatusFailed}:    true,
		{models.ItemStatusRunning, models.ItemStatusQueued}:    true, // crash-recovery requeue
	}
	all := []models.ItemStatus{
		models.ItemStatusQueued, models.ItemStatusRunning,
		models.ItemStatusSucceeded, models.ItemStatusFailed,
	}
	for _, from := range all {
		for _, to := range all {
			want := valid[[2]models.ItemStatus{from, to}]
			assert.Equalf(t, want, jobs.ValidItemTransition(from, to),
				"ValidItemTransition(%s, %s)", from, to)
		}
	}
}

func TestAggregateJobStatus(t *testing.T) {
	tests := []struct {
		name                     string
		total, succeeded, failed int
		want                     models.JobStatus
	}{
		{"all succeeded", 3, 3, 0, models.JobStatusSucceeded},
		{"all failed", 3, 0, 3, models.JobStatusFailed},
		{"mixed is partially_failed", 3, 2, 1, models.JobStatusPartiallyFailed},
		{"still running while items pending", 3, 1, 0, models.JobStatusRunning},
		{"running with one failure so far", 3, 0, 1, models.JobStatusRunning},
		{"single success", 1, 1, 0, models.JobStatusSucceeded},
		{"single failure", 1, 0, 1, models.JobStatusFailed},
		{"zero items aggregates to succeeded vacuously", 0, 0, 0, models.JobStatusSucceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				jobs.AggregateJobStatus(tt.total, tt.succeeded, tt.failed))
		})
	}
}
