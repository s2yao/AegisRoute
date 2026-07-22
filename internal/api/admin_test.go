package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/models"
)

// adminReq builds a request carrying the correct admin token and JSON body.
func adminReq(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Admin-Token", testAdminToken)
	return req
}

func validBackendBody() map[string]any {
	return map[string]any{
		"name":          "mock-llm-fast",
		"base_url":      "http://mock-llm-fast:8081",
		"model_name":    "llama-fast",
		"kind":          "mock",
		"enabled":       true,
		"priority":      10,
		"weight":        1,
		"max_in_flight": 32,
	}
}

func TestAdminRequiresToken(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())

	// No token at all.
	rec := do(t, router, httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, httperror.CodeUnauthorized, env.Error.Code)
	assert.NotEmpty(t, env.Error.RequestID, "401 body must carry a request id")

	// Wrong token.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	req.Header.Set("X-Admin-Token", "nope")
	rec = do(t, router, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestCreateBackend(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/backends", validBackendBody()))

	require.Equal(t, http.StatusCreated, rec.Code)

	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.NotEmpty(t, got["id"])
	assert.Equal(t, "mock-llm-fast", got["name"])
	assert.Equal(t, "http://mock-llm-fast:8081", got["base_url"])
	assert.Equal(t, "llama-fast", got["model_name"])
	assert.Equal(t, "mock", got["kind"])
	assert.Equal(t, true, got["enabled"])
	assert.Equal(t, float64(10), got["priority"])
	assert.Equal(t, float64(1), got["weight"])
	assert.Equal(t, float64(32), got["max_in_flight"])
	assert.Contains(t, got, "created_at")
	assert.Contains(t, got, "updated_at")
}

func TestCreateBackendDefaultsEnabledTrue(t *testing.T) {
	t.Parallel()

	body := validBackendBody()
	delete(body, "enabled")
	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/backends", body))

	require.Equal(t, http.StatusCreated, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, true, got["enabled"], "enabled must default to true when omitted")
}

func TestCreateBackendValidation(t *testing.T) {
	t.Parallel()

	tweaks := map[string]func(map[string]any){
		"invalid kind":          func(b map[string]any) { b["kind"] = "sqlite" },
		"non-http url":          func(b map[string]any) { b["base_url"] = "ftp://x/y" },
		"relative url":          func(b map[string]any) { b["base_url"] = "/relative" },
		"zero weight":           func(b map[string]any) { b["weight"] = 0 },
		"negative priority":     func(b map[string]any) { b["priority"] = -1 },
		"zero max_in_flight":    func(b map[string]any) { b["max_in_flight"] = 0 },
		"missing name":          func(b map[string]any) { delete(b, "name") },
		"missing model_name":    func(b map[string]any) { delete(b, "model_name") },
		"missing priority":      func(b map[string]any) { delete(b, "priority") },
		"missing max_in_flight": func(b map[string]any) { delete(b, "max_in_flight") },
	}

	router := api.NewRouter(testDeps())
	for name, tweak := range tweaks {
		t.Run(name, func(t *testing.T) {
			body := validBackendBody()
			tweak(body)
			rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/backends", body))

			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodeError(t, rec.Body)
			assert.Equal(t, httperror.CodeBadRequest, env.Error.Code)
			assert.NotEmpty(t, env.Error.Message)
		})
	}
}

func TestPatchBackendSoftDisablePreservesImmutables(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	store := newFakeBackendStore(models.ModelBackend{
		Name: "mock-llm-fast", BaseURL: "http://mock-llm-fast:8081", ModelName: "llama-fast",
		Kind: models.BackendKindMock, Enabled: true, Priority: 10, Weight: 1, MaxInFlight: 32,
	})
	deps.Backends = store
	router := api.NewRouter(deps)

	id := store.order[0]
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/backends/"+id.String(),
		map[string]any{"enabled": false}))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, false, got["enabled"], "PATCH must soft-disable")
	// Immutable fields untouched.
	assert.Equal(t, "mock-llm-fast", got["name"])
	assert.Equal(t, "llama-fast", got["model_name"])
	assert.Equal(t, "mock", got["kind"])
	assert.Equal(t, id.String(), got["id"])

	// The list now reflects the disabled state.
	rec = do(t, router, adminReq(t, http.MethodGet, "/api/v1/backends", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	require.Len(t, list, 1)
	assert.Equal(t, false, list[0]["enabled"])
}

func TestPatchBackendCannotMutateImmutableFields(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	store := newFakeBackendStore(models.ModelBackend{
		Name: "mock-llm-fast", BaseURL: "http://a:1", ModelName: "llama-fast",
		Kind: models.BackendKindMock, Enabled: true, Priority: 10, Weight: 1, MaxInFlight: 32,
	})
	deps.Backends = store
	router := api.NewRouter(deps)

	id := store.order[0]
	// Attempt to sneak name/model_name/kind through PATCH; they must be ignored.
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/backends/"+id.String(),
		map[string]any{"name": "hacked", "model_name": "gpt", "kind": "openai_compatible", "priority": 5}))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, "mock-llm-fast", got["name"])
	assert.Equal(t, "llama-fast", got["model_name"])
	assert.Equal(t, "mock", got["kind"])
	assert.Equal(t, float64(5), got["priority"], "the mutable field must still apply")
}

