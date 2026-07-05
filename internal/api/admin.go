package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/models"
)

// maxBodyBytes caps admin request bodies. Control-plane payloads are tiny, so a
// 1 MiB ceiling is generous while still refusing an unbounded upload.
const maxBodyBytes = 1 << 20

// --- response shapes -------------------------------------------------------

type backendResponse struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	ModelName   string    `json:"model_name"`
	Kind        string    `json:"kind"`
	Enabled     bool      `json:"enabled"`
	Priority    int       `json:"priority"`
	Weight      int       `json:"weight"`
	MaxInFlight int       `json:"max_in_flight"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toBackendResponse(b models.ModelBackend) backendResponse {
	return backendResponse{
		ID:          b.ID,
		Name:        b.Name,
		BaseURL:     b.BaseURL,
		ModelName:   b.ModelName,
		Kind:        b.Kind.String(),
		Enabled:     b.Enabled,
		Priority:    b.Priority,
		Weight:      b.Weight,
		MaxInFlight: b.MaxInFlight,
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
	}
}

type policyResponse struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	ModelName string          `json:"model_name"`
	Strategy  string          `json:"strategy"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func toPolicyResponse(p models.RoutingPolicy) policyResponse {
	cfg := p.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	return policyResponse{
		ID:        p.ID,
		Name:      p.Name,
		ModelName: p.ModelName,
		Strategy:  p.Strategy,
		Config:    cfg,
		Enabled:   p.Enabled,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

// --- request shapes --------------------------------------------------------

// Pointer fields distinguish "field omitted" from "field set to zero value",
// which is what lets create enforce required fields and patch touch only the
// fields the caller actually sent.

type backendCreateRequest struct {
	Name        *string `json:"name"`
	BaseURL     *string `json:"base_url"`
	ModelName   *string `json:"model_name"`
	Kind        *string `json:"kind"`
	Enabled     *bool   `json:"enabled"`
	Priority    *int    `json:"priority"`
	Weight      *int    `json:"weight"`
	MaxInFlight *int    `json:"max_in_flight"`
}

// backendPatchRequest lists only the mutable columns; name, model_name, and
// kind are absent by design, so the admin API cannot rewrite a backend's
// identity.
type backendPatchRequest struct {
	BaseURL     *string `json:"base_url"`
	Enabled     *bool   `json:"enabled"`
	Priority    *int    `json:"priority"`
	Weight      *int    `json:"weight"`
	MaxInFlight *int    `json:"max_in_flight"`
}

type policyCreateRequest struct {
	Name      *string         `json:"name"`
	ModelName *string         `json:"model_name"`
	Strategy  *string         `json:"strategy"`
	Config    json.RawMessage `json:"config"`
	Enabled   *bool           `json:"enabled"`
}

// policyPatchRequest lists only the mutable columns; name, model_name, and
// strategy are absent by design.
type policyPatchRequest struct {
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

// --- backend handlers ------------------------------------------------------

func listBackends(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		backends, err := deps.Backends.List(r.Context())
		if err != nil {
			writeInternal(w, r, "could not list backends")
			return
		}
		out := make([]backendResponse, 0, len(backends))
		for _, b := range backends {
			out = append(out, toBackendResponse(b))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createBackend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req backendCreateRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		if msg, ok := validateBackendCreate(req); !ok {
			writeBadRequest(w, r, msg)
			return
		}

		kind, _ := models.ParseBackendKind(*req.Kind)
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		created, err := deps.Backends.Insert(r.Context(), models.ModelBackend{
			Name:        *req.Name,
			BaseURL:     *req.BaseURL,
			ModelName:   *req.ModelName,
			Kind:        kind,
			Enabled:     enabled,
			Priority:    *req.Priority,
			Weight:      *req.Weight,
			MaxInFlight: *req.MaxInFlight,
		})
		if err != nil {
			if db.IsUniqueViolation(err) {
				httperror.Write(w, r, http.StatusConflict, httperror.CodeConflict,
					"a backend with that name already exists")
				return
			}
			writeInternal(w, r, "could not create backend")
			return
		}
		writeJSON(w, http.StatusCreated, toBackendResponse(created))
	}
}

func patchBackend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var req backendPatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		current, err := deps.Backends.GetByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeNotFound(w, r, "backend not found")
				return
			}
			writeInternal(w, r, "could not load backend")
			return
		}

		// Apply only mutable fields onto the loaded row; name, model_name, and
		// kind are carried through untouched.
		if req.BaseURL != nil {
			current.BaseURL = *req.BaseURL
		}
		if req.Enabled != nil {
			current.Enabled = *req.Enabled
		}
		if req.Priority != nil {
			current.Priority = *req.Priority
		}
		if req.Weight != nil {
			current.Weight = *req.Weight
		}
		if req.MaxInFlight != nil {
			current.MaxInFlight = *req.MaxInFlight
		}

		if msg, ok := validateBackendValues(current); !ok {
			writeBadRequest(w, r, msg)
			return
		}

		updated, err := deps.Backends.Update(r.Context(), current)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeNotFound(w, r, "backend not found")
				return
			}
			writeInternal(w, r, "could not update backend")
			return
		}
		writeJSON(w, http.StatusOK, toBackendResponse(updated))
	}
}

// --- routing-policy handlers ----------------------------------------------

