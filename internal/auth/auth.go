package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/models"
)

// bearerPrefix is the exact scheme (with its trailing space) accepted in the
// Authorization header. The scheme is matched case-insensitively but the space
// is required, so "Bearer<key>" and "Bearertoken" are rejected.
const bearerPrefix = "Bearer "

// adminTokenHeader carries the admin token. A header (never a query parameter)
// is used deliberately so the token stays out of logs, proxies, and browser
// history; observability.Redact treats "x-admin-token" as secret-bearing.
const adminTokenHeader = "X-Admin-Token"

// KeyStore resolves a presented key's HMAC hash to its stored row. It is
// satisfied directly by *db.APIKeyRepo. A nil *models.APIKey paired with
// db.ErrNotFound means the hash is unknown (→ 401); any other non-nil error is
// an infrastructure failure (→ 500). The two are never conflated.
type KeyStore interface {
	GetByHash(ctx context.Context, keyHash string) (*models.APIKey, error)
}

// Principal is the authenticated caller's identity, attached to the request
// context by BearerAuth so downstream handlers know the tenant and API key
// without re-reading the Authorization header.
type Principal struct {
	TenantID uuid.UUID
	APIKeyID uuid.UUID
}

type principalKey struct{}

// ContextWithPrincipal returns a child context carrying p.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the Principal stored by BearerAuth and whether
// one was present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// BearerAuth guards a route group with bearer API-key authentication. It reads
// "Authorization: Bearer <raw_key>", hashes the raw key with HashAPIKey, and
// resolves it through keys. A missing or malformed header, an unsupported
// scheme, an empty bearer value, or an unknown hash all yield 401; only an
// infrastructure error from the store yields 500. On success it attaches the
// caller's Principal to the request context. The raw key, its hash, and secret
// are never logged.
func BearerAuth(secret string, keys KeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httperror.Write(w, r, http.StatusUnauthorized, httperror.CodeUnauthorized,
					"missing or malformed bearer token")
				return
			}
			key, err := keys.GetByHash(r.Context(), HashAPIKey(secret, raw))
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					httperror.Write(w, r, http.StatusUnauthorized, httperror.CodeUnauthorized,
						"invalid API key")
					return
				}
				httperror.Write(w, r, http.StatusInternalServerError, httperror.CodeInternal,
					"authentication backend error")
				return
			}
			ctx := ContextWithPrincipal(r.Context(), Principal{
				TenantID: key.TenantID,
				APIKeyID: key.ID,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AdminAuth guards a route group with the shared admin token presented in the
// X-Admin-Token header. The comparison uses crypto/subtle.ConstantTimeCompare
// so a wrong token cannot be discovered byte-by-byte via timing. A missing or
// mismatched token yields 401; an empty configured token rejects everything.
func AdminAuth(adminToken string) func(http.Handler) http.Handler {
	expected := []byte(adminToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := []byte(r.Header.Get(adminTokenHeader))
			// A zero-length expected token would make ConstantTimeCompare("","")
			// return 1, so guard it explicitly: an unset admin token authorizes
			// nobody.
			if len(expected) == 0 || subtle.ConstantTimeCompare(presented, expected) != 1 {
				httperror.Write(w, r, http.StatusUnauthorized, httperror.CodeUnauthorized,
					"invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the raw credential from the Authorization header. It
// returns ("", false) for a missing header, a non-Bearer scheme, or an empty
// value after the scheme. The scheme match is case-insensitive but the single
// separating space is mandatory.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
