package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/models"
)

const (
	testSecret = "0123456789abcdef0123456789abcdef"
	testRawKey = "sg_dev_key_123"
	// testKeyHash is HMAC-SHA256(testSecret, testRawKey), hex-encoded, computed
	// out-of-band. Pinning it guards against an accidental change to the hashing
	// construction that would silently invalidate every stored key.
	testKeyHash = "51c4f733dcb413e46d261996ce5bd45311deb804a3e34274bf30d20b12fa5545"

	testAdminToken = "correct-admin-token"
)

// fakeKeyStore is the consumer-declared KeyStore for tests: a map from key
// hash to row, plus an optional infrastructure error to exercise the 500 path.
type fakeKeyStore struct {
	keys map[string]*models.APIKey
	err  error
}

func (f fakeKeyStore) GetByHash(_ context.Context, hash string) (*models.APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	k, ok := f.keys[hash]
	if !ok {
		return nil, db.ErrNotFound
	}
	return k, nil
}

func TestHashAPIKey(t *testing.T) {
	t.Parallel()

	got := auth.HashAPIKey(testSecret, testRawKey)

	assert.Equal(t, testKeyHash, got, "hash must match the precomputed HMAC-SHA256 vector")
	assert.Equal(t, got, auth.HashAPIKey(testSecret, testRawKey), "hash must be deterministic")
	assert.NotEqual(t, got, auth.HashAPIKey("different-secret-padding-to-32ch!", testRawKey),
		"a different secret must produce a different hash")
}

func TestBearerAuth(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	keyID := uuid.New()
	validStore := fakeKeyStore{keys: map[string]*models.APIKey{
		testKeyHash: {ID: keyID, TenantID: tenantID},
	}}

	tests := []struct {
		name       string
		store      auth.KeyStore
		authHeader string
		wantStatus int
	}{
		{"missing header", validStore, "", http.StatusUnauthorized},
		{"wrong scheme", validStore, "Basic " + testRawKey, http.StatusUnauthorized},
		{"empty bearer", validStore, "Bearer ", http.StatusUnauthorized},
		{"unknown key", fakeKeyStore{keys: map[string]*models.APIKey{}}, "Bearer " + testRawKey, http.StatusUnauthorized},
		{"infra error", fakeKeyStore{err: errors.New("db down")}, "Bearer " + testRawKey, http.StatusInternalServerError},
		{"valid key", validStore, "Bearer " + testRawKey, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPrincipal auth.Principal
			var sawPrincipal bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPrincipal, sawPrincipal = auth.PrincipalFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			handler := auth.BearerAuth(testSecret, tc.store)(next)

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus == http.StatusOK {
				require.True(t, sawPrincipal, "successful auth must attach a Principal")
				assert.Equal(t, tenantID, gotPrincipal.TenantID)
				assert.Equal(t, keyID, gotPrincipal.APIKeyID)
			}
		})
	}
}

func TestAdminAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token same length", "wronggg-admin-token", http.StatusUnauthorized},
		{"wrong token different length", "nope", http.StatusUnauthorized},
		{"correct token", testAdminToken, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			handler := auth.AdminAuth(testAdminToken)(next)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
			if tc.token != "" {
				req.Header.Set("X-Admin-Token", tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

// TestAdminAuthEmptyConfiguredTokenRejectsAll guards the ConstantTimeCompare
// edge case: with an unset admin token, an empty presented token must not slip
// through (subtle.ConstantTimeCompare("","") returns 1).
func TestAdminAuthEmptyConfiguredTokenRejectsAll(t *testing.T) {
	t.Parallel()

	handler := auth.AdminAuth("")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
