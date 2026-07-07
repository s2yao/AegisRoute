package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/models"
)

// Header is the request header carrying the client's idempotency key. A
// request without it bypasses idempotency entirely.
const Header = "Idempotency-Key"

// ErrRecordActive is returned by IdempotencyStore.Begin when a record for
// the scope/key already exists and is neither expired nor a reclaimable
// stale pending — someone else holds it (live pending, or completed). The
// Coordinator resolves the race by re-reading the record.
var ErrRecordActive = errors.New("idempotency: record already active")

// Outcome classifies what an existing record means for a new request
// carrying the same scope and key.
type Outcome int

const (
	// OutcomeAbsent: no record, or an expired one — treat the request as new.
	OutcomeAbsent Outcome = iota
	// OutcomeReplay: completed with the same body — replay the stored response.
	OutcomeReplay
	// OutcomeConflictBody: the key was reused with a different body — 409.
	OutcomeConflictBody
	// OutcomeInProgress: a pending record with the same body holds a live
	// lock — the first request is still running — 409.
	OutcomeInProgress
	// OutcomeStale: a pending record whose lock has expired (the original
	// worker died mid-flight) — Begin may atomically reclaim it.
	OutcomeStale
)

// Classify maps a stored record (nil = none) to an Outcome given the new
// request's hash and the current time. It is the single source of the
// idempotency semantics, shared by the Postgres store and the in-memory test
// fake: an expired record is absent; a body mismatch is a conflict no matter
// the record's state; completed same-body replays; pending same-body is
// in-progress while its lock holds and stale after.
func Classify(rec *models.IdempotencyRecord, requestHash string, now time.Time) Outcome {
	if rec == nil || !now.Before(rec.ExpiresAt) {
		return OutcomeAbsent
	}
	if rec.RequestHash != requestHash {
		return OutcomeConflictBody
	}
	if rec.Status == models.IdempotencyStatusCompleted {
		return OutcomeReplay
	}
	if rec.LockedUntil != nil && now.Before(*rec.LockedUntil) {
		return OutcomeInProgress
	}
	return OutcomeStale
}

// LookupResult pairs a classification with the record it was derived from
// (nil when absent).
type LookupResult struct {
	Outcome Outcome
	Record  *models.IdempotencyRecord
}

// IdempotencyStore persists idempotency records. Postgres is authoritative
// (satisfied by *db.IdempotencyRepo); business-logic tests use an in-memory
// fake honoring the same Classify semantics.
type IdempotencyStore interface {
	// Lookup classifies the record stored for scope/key against requestHash.
	Lookup(ctx context.Context, scope, key, requestHash string) (LookupResult, error)
	// Begin atomically inserts a pending record — or reclaims an expired or
	// stale-pending one — locked for lockTTL and expiring after ttl. When the
	// record is held by someone else it returns ErrRecordActive.
	Begin(ctx context.Context, scope, key, requestHash string, ttl, lockTTL time.Duration) (uuid.UUID, error)
	// Complete marks a pending record completed with the response to replay.
	// headers and body are JSON (headers: an object of header name → value).
	Complete(ctx context.Context, recordID uuid.UUID, status int, headers, body []byte) error
}

// Scope builds the idempotency scope, matching the format documented on the
// idempotency_records migration: tenant + API key + method + route pattern,
// so one Idempotency-Key can never collide across tenants, keys, or routes.
// Stage 6's batch endpoint reuses this for POST /api/v1/batch-jobs.
func Scope(tenantID, apiKeyID uuid.UUID, method, routePattern string) string {
	return fmt.Sprintf("tenant:%s:key:%s:%s:%s", tenantID, apiKeyID, method, routePattern)
}

// StoredResponse is a completed record's replayable response. Headers hold
// only the replay-safe whitelist — never X-Request-ID: every replay carries
// the current request's id, not the original's.
type StoredResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Action is what the caller should do next.
type Action int

const (
	// ActionProceed: no idempotency involvement (no key) — just do the work.
	ActionProceed Action = iota
	// ActionStarted: a pending record was opened; RecordID is set and the
	// caller MUST eventually Complete it with whatever response it sends.
	ActionStarted
	// ActionReplay: return Stored as the response.
	ActionReplay
	// ActionConflictBody: 409 — the key was reused with a different body.
	ActionConflictBody
	// ActionInProgress: 409 — the first request with this key is still running.
	ActionInProgress
)

// Decision is the Coordinator's verdict for one request.
type Decision struct {
	Action   Action
	RecordID uuid.UUID
	Stored   *StoredResponse
}

