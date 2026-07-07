package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/api"
	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/cache"
	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
)

const (
	testSecret     = "0123456789abcdef0123456789abcdef"
	testRawKey     = "sg_dev_key_123"
	testAdminToken = "correct-admin-token"
)

// testDeps builds an api.Deps wired entirely with in-memory fakes and healthy
// pingers. Individual tests override fields (Backends, Policies, pingers)
// before calling api.NewRouter.
func testDeps() api.Deps {
	hash := auth.HashAPIKey(testSecret, testRawKey)
	return api.Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:       metrics.New(),
		KeyHashSecret: testSecret,
		AdminToken:    testAdminToken,
		Keys: &fakeKeyStore{keys: map[string]*models.APIKey{
			hash: {ID: uuid.New(), TenantID: uuid.New()},
		}},
		Backends:    newFakeBackendStore(),
		Policies:    newFakePolicyStore(),
		DBPinger:    stubPinger{},
		RedisPinger: stubPinger{},
		Cache:       noopCache{},
		Limiter:     allowAllLimiter{},
		Idempotency: bypassIdempotency{},
	}
}

// --- benign Stage-5 pass-through fakes ---------------------------------------
// Tests not exercising cache/rate-limit/idempotency get these no-ops; the
// Stage-5 handler integration tests swap in the real implementations over
// miniredis (see chat_stage5_test.go).

// noopCache always misses and stores nothing.
type noopCache struct{}

func (noopCache) Get(context.Context, string) (*cache.Entry, error) { return nil, nil }
func (noopCache) Put(context.Context, string, cache.Entry) error    { return nil }

// allowAllLimiter never limits.
type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, string) (bool, error) { return true, nil }

// bypassIdempotency proceeds without ever creating records.
type bypassIdempotency struct{}

func (bypassIdempotency) Check(context.Context, string, string, string) (idempotency.Decision, error) {
	return idempotency.Decision{Action: idempotency.ActionProceed}, nil
}

func (bypassIdempotency) Begin(context.Context, string, string, string) (idempotency.Decision, error) {
	return idempotency.Decision{Action: idempotency.ActionProceed}, nil
}

func (bypassIdempotency) Complete(context.Context, uuid.UUID, int, map[string]string, []byte) error {
	return nil
}

// decodeError parses a response body into the standard error envelope.
func decodeError(t *testing.T, body io.Reader) struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
} {
	t.Helper()
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(body).Decode(&env))
	return env
}

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- fakes -----------------------------------------------------------------

type fakeKeyStore struct {
	keys map[string]*models.APIKey
	err  error
}

func (f *fakeKeyStore) GetByHash(_ context.Context, hash string) (*models.APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	k, ok := f.keys[hash]
	if !ok {
		return nil, db.ErrNotFound
	}
	return k, nil
}

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

// panicPinger panics on Ping, used to drive the recover middleware through the
// public router (the readyz handler calls a Pinger).
type panicPinger struct{}

func (panicPinger) Ping(context.Context) error { panic("boom") }

// fakeBackendStore is an in-memory BackendStore. It preserves insertion order
// for List/ListEnabled and can be primed with errors to drive failure paths.
type fakeBackendStore struct {
	order     []uuid.UUID
	byID      map[uuid.UUID]models.ModelBackend
	insertErr error
	listErr   error
}

func newFakeBackendStore(seed ...models.ModelBackend) *fakeBackendStore {
	f := &fakeBackendStore{byID: map[uuid.UUID]models.ModelBackend{}}
	for _, b := range seed {
		if b.ID == uuid.Nil {
			b.ID = uuid.New()
		}
		f.order = append(f.order, b.ID)
		f.byID[b.ID] = b
	}
	return f
}

func (f *fakeBackendStore) Insert(_ context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	if f.insertErr != nil {
		return models.ModelBackend{}, f.insertErr
	}
	b.ID = uuid.New()
	f.order = append(f.order, b.ID)
	f.byID[b.ID] = b
	return b, nil
}

func (f *fakeBackendStore) Update(_ context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	if _, ok := f.byID[b.ID]; !ok {
		return models.ModelBackend{}, db.ErrNotFound
	}
	f.byID[b.ID] = b
	return b, nil
}

func (f *fakeBackendStore) GetByID(_ context.Context, id uuid.UUID) (models.ModelBackend, error) {
	b, ok := f.byID[id]
	if !ok {
		return models.ModelBackend{}, db.ErrNotFound
	}
	return b, nil
}

func (f *fakeBackendStore) List(context.Context) ([]models.ModelBackend, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]models.ModelBackend, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.byID[id])
	}
	return out, nil
}

func (f *fakeBackendStore) ListEnabled(context.Context) ([]models.ModelBackend, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]models.ModelBackend, 0, len(f.order))
	for _, id := range f.order {
		if b := f.byID[id]; b.Enabled {
			out = append(out, b)
		}
	}
	return out, nil
}

// fakePolicyStore is an in-memory PolicyStore mirroring fakeBackendStore.
type fakePolicyStore struct {
	order []uuid.UUID
	byID  map[uuid.UUID]models.RoutingPolicy
}

func newFakePolicyStore(seed ...models.RoutingPolicy) *fakePolicyStore {
	f := &fakePolicyStore{byID: map[uuid.UUID]models.RoutingPolicy{}}
	for _, p := range seed {
		if p.ID == uuid.Nil {
			p.ID = uuid.New()
		}
		f.order = append(f.order, p.ID)
		f.byID[p.ID] = p
	}
	return f
}

func (f *fakePolicyStore) Insert(_ context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	p.ID = uuid.New()
	f.order = append(f.order, p.ID)
	f.byID[p.ID] = p
	return p, nil
}

func (f *fakePolicyStore) Update(_ context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	if _, ok := f.byID[p.ID]; !ok {
		return models.RoutingPolicy{}, db.ErrNotFound
	}
	f.byID[p.ID] = p
	return p, nil
}

func (f *fakePolicyStore) GetByID(_ context.Context, id uuid.UUID) (models.RoutingPolicy, error) {
	p, ok := f.byID[id]
	if !ok {
		return models.RoutingPolicy{}, db.ErrNotFound
	}
	return p, nil
}

func (f *fakePolicyStore) List(context.Context) ([]models.RoutingPolicy, error) {
	out := make([]models.RoutingPolicy, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.byID[id])
	}
	return out, nil
}
