package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Named string constants for the status vocabularies that stay untyped:
// the stage spec locks exactly four typed enums, but later stages must
// still never inline these literals — a typo here would only surface as a
// CHECK-constraint violation in production.
const (
	// inference_requests.cache_result
	CacheResultHit    = "hit"
	CacheResultMiss   = "miss"
	CacheResultBypass = "bypass"

	// inference_requests.status
	RequestStatusSucceeded = "succeeded"
	RequestStatusFailed    = "failed"

	// batch_job_outbox.status
	OutboxStatusPending   = "pending"
	OutboxStatusPublished = "published"
	OutboxStatusFailed    = "failed"

	// idempotency_records.status
	IdempotencyStatusPending   = "pending"
	IdempotencyStatusCompleted = "completed"

	// routing_policies.strategy
	StrategyPriorityWeighted = "priority_weighted"
)

// Tenant is a paying customer; every API key, request, and batch job hangs
// off one. Mirrors the tenants table.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// APIKey is a tenant credential. Only the HMAC hash of the raw key is ever
// stored, so KeyHash can never be reversed into the credential. Mirrors the
// api_keys table.
type APIKey struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	KeyHash   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ModelBackend is one upstream inference endpoint that can serve a logical
// model name; the router picks among enabled backends by priority and
// weight. Mirrors the model_backends table.
type ModelBackend struct {
	ID          uuid.UUID
	Name        string
	BaseURL     string
	ModelName   string
	Kind        BackendKind
	Enabled     bool
	Priority    int
	Weight      int
	MaxInFlight int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RoutingPolicy names a routing strategy for a logical model; Config holds
// strategy-specific settings as raw JSON so new strategies need no schema
// change. Mirrors the routing_policies table.
type RoutingPolicy struct {
	ID        uuid.UUID
	Name      string
	ModelName string
	Strategy  string
	Config    json.RawMessage
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InferenceRequest is an append-only audit record of one gateway request.
// BackendID and CacheResult are pointers because cache hits never touch a
// backend, and the backend row may be deleted later (ON DELETE SET NULL).
// Mirrors the inference_requests table, which has no updated_at.
type InferenceRequest struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	APIKeyID    uuid.UUID
	Model       string
	BackendID   *uuid.UUID
	CacheResult *string
	Status      string
	LatencyMS   int
	RequestHash string
	CreatedAt   time.Time
}

// BatchJob is one asynchronous batch of inference items; the counters are
// denormalized so job progress can be read without scanning items. Mirrors
// the batch_jobs table.
type BatchJob struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	APIKeyID       uuid.UUID
	Model          string
	Status         JobStatus
	TotalItems     int
	CompletedItems int
	FailedItems    int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// BatchJobItem is one unit of work inside a batch job. Response and Error
// are nullable because they only exist after the item has been attempted.
// Mirrors the batch_job_items table.
type BatchJobItem struct {
	ID        uuid.UUID
	JobID     uuid.UUID
	CustomID  string
	Request   json.RawMessage
	Status    ItemStatus
	Attempts  int
	Response  json.RawMessage
	Error     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BatchJobOutbox is the transactional-outbox row written in the same
// transaction as its BatchJob, so the job-level Redis enqueue survives a
// crash between commit and publish; physical delivery is at-least-once.
// Mirrors the batch_job_outbox table.
type BatchJobOutbox struct {
	ID          uuid.UUID
	JobID       uuid.UUID
	Status      string
	Attempts    int
	LastError   *string
	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// BackendHealthSnapshot is a point-in-time observation of a backend's
// circuit breaker, kept for post-incident analysis; ObservedAt is the only
// timestamp because a snapshot is never updated. Mirrors the
// backend_health_snapshots table.
type BackendHealthSnapshot struct {
	ID           uuid.UUID
	BackendID    uuid.UUID
	CircuitState CircuitState
	InFlight     int
	ObservedAt   time.Time
}

// IdempotencyRecord stores the outcome of a request keyed by (Scope,
// IdemKey). Scope embeds tenant, key, and route so one Idempotency-Key
// cannot collide across routes; RequestHash detects key reuse with a
// different body. Mirrors the idempotency_records table, which has no
// updated_at.
type IdempotencyRecord struct {
	ID              uuid.UUID
	Scope           string
	IdemKey         string
	RequestHash     string
	Status          string
	LockedUntil     *time.Time
	ResponseStatus  *int
	ResponseHeaders json.RawMessage
	ResponseBody    json.RawMessage
	CreatedAt       time.Time
	ExpiresAt       time.Time
}
