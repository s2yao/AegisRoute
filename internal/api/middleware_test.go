package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/httperror"
)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := do(t, router, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	got := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, got, "a request id must be generated when none is supplied")
	_, err := uuid.Parse(got)
	assert.NoError(t, err, "generated request id must be a UUID")
}

func TestRequestIDPreservedWhenPresent(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "client-supplied-123")
	rec := do(t, router, req)

	assert.Equal(t, "client-supplied-123", rec.Header().Get("X-Request-ID"),
		"an inbound request id must be preserved and echoed")
}

func TestRequestIDAlwaysOnErrorResponses(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	// A credential in the query is rejected with 400 before routing; the
	// response must still carry a request id on both the header and the body.
	req := httptest.NewRequest(http.MethodGet, "/healthz?api_key=leak", nil)
	rec := do(t, router, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	env := decodeError(t, rec.Body)
	assert.Equal(t, rec.Header().Get("X-Request-ID"), env.Error.RequestID,
		"the error body request_id must match the response header")
}

func TestRejectQueryCredentials(t *testing.T) {
	t.Parallel()

	// Every credential-bearing query name, in assorted cases, must be rejected.
	badParams := []string{
		"api_key", "apikey", "access_token", "token",
		"authorization", "admin_token", "x_admin_token", "x-api-key",
		"API_KEY", "Token", "X-Api-Key",
	}
	router := api.NewRouter(testDeps())

	for _, name := range badParams {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz?"+name+"=secret", nil)
			rec := do(t, router, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodeError(t, rec.Body)
			assert.Equal(t, httperror.CodeBadRequest, env.Error.Code)
		})
	}
}

func TestRecovererHandlesPanic(t *testing.T) {
	t.Parallel()

	deps := testDeps()
	deps.DBPinger = panicPinger{} // readyz will panic when it pings the DB
	router := api.NewRouter(deps)

	rec := do(t, router, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeError(t, rec.Body)
	assert.Equal(t, httperror.CodeInternal, env.Error.Code)
	// The recover path still surfaces a correlation id, on header and in body.
	require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	assert.Equal(t, rec.Header().Get("X-Request-ID"), env.Error.RequestID)
}

func TestBenignQueryParamAllowed(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	req := httptest.NewRequest(http.MethodGet, "/healthz?verbose=true", nil)
	rec := do(t, router, req)

	assert.Equal(t, http.StatusOK, rec.Code, "a non-credential query param must pass")
}
