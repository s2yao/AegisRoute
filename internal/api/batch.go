package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/jobs"
	"github.com/example/aegisroute/internal/models"
)

// Batch create limits: the whole request body is capped at 10 MiB, and one
// batch carries 1..100 items.
const (
	maxBatchBodyBytes = 10 << 20
	maxBatchRequests  = 100
)

// allowedBatchFields / allowedBatchItemFields are the exact keys accepted at
// each level, matched case-SENSITIVELY on the raw key sets for the same
// reason as the chat handler: encoding/json's case-insensitive tag matching
// would otherwise let "Requests" or "CUSTOM_ID" alias the real fields.
var (
	allowedBatchFields     = map[string]struct{}{"requests": {}}
	allowedBatchItemFields = map[string]struct{}{"custom_id": {}, "body": {}}
)

// batchItemInput is one validated create item: the caller's id plus the
// canonical re-marshalled chat body (exactly what the worker forwards to a
// backend — validated fields only, stream never stored).
type batchItemInput struct {
	customID string
	body     json.RawMessage
}

// batchJobCreateResponse is the exact create-response shape from the spec.
type batchJobCreateResponse struct {
	ID             uuid.UUID `json:"id"`
	Object         string    `json:"object"`
	Status         string    `json:"status"`
	TotalItems     int       `json:"total_items"`
	CompletedItems int       `json:"completed_items"`
	FailedItems    int       `json:"failed_items"`
}

// batchJobResponse is the read shape for GET/List: the create fields plus
// the model and timestamps.
type batchJobResponse struct {
	ID             uuid.UUID `json:"id"`
	Object         string    `json:"object"`
	Status         string    `json:"status"`
	Model          string    `json:"model"`
	TotalItems     int       `json:"total_items"`
	CompletedItems int       `json:"completed_items"`
	FailedItems    int       `json:"failed_items"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const batchJobObject = "batch_job"

func toBatchJobResponse(j models.BatchJob) batchJobResponse {
	return batchJobResponse{
		ID:             j.ID,
		Object:         batchJobObject,
		Status:         j.Status.String(),
		Model:          j.Model,
		TotalItems:     j.TotalItems,
		CompletedItems: j.CompletedItems,
		FailedItems:    j.FailedItems,
		CreatedAt:      j.CreatedAt,
		UpdatedAt:      j.UpdatedAt,
	}
}

// batchItemResponse is one item in the items listing. Request is echoed so a
// caller can correlate results without keeping its own copy; Response and
// Error are null until the item reaches a terminal state.
type batchItemResponse struct {
	ID        uuid.UUID       `json:"id"`
	CustomID  string          `json:"custom_id"`
	Request   json.RawMessage `json:"request"`
	Status    string          `json:"status"`
	Attempts  int             `json:"attempts"`
	Response  json.RawMessage `json:"response"`
	Error     *string         `json:"error"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func toBatchItemResponse(it models.BatchJobItem) batchItemResponse {
	return batchItemResponse{
		ID:        it.ID,
		CustomID:  it.CustomID,
		Request:   it.Request,
		Status:    it.Status.String(),
		Attempts:  it.Attempts,
		Response:  it.Response,
		Error:     it.Error,
		CreatedAt: it.CreatedAt,
		UpdatedAt: it.UpdatedAt,
	}
}

// readBatchBody drains the create body — read exactly once, capped at
// 10 MiB. The idempotency request hash is computed from these exact bytes.
func readBatchBody(w http.ResponseWriter, r *http.Request) ([]byte, *chatError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, &chatError{http.StatusRequestEntityTooLarge, httperror.CodeBadRequest,
				"request body must not exceed 10 MiB"}
		}
		return nil, badRequest("could not read request body")
	}
	return raw, nil
}