// Coordinator drives the idempotency flow over an IdempotencyStore. The
// handler calls Check before rate limiting (completed replays are free),
// Begin after it (only admitted new work opens a pending record), and
// Complete with the final response.
type Coordinator struct {
	store   IdempotencyStore
	ttl     time.Duration
	lockTTL time.Duration
}

// NewCoordinator builds a Coordinator. ttl bounds how long completed records
// replay (IDEMPOTENCY_TTL_SECONDS); lockTTL bounds how long a pending record
// blocks same-key retries if its owner dies without completing — it must
// exceed the longest possible request so a live request is never reclaimed
// mid-flight.
func NewCoordinator(store IdempotencyStore, ttl, lockTTL time.Duration) *Coordinator {
	return &Coordinator{store: store, ttl: ttl, lockTTL: lockTTL}
}

// Check performs the pre-work lookup: replay a completed response or report
// a conflict without creating anything. An empty key bypasses (Proceed).
func (c *Coordinator) Check(ctx context.Context, scope, key, requestHash string) (Decision, error) {
	if key == "" {
		return Decision{Action: ActionProceed}, nil
	}
	res, err := c.store.Lookup(ctx, scope, key, requestHash)
	if err != nil {
		return Decision{}, fmt.Errorf("idempotency: lookup: %w", err)
	}
	return c.decideFromLookup(res, ActionProceed)
}

// Begin opens the pending record for admitted new work. An empty key
// bypasses. A lost race (someone else inserted or completed the record
// between Check and Begin) folds back into replay or conflict.
func (c *Coordinator) Begin(ctx context.Context, scope, key, requestHash string) (Decision, error) {
	if key == "" {
		return Decision{Action: ActionProceed}, nil
	}
	id, err := c.store.Begin(ctx, scope, key, requestHash, c.ttl, c.lockTTL)
	if err == nil {
		return Decision{Action: ActionStarted, RecordID: id}, nil
	}
	if !errors.Is(err, ErrRecordActive) {
		return Decision{}, fmt.Errorf("idempotency: begin: %w", err)
	}
	res, err := c.store.Lookup(ctx, scope, key, requestHash)
	if err != nil {
		return Decision{}, fmt.Errorf("idempotency: lookup after begin race: %w", err)
	}
	// Absent/Stale here means the blocker changed again mid-race; telling the
	// client the request is in progress is always safe — a retry resolves it.
	return c.decideFromLookup(res, ActionInProgress)
}

// Complete stores the final response on an opened record. headers must not
// include X-Request-ID (respondents strip it; stripped again here
// defensively).
func (c *Coordinator) Complete(ctx context.Context, recordID uuid.UUID, status int, headers map[string]string, body []byte) error {
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, "X-Request-ID") {
			continue
		}
		clean[k] = v
	}
	headerJSON, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("idempotency: marshal headers: %w", err)
	}
	if err := c.store.Complete(ctx, recordID, status, headerJSON, body); err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}

// decideFromLookup maps a classified lookup onto a Decision; whenNew is the
// action for Absent/Stale (Proceed during Check, InProgress after a lost
// Begin race).
func (c *Coordinator) decideFromLookup(res LookupResult, whenNew Action) (Decision, error) {
	switch res.Outcome {
	case OutcomeReplay:
		stored, err := replayFromRecord(res.Record)
		if err != nil {
			return Decision{}, err
		}
		return Decision{Action: ActionReplay, Stored: stored}, nil
	case OutcomeConflictBody:
		return Decision{Action: ActionConflictBody}, nil
	case OutcomeInProgress:
		return Decision{Action: ActionInProgress}, nil
	default: // Absent, Stale
		return Decision{Action: whenNew}, nil
	}
}

// replayFromRecord converts a completed record into a StoredResponse,
// dropping any X-Request-ID that might have slipped into storage.
func replayFromRecord(rec *models.IdempotencyRecord) (*StoredResponse, error) {
	if rec == nil || rec.ResponseStatus == nil {
		return nil, errors.New("idempotency: completed record has no stored response")
	}
	headers := map[string]string{}
	if len(rec.ResponseHeaders) > 0 {
		if err := json.Unmarshal(rec.ResponseHeaders, &headers); err != nil {
			return nil, fmt.Errorf("idempotency: stored headers corrupt: %w", err)
		}
	}
	for k := range headers {
		if strings.EqualFold(k, "X-Request-ID") {
			delete(headers, k)
		}
	}
	return &StoredResponse{
		Status:  *rec.ResponseStatus,
		Headers: headers,
		Body:    []byte(rec.ResponseBody),
	}, nil
}
