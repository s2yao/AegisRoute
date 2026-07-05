package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/models"
)

func bearer(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	return req
}

func TestModelsRequiresBearer(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestModelsListsEnabledDeduplicated(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	deps.Backends = newFakeBackendStore(
		models.ModelBackend{Name: "fast", ModelName: "llama-fast", Kind: models.BackendKindMock, Enabled: true, Priority: 10, Weight: 1, MaxInFlight: 1},
		models.ModelBackend{Name: "cheap", ModelName: "llama-fast", Kind: models.BackendKindMock, Enabled: true, Priority: 20, Weight: 1, MaxInFlight: 1},
		models.ModelBackend{Name: "big", ModelName: "llama-big", Kind: models.BackendKindMock, Enabled: true, Priority: 30, Weight: 1, MaxInFlight: 1},
		models.ModelBackend{Name: "disabled", ModelName: "llama-secret", Kind: models.BackendKindMock, Enabled: false, Priority: 40, Weight: 1, MaxInFlight: 1},
	)
	router := api.NewRouter(deps)

	rec := do(t, router, bearer(httptest.NewRequest(http.MethodGet, "/v1/models", nil)))
	require.Equal(t, http.StatusOK, rec.Code)

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))

	assert.Equal(t, "list", out.Object)
	// llama-fast de-duplicated to one entry; disabled backend's model excluded.
	require.Len(t, out.Data, 2)
	ids := []string{out.Data[0].ID, out.Data[1].ID}
	assert.ElementsMatch(t, []string{"llama-fast", "llama-big"}, ids)
	for _, m := range out.Data {
		assert.Equal(t, "model", m.Object)
		assert.Equal(t, "aegisroute", m.OwnedBy)
	}
}

func TestModelsEmptyWhenNoBackends(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, bearer(httptest.NewRequest(http.MethodGet, "/v1/models", nil)))
	require.Equal(t, http.StatusOK, rec.Code)

	var out struct {
		Object string        `json:"object"`
		Data   []interface{} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	assert.Equal(t, "list", out.Object)
	assert.Empty(t, out.Data, "data must be an empty array, not null, when no backends exist")
}