func TestPatchBackendNotFound(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/backends/"+uuid.New().String(),
		map[string]any{"enabled": false}))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, httperror.CodeNotFound, env.Error.Code)
}

func TestPatchBackendInvalidID(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/backends/not-a-uuid",
		map[string]any{"enabled": false}))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreatePolicy(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/routing-policies", map[string]any{
		"name": "default", "model_name": "llama-fast", "strategy": "priority_weighted",
		"config": map[string]any{}, "enabled": true,
	}))

	require.Equal(t, http.StatusCreated, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.NotEmpty(t, got["id"])
	assert.Equal(t, "default", got["name"])
	assert.Equal(t, "llama-fast", got["model_name"])
	assert.Equal(t, "priority_weighted", got["strategy"])
	assert.Equal(t, map[string]any{}, got["config"])
	assert.Equal(t, true, got["enabled"])
}

func TestCreatePolicyDefaultsConfigAndEnabled(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/routing-policies", map[string]any{
		"name": "default", "model_name": "llama-fast", "strategy": "priority_weighted",
	}))

	require.Equal(t, http.StatusCreated, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, map[string]any{}, got["config"], "config must default to {}")
	assert.Equal(t, true, got["enabled"], "enabled must default to true")
}

func TestCreatePolicyValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]any{
		"bad strategy":    {"name": "x", "model_name": "m", "strategy": "round_robin"},
		"missing name":    {"model_name": "m", "strategy": "priority_weighted"},
		"missing model":   {"name": "x", "strategy": "priority_weighted"},
		"config as array": {"name": "x", "model_name": "m", "strategy": "priority_weighted", "config": []int{1, 2}},
		"config null":     {"name": "x", "model_name": "m", "strategy": "priority_weighted", "config": nil},
	}

	router := api.NewRouter(testDeps())
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/routing-policies", body))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodeError(t, rec.Body)
			assert.Equal(t, httperror.CodeBadRequest, env.Error.Code)
		})
	}
}

func TestPatchPolicySoftDisablePreservesImmutables(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	store := newFakePolicyStore(models.RoutingPolicy{
		Name: "default", ModelName: "llama-fast", Strategy: models.StrategyPriorityWeighted,
		Config: json.RawMessage(`{}`), Enabled: true,
	})
	deps.Policies = store
	router := api.NewRouter(deps)

	id := store.order[0]
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/routing-policies/"+id.String(),
		map[string]any{"enabled": false}))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, false, got["enabled"])
	assert.Equal(t, "default", got["name"])
	assert.Equal(t, "llama-fast", got["model_name"])
	assert.Equal(t, "priority_weighted", got["strategy"])
}

func TestPatchPolicyRejectsNullConfig(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	store := newFakePolicyStore(models.RoutingPolicy{
		Name: "default", ModelName: "llama-fast", Strategy: models.StrategyPriorityWeighted,
		Config: json.RawMessage(`{}`), Enabled: true,
	})
	deps.Policies = store
	router := api.NewRouter(deps)

	id := store.order[0]
	// config: null is not a JSON object and must be rejected, not stored.
	rec := do(t, router, adminReq(t, http.MethodPatch, "/api/v1/routing-policies/"+id.String(),
		map[string]any{"config": nil}))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, httperror.CodeBadRequest, env.Error.Code)
}

func TestCreateBackendBadJSON(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", bytes.NewBufferString("{not json"))
	req.Header.Set("X-Admin-Token", testAdminToken)
	rec := do(t, router, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateBackendConflict(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	deps.Backends = &fakeBackendStore{
		byID:      map[uuid.UUID]models.ModelBackend{},
		insertErr: &pgconn.PgError{Code: "23505"},
	}
	router := api.NewRouter(deps)
	rec := do(t, router, adminReq(t, http.MethodPost, "/api/v1/backends", validBackendBody()))

	require.Equal(t, http.StatusConflict, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, httperror.CodeConflict, env.Error.Code)
}
