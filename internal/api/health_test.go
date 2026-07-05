package api_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/httperror"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	router := api.NewRouter(testDeps())
	rec := do(t, router, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		db         stubPinger
		redis      stubPinger
		wantStatus int
	}{
		{"both healthy", stubPinger{}, stubPinger{}, http.StatusOK},
		{"db down", stubPinger{err: errors.New("pg down")}, stubPinger{}, http.StatusServiceUnavailable},
		{"redis down", stubPinger{}, stubPinger{err: errors.New("redis down")}, http.StatusServiceUnavailable},
		{"both down", stubPinger{err: errors.New("pg")}, stubPinger{err: errors.New("redis")}, http.StatusServiceUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps()
			deps.DBPinger = tc.db
			deps.RedisPinger = tc.redis
			router := api.NewRouter(deps)

			rec := do(t, router, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus == http.StatusServiceUnavailable {
				env := decodeError(t, rec.Body)
				assert.Equal(t, httperror.CodeUpstreamUnavailable, env.Error.Code)
			}
		})
	}
}