func listPolicies(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policies, err := deps.Policies.List(r.Context())
		if err != nil {
			writeInternal(w, r, "could not list routing policies")
			return
		}
		out := make([]policyResponse, 0, len(policies))
		for _, p := range policies {
			out = append(out, toPolicyResponse(p))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func createPolicy(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req policyCreateRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		if msg, ok := validatePolicyCreate(req); !ok {
			writeBadRequest(w, r, msg)
			return
		}

		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		created, err := deps.Policies.Insert(r.Context(), models.RoutingPolicy{
			Name:      *req.Name,
			ModelName: *req.ModelName,
			Strategy:  *req.Strategy,
			Config:    defaultConfig(req.Config),
			Enabled:   enabled,
		})
		if err != nil {
			if db.IsUniqueViolation(err) {
				httperror.Write(w, r, http.StatusConflict, httperror.CodeConflict,
					"a routing policy with that name already exists")
				return
			}
			writeInternal(w, r, "could not create routing policy")
			return
		}
		writeJSON(w, http.StatusCreated, toPolicyResponse(created))
	}
}

func patchPolicy(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var req policyPatchRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		current, err := deps.Policies.GetByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeNotFound(w, r, "routing policy not found")
				return
			}
			writeInternal(w, r, "could not load routing policy")
			return
		}

		if req.Config != nil {
			if !isJSONObject(req.Config) {
				writeBadRequest(w, r, "config must be a JSON object")
				return
			}
			current.Config = req.Config
		}
		if req.Enabled != nil {
			current.Enabled = *req.Enabled
		}

		updated, err := deps.Policies.Update(r.Context(), current)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeNotFound(w, r, "routing policy not found")
				return
			}
			writeInternal(w, r, "could not update routing policy")
			return
		}
		writeJSON(w, http.StatusOK, toPolicyResponse(updated))
	}
}

// --- validation ------------------------------------------------------------

// validateBackendCreate checks that every required field is present and every
// value is in range. It returns a client-facing message and false on the first
// problem found.
func validateBackendCreate(req backendCreateRequest) (string, bool) {
	if req.Name == nil || *req.Name == "" {
		return "name is required", false
	}
	if req.BaseURL == nil {
		return "base_url is required", false
	}
	if req.ModelName == nil || *req.ModelName == "" {
		return "model_name is required", false
	}
	if req.Kind == nil {
		return "kind is required", false
	}
	if req.Priority == nil {
		return "priority is required", false
	}
	if req.Weight == nil {
		return "weight is required", false
	}
	if req.MaxInFlight == nil {
		return "max_in_flight is required", false
	}
	return validateBackendValues(models.ModelBackend{
		BaseURL:     *req.BaseURL,
		Kind:        models.BackendKind(*req.Kind),
		Priority:    *req.Priority,
		Weight:      *req.Weight,
		MaxInFlight: *req.MaxInFlight,
	})
}

// validateBackendValues checks the value-level constraints shared by create and
// patch: kind vocabulary, absolute http/https URL, and the numeric ranges that
// mirror the model_backends CHECK constraints.
func validateBackendValues(b models.ModelBackend) (string, bool) {
	if !b.Kind.IsValid() {
		return "kind must be one of: openai_compatible, mock", false
	}
	if !isAbsoluteHTTPURL(b.BaseURL) {
		return "base_url must be an absolute http or https URL", false
	}
	if b.Priority < 0 {
		return "priority must be >= 0", false
	}
	if b.Weight <= 0 {
		return "weight must be > 0", false
	}
	if b.MaxInFlight <= 0 {
		return "max_in_flight must be > 0", false
	}
	return "", true
}

func validatePolicyCreate(req policyCreateRequest) (string, bool) {
	if req.Name == nil || *req.Name == "" {
		return "name is required", false
	}
	if req.ModelName == nil || *req.ModelName == "" {
		return "model_name is required", false
	}
	if req.Strategy == nil || *req.Strategy == "" {
		return "strategy is required", false
	}
	if *req.Strategy != models.StrategyPriorityWeighted {
		return "strategy must be priority_weighted", false
	}
	if req.Config != nil && !isJSONObject(req.Config) {
		return "config must be a JSON object", false
	}
	return "", true
}

// isAbsoluteHTTPURL reports whether raw is an absolute URL with an http/https
// scheme and a host.
func isAbsoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// isJSONObject reports whether raw is a syntactically valid JSON object (as
// opposed to an array, string, number, or null).
func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil
}

// defaultConfig returns raw when it carries content, or the "{}" default when
// the caller omitted config. Content validity is already enforced by
// validatePolicyCreate.
func defaultConfig(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// --- request helpers -------------------------------------------------------

// decodeJSON reads and decodes the request body into dst, writing a 400 and
// returning false on any read or parse error. The body is size-limited.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeBadRequest(w, r, "request body is required")
			return false
		}
		writeBadRequest(w, r, "invalid JSON body")
		return false
	}
	return true
}

// parseID reads and parses the {id} path parameter as a UUID, writing a 400 and
// returning false when it is missing or malformed.
func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeBadRequest(w, r, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func writeBadRequest(w http.ResponseWriter, r *http.Request, msg string) {
	httperror.Write(w, r, http.StatusBadRequest, httperror.CodeBadRequest, msg)
}

func writeNotFound(w http.ResponseWriter, r *http.Request, msg string) {
	httperror.Write(w, r, http.StatusNotFound, httperror.CodeNotFound, msg)
}

func writeInternal(w http.ResponseWriter, r *http.Request, msg string) {
	httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal, msg)
}
