package models

import "fmt"

// The four typed enums below back columns whose values drive control flow
// (worker state machines, backend dispatch, circuit breaking). Typing them
// lets the compiler catch a misspelled status where a bare string would
// silently violate the matching CHECK constraint at runtime. The remaining
// status vocabularies stay as plain string constants in models.go.

// JobStatus is the lifecycle state of a batch job, mirroring the CHECK
// constraint on batch_jobs.status.
type JobStatus string

const (
	JobStatusQueued          JobStatus = "queued"
	JobStatusRunning         JobStatus = "running"
	JobStatusSucceeded       JobStatus = "succeeded"
	JobStatusPartiallyFailed JobStatus = "partially_failed"
	JobStatusFailed          JobStatus = "failed"
)

// String returns the exact value stored in batch_jobs.status.
func (s JobStatus) String() string { return string(s) }

// IsValid reports whether s is one of the values the database accepts.
func (s JobStatus) IsValid() bool {
	switch s {
	case JobStatusQueued, JobStatusRunning, JobStatusSucceeded,
		JobStatusPartiallyFailed, JobStatusFailed:
		return true
	}
	return false
}

// ParseJobStatus converts untrusted input (API payloads, DB scans) into a
// JobStatus, rejecting anything the schema's CHECK constraint would reject.
func ParseJobStatus(s string) (JobStatus, error) {
	v := JobStatus(s)
	if !v.IsValid() {
		return "", fmt.Errorf("invalid job status %q", s)
	}
	return v, nil
}

// ItemStatus is the lifecycle state of a single batch job item, mirroring
// the CHECK constraint on batch_job_items.status. Items have no
// partially_failed state: an item either finished or it did not.
type ItemStatus string

const (
	ItemStatusQueued    ItemStatus = "queued"
	ItemStatusRunning   ItemStatus = "running"
	ItemStatusSucceeded ItemStatus = "succeeded"
	ItemStatusFailed    ItemStatus = "failed"
)

// String returns the exact value stored in batch_job_items.status.
func (s ItemStatus) String() string { return string(s) }

// IsValid reports whether s is one of the values the database accepts.
func (s ItemStatus) IsValid() bool {
	switch s {
	case ItemStatusQueued, ItemStatusRunning, ItemStatusSucceeded, ItemStatusFailed:
		return true
	}
	return false
}

// ParseItemStatus converts untrusted input into an ItemStatus, rejecting
// anything the schema's CHECK constraint would reject.
func ParseItemStatus(s string) (ItemStatus, error) {
	v := ItemStatus(s)
	if !v.IsValid() {
		return "", fmt.Errorf("invalid item status %q", s)
	}
	return v, nil
}

// BackendKind selects the upstream client implementation for a model
// backend, mirroring the CHECK constraint on model_backends.kind.
type BackendKind string

const (
	BackendKindOpenAICompatible BackendKind = "openai_compatible"
	BackendKindMock             BackendKind = "mock"
)

// String returns the exact value stored in model_backends.kind.
func (k BackendKind) String() string { return string(k) }

// IsValid reports whether k is one of the values the database accepts.
func (k BackendKind) IsValid() bool {
	switch k {
	case BackendKindOpenAICompatible, BackendKindMock:
		return true
	}
	return false
}

// ParseBackendKind converts untrusted input into a BackendKind, rejecting
// anything the schema's CHECK constraint would reject.
func ParseBackendKind(s string) (BackendKind, error) {
	v := BackendKind(s)
	if !v.IsValid() {
		return "", fmt.Errorf("invalid backend kind %q", s)
	}
	return v, nil
}

// CircuitState is a circuit breaker state, mirroring the CHECK constraint
// on backend_health_snapshots.circuit_state.
type CircuitState string

const (
	CircuitStateClosed   CircuitState = "closed"
	CircuitStateOpen     CircuitState = "open"
	CircuitStateHalfOpen CircuitState = "half_open"
)

// String returns the exact value stored in
// backend_health_snapshots.circuit_state.
func (s CircuitState) String() string { return string(s) }

// IsValid reports whether s is one of the values the database accepts.
func (s CircuitState) IsValid() bool {
	switch s {
	case CircuitStateClosed, CircuitStateOpen, CircuitStateHalfOpen:
		return true
	}
	return false
}

// ParseCircuitState converts untrusted input into a CircuitState, rejecting
// anything the schema's CHECK constraint would reject.
func ParseCircuitState(s string) (CircuitState, error) {
	v := CircuitState(s)
	if !v.IsValid() {
		return "", fmt.Errorf("invalid circuit state %q", s)
	}
	return v, nil
}