// decodeBatchCreate strictly validates the create payload against the locked
// MVP schema — {"requests":[{"custom_id","body"},...]} — reusing the chat
// validation for every item body. It returns the shared model and the
// canonical item bodies, or the error to write. Rules: requests 1..100;
// custom_id required, non-empty, unique within the batch; every body passes
// the Stage-4 chat validation; all bodies name the same model.
func decodeBatchCreate(raw []byte) (string, []batchItemInput, *chatError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil, classifyDecodeError(err)
	}
	for key := range top {
		if _, ok := allowedBatchFields[key]; !ok {
			return "", nil, badRequest(fmt.Sprintf("unsupported field %q", key))
		}
	}
	rawRequests, ok := top["requests"]
	if !ok || string(rawRequests) == "null" {
		return "", nil, badRequest("requests is required")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(rawRequests, &entries); err != nil {
		return "", nil, badRequest(`invalid type for field "requests"`)
	}
	if len(entries) == 0 {
		return "", nil, badRequest("requests must contain at least 1 item")
	}
	if len(entries) > maxBatchRequests {
		return "", nil, badRequest(fmt.Sprintf("requests must not contain more than %d items", maxBatchRequests))
	}

	model := ""
	seen := make(map[string]struct{}, len(entries))
	items := make([]batchItemInput, 0, len(entries))
	for i, entry := range entries {
		for key := range entry {
			if _, ok := allowedBatchItemFields[key]; !ok {
				return "", nil, badRequest(fmt.Sprintf("unsupported field %q in requests[%d]", key, i))
			}
		}
		var customID string
		if v, ok := entry["custom_id"]; ok {
			if err := json.Unmarshal(v, &customID); err != nil {
				return "", nil, badRequest(fmt.Sprintf("invalid type for requests[%d].custom_id", i))
			}
		}
		if strings.TrimSpace(customID) == "" {
			return "", nil, badRequest(fmt.Sprintf("requests[%d].custom_id is required", i))
		}
		if _, dup := seen[customID]; dup {
			return "", nil, badRequest(fmt.Sprintf("requests[%d].custom_id %q is not unique within the batch", i, customID))
		}
		seen[customID] = struct{}{}

		body, ok := entry["body"]
		if !ok || string(body) == "null" {
			return "", nil, badRequest(fmt.Sprintf("requests[%d].body is required", i))
		}
		chatReq, cerr := decodeChatRequest(body)
		if cerr != nil {
			return "", nil, &chatError{cerr.status, cerr.code,
				fmt.Sprintf("requests[%d].body: %s", i, cerr.message)}
		}
		if model == "" {
			model = chatReq.Model
		} else if chatReq.Model != model {
			return "", nil, badRequest(fmt.Sprintf(
				"requests[%d].body.model %q differs from %q; all items in a batch must use the same model",
				i, chatReq.Model, model))
		}
		canonical, err := chatReq.forwardBody()
		if err != nil {
			return "", nil, &chatError{http.StatusInternalServerError, httperror.CodeInternal,
				"could not encode item request"}
		}
		items = append(items, batchItemInput{customID: customID, body: canonical})
	}
	return model, items, nil
}

