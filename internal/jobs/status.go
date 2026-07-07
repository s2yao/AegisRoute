package jobs

import "github.com/example/aegisroute/internal/models"

// The batch status machines are pure functions with no mocks (DECISIONS.md).
// They are the single source of transition and aggregation semantics: the
// Postgres repo encodes the same rules in SQL WHERE clauses and the in-memory
// MemStore calls these directly, so the two stores cannot drift apart.

// ValidJobTransition reports whether a batch job may move from one status to
// another. The lifecycle is queued → running → exactly one terminal state;
// terminal states never change, and a job never skips running (even a job
// whose items all fail passes through running while the worker processes it).
// Self-transitions are not transitions (repos treat them as no-ops).
func ValidJobTransition(from, to models.JobStatus) bool {
	switch from {
	case models.JobStatusQueued:
		return to == models.JobStatusRunning
	case models.JobStatusRunning:
		return to == models.JobStatusSucceeded ||
			to == models.JobStatusPartiallyFailed ||
			to == models.JobStatusFailed
	default: // terminal
		return false
	}
}

// ValidItemTransition reports whether a batch item may move from one status
// to another:
//
//   - queued → running: a worker claimed the item (attempts incremented).
//   - queued → failed: the claim found the item's attempts already exhausted
//     and terminally failed it without processing.
//   - running → succeeded | failed: the worker wrote the terminal result.
//   - running → queued: crash recovery — a (re)delivered job message requeues
//     items a dead worker left mid-flight, preserving their attempt count.
//
// Terminal items (succeeded, failed) never change: that immutability is what
// makes at-least-once redelivery idempotent.
func ValidItemTransition(from, to models.ItemStatus) bool {
	switch from {
	case models.ItemStatusQueued:
		return to == models.ItemStatusRunning || to == models.ItemStatusFailed
	case models.ItemStatusRunning:
		return to == models.ItemStatusSucceeded ||
			to == models.ItemStatusFailed ||
			to == models.ItemStatusQueued
	default: // terminal
		return false
	}
}

// AggregateJobStatus derives a job's status from its item counts. While any
// item is non-terminal (succeeded+failed < total) the job is still running.
// Once every item is terminal: all succeeded → succeeded; all failed →
// failed; a mix → partially_failed. A zero-item job (unreachable — creation
// requires 1..100 items) aggregates to succeeded vacuously.
func AggregateJobStatus(total, succeeded, failed int) models.JobStatus {
	if succeeded+failed < total {
		return models.JobStatusRunning
	}
	switch {
	case failed == 0:
		return models.JobStatusSucceeded
	case succeeded == 0:
		return models.JobStatusFailed
	default:
		return models.JobStatusPartiallyFailed
	}
}
