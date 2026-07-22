package seed_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/seed"
)

// The four fakes record what the seeder upserts, keyed by name so a rerun
// overwrites rather than duplicates — mirroring the ON CONFLICT (name) repos.

type fakeTenants struct {
	byName map[string]models.Tenant
	calls  int
}

func (f *fakeTenants) Upsert(_ context.Context, name string) (models.Tenant, error) {
	f.calls++
	t, ok := f.byName[name]
	if !ok {
		t = models.Tenant{ID: uuid.New(), Name: name}
		f.byName[name] = t
	}
	return t, nil
}

type fakeKeys struct {
	byHash map[string]models.APIKey
	calls  int
}

func (f *fakeKeys) Upsert(_ context.Context, tenantID uuid.UUID, name, keyHash string) (models.APIKey, error) {
	f.calls++
	k := models.APIKey{ID: uuid.New(), TenantID: tenantID, Name: name, KeyHash: keyHash}
	f.byHash[keyHash] = k
	return k, nil
}

type fakeBackends struct {
	byName map[string]models.ModelBackend
	calls  int
}

func (f *fakeBackends) Upsert(_ context.Context, b models.ModelBackend) (models.ModelBackend, error) {
	f.calls++
	b.ID = uuid.New()
	f.byName[b.Name] = b
	return b, nil
}

type fakePolicies struct {
	byName map[string]models.RoutingPolicy
	calls  int
}

func (f *fakePolicies) Upsert(_ context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error) {
	f.calls++
	p.ID = uuid.New()
	f.byName[p.Name] = p
	return p, nil
}

func newRepos() (*fakeTenants, *fakeKeys, *fakeBackends, *fakePolicies, seed.Repos) {
	tenants := &fakeTenants{byName: map[string]models.Tenant{}}
	keys := &fakeKeys{byHash: map[string]models.APIKey{}}
	backends := &fakeBackends{byName: map[string]models.ModelBackend{}}
	policies := &fakePolicies{byName: map[string]models.RoutingPolicy{}}
	return tenants, keys, backends, policies, seed.Repos{
		Tenants:  tenants,
		Keys:     keys,
		Backends: backends,
		Policies: policies,
	}
}

func testConfig() *config.Config {
	return &config.Config{
		AppKeyHashSecret:    "0123456789abcdef0123456789abcdef",
		DevAPIKey:           "sg_dev_key_123",
		SeedBackendFastURL:  "http://mock-llm-fast:8081",
		SeedBackendCheapURL: "http://mock-llm-cheap:8082",
	}
}

func TestRunSeedsDeclaredState(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	tenants, keys, backends, policies, repos := newRepos()

	require.NoError(t, seed.Run(context.Background(), cfg, repos))

	// Tenant.
	require.Contains(t, tenants.byName, "demo")

	// API key stored only as its HMAC of (secret, dev key), never the raw key.
	wantHash := auth.HashAPIKey(cfg.AppKeyHashSecret, cfg.DevAPIKey)
	require.Contains(t, keys.byHash, wantHash)
	for hash := range keys.byHash {
		assert.NotContains(t, hash, cfg.DevAPIKey, "the raw key must never be stored")
	}
	assert.Equal(t, tenants.byName["demo"].ID, keys.byHash[wantHash].TenantID,
		"the api key must belong to the demo tenant")

	// Two backends, URLs sourced from config (not hardcoded), enabled, mock.
	require.Len(t, backends.byName, 2)
	fast := backends.byName["mock-llm-fast"]
	assert.Equal(t, cfg.SeedBackendFastURL, fast.BaseURL)
	assert.Equal(t, "llama-fast", fast.ModelName)
	assert.Equal(t, models.BackendKindMock, fast.Kind)
	assert.True(t, fast.Enabled)
	assert.Equal(t, 10, fast.Priority)
	assert.Equal(t, 1, fast.Weight)
	assert.Equal(t, 32, fast.MaxInFlight)

	cheap := backends.byName["mock-llm-cheap"]
	assert.Equal(t, cfg.SeedBackendCheapURL, cheap.BaseURL)
	assert.Equal(t, 20, cheap.Priority)

	// Default routing policy.
	require.Contains(t, policies.byName, "default")
	pol := policies.byName["default"]
	assert.Equal(t, "llama-fast", pol.ModelName)
	assert.Equal(t, models.StrategyPriorityWeighted, pol.Strategy)
	assert.True(t, pol.Enabled)
}

func TestRunIsIdempotent(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	tenants, _, backends, policies, repos := newRepos()

	require.NoError(t, seed.Run(context.Background(), cfg, repos))
	require.NoError(t, seed.Run(context.Background(), cfg, repos))

	// Re-running must not create duplicate rows: name-keyed maps stay at their
	// declared sizes even though Upsert was called twice per entity.
	assert.Len(t, tenants.byName, 1)
	assert.Len(t, backends.byName, 2)
	assert.Len(t, policies.byName, 1)
	assert.Equal(t, 2, tenants.calls, "seeder must call tenant upsert once per run")
}