// createBatchJob is POST /api/v1/batch-jobs. It follows the same precedence
// as the chat handler (docs/design-decisions.md): read raw body once → hash
// raw bytes → validate → idempotency Check → rate limit (new work only) →
// Begin → do the work → Complete (<500) / Release (>=500) on every path.
//
// The work itself is the transactional-outbox handoff: persist the job, its
// items, and one pending outbox row in a single Postgres transaction, then
// attempt exactly one job-level publish. Publish success durably marks the
// outbox row published; publish failure leaves it pending for the worker's
// drain loop — the job is never orphaned and never enqueued per-item.
func createBatchJob(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}

		raw, cerr := readBatchBody(w, r)
		if cerr != nil {
			httperror.Write(w, r, cerr.status, cerr.code, cerr.message)
			return
		}
		model, items, cerr := decodeBatchCreate(raw)
		if cerr != nil {
			// Invalid requests return before any idempotency record exists.
			httperror.Write(w, r, cerr.status, cerr.code, cerr.message)
			return
		}

		scope := idempotency.Scope(principal.TenantID, principal.APIKeyID, r.Method, routePattern(r))
		idemKey := strings.TrimSpace(r.Header.Get(idempotency.Header))
		if len(idemKey) > maxIdempotencyKeyLen {
			httperror.Write(w, r, http.StatusBadRequest, httperror.CodeBadRequest,
				fmt.Sprintf("Idempotency-Key must not exceed %d characters", maxIdempotencyKeyLen))
			return
		}
		rawHash := sha256Hex(raw)

		if idemKey != "" {
			dec, err := deps.Idempotency.Check(r.Context(), scope, idemKey, rawHash)
			if err != nil {
				deps.Logger.Error("idempotency lookup failed", "error", err)
				httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
					"idempotency lookup failed")
				return
			}
			if applyIdempotencyDecision(w, r, dec) {
				return
			}
		}

		if !allowRate(w, r, deps, principal) {
			return
		}

		var recordID *uuid.UUID
		if idemKey != "" {
			dec, err := deps.Idempotency.Begin(r.Context(), scope, idemKey, rawHash)
			if err != nil {
				deps.Logger.Error("idempotency begin failed", "error", err)
				httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
					"idempotency begin failed")
				return
			}
			if dec.Action == idempotency.ActionStarted {
				id := dec.RecordID
				recordID = &id
			} else if applyIdempotencyDecision(w, r, dec) {
				return
			}
		}
		// From here on every response resolves an opened record via
		// respondChat/respondError — no early return may skip it.

		jobItems := make([]models.BatchJobItem, 0, len(items))
		for _, it := range items {
			jobItems = append(jobItems, models.BatchJobItem{
				CustomID: it.customID,
				Request:  it.body,
				Status:   models.ItemStatusQueued,
			})
		}
		job, outbox, err := deps.Jobs.CreateWithItemsAndOutbox(r.Context(), models.BatchJob{
			TenantID: principal.TenantID,
			APIKeyID: principal.APIKeyID,
			Model:    model,
			Status:   models.JobStatusQueued,
		}, jobItems)
		if err != nil {
			deps.Logger.Error("batch job create failed", "error", err)
			respondError(w, r, deps, recordID, http.StatusInternalServerError,
				httperror.CodeInternal, "could not create batch job")
			return
		}
		deps.Metrics.BatchJobsCreatedTotal.Inc()

		// Exactly one logical publish attempt for the whole job. Failure is
		// not the client's problem: the job is durably committed and the
		// outbox row stays pending for the worker's drain loop.
		if err := deps.JobQueue.Publish(r.Context(), job.ID.String()); err != nil {
			deps.Logger.Warn("batch job publish failed; outbox row stays pending",
				"job_id", job.ID, "error", err)
			if merr := deps.Jobs.MarkOutboxFailedAttempt(r.Context(), outbox.ID, err.Error()); merr != nil {
				deps.Logger.Error("outbox failed-attempt mark failed", "outbox_id", outbox.ID, "error", merr)
			}
		} else if merr := deps.Jobs.MarkOutboxPublished(r.Context(), outbox.ID); merr != nil {
			// Row stays pending → the drain loop re-publishes → duplicate
			// delivery, absorbed by the worker's per-item idempotency.
			deps.Logger.Warn("outbox published mark failed; duplicate publish possible",
				"outbox_id", outbox.ID, "error", merr)
		}

		body, err := json.Marshal(batchJobCreateResponse{
			ID:             job.ID,
			Object:         batchJobObject,
			Status:         job.Status.String(),
			TotalItems:     job.TotalItems,
			CompletedItems: job.CompletedItems,
			FailedItems:    job.FailedItems,
		})
		if err != nil {
			respondError(w, r, deps, recordID, http.StatusInternalServerError,
				httperror.CodeInternal, "could not encode response")
			return
		}
		respondChat(w, r, deps, recordID, http.StatusCreated, map[string]string{
			"Content-Type": "application/json; charset=utf-8",
		}, body)
	}
}

// listBatchJobs is GET /api/v1/batch-jobs: the authenticated tenant's jobs,
// newest first. The store scopes by tenant, so another tenant's jobs are
// structurally unreachable.
func listBatchJobs(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}
		list, err := deps.Jobs.List(r.Context(), principal.TenantID)
		if err != nil {
			deps.Logger.Error("batch job list failed", "error", err)
			writeInternal(w, r, "could not list batch jobs")
			return
		}
		out := make([]batchJobResponse, 0, len(list))
		for _, j := range list {
			out = append(out, toBatchJobResponse(j))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// getBatchJob is GET /api/v1/batch-jobs/{id}. A job that does not exist and
// a job owned by another tenant are the same 404 — existence is never leaked
// across tenants.
func getBatchJob(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		job, err := deps.Jobs.Get(r.Context(), principal.TenantID, id)
		if errors.Is(err, jobs.ErrNotFound) {
			writeNotFound(w, r, "batch job not found")
			return
		}
		if err != nil {
			deps.Logger.Error("batch job get failed", "job_id", id, "error", err)
			writeInternal(w, r, "could not load batch job")
			return
		}
		writeJSON(w, http.StatusOK, toBatchJobResponse(job))
	}
}

// listBatchJobItems is GET /api/v1/batch-jobs/{id}/items, tenant-scoped the
// same way as getBatchJob.
func listBatchJobItems(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		if !ok {
			httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
				"missing authenticated principal")
			return
		}
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		items, err := deps.Jobs.Items(r.Context(), principal.TenantID, id)
		if errors.Is(err, jobs.ErrNotFound) {
			writeNotFound(w, r, "batch job not found")
			return
		}
		if err != nil {
			deps.Logger.Error("batch job items failed", "job_id", id, "error", err)
			writeInternal(w, r, "could not load batch job items")
			return
		}
		out := make([]batchItemResponse, 0, len(items))
		for _, it := range items {
			out = append(out, toBatchItemResponse(it))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
