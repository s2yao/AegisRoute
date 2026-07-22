// Package seed idempotently inserts the demo tenant, API key, model backends,
// and routing policy. It is the single seeding path, invoked by
// `gateway-api -seed` and, when AEGISROUTE_AUTO_SEED is set, on server startup.
// Seeding never lives in SQL migrations: migration files are permanent, and
// credentials are not.
package seed

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/config"
	"github.com/example/aegisroute/internal/models"
)

// Declared demo state. The seeder converges the database to exactly these
// values every run, so re-seeding is always safe and always leaves the local
// stack in a known-good configuration. Backend base URLs are NOT here: they
// come from config so the same seed works for host runs (localhost) and
// compose runs (service names).
const (
	demoTenantName    = "demo"
	demoAPIKeyName    = "demo-key"
	demoModelName     = "llama-fast"
	fastBackendName   = "mock-llm-fast"
	cheapBackendName  = "mock-llm-cheap"
	defaultPolicyName = "default"
)

// TenantUpserter upserts a tenant by name. Satisfied by *db.TenantRepo.
type TenantUpserter interface {
	Upsert(ctx context.Context, name string) (models.Tenant, error)
}

// KeyUpserter upserts an API key by its hash. Satisfied by *db.APIKeyRepo.
type KeyUpserter interface {
	Upsert(ctx context.Context, tenantID uuid.UUID, name, keyHash string) (models.APIKey, error)
}

// BackendUpserter upserts a model backend by name. Satisfied by
// *db.BackendRepo.
type BackendUpserter interface {
	Upsert(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error)
}

// PolicyUpserter upserts a routing policy by name. Satisfied by
// *db.RoutingPolicyRepo.
type PolicyUpserter interface {
	Upsert(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error)
}

// Repos bundles the four upserters Run needs. The composition root fills it
// with real repositories; tests fill it with fakes.
type Repos struct {
	Tenants  TenantUpserter
	Keys     KeyUpserter
	Backends BackendUpserter
	Policies PolicyUpserter
}

// Run idempotently seeds the demo data: the "demo" tenant, an API key whose
// stored hash is HashAPIKey(APP_KEY_HASH_SECRET, DEV_API_KEY), two enabled mock
// backends serving "llama-fast", and a default priority_weighted routing
// policy. It is safe to run repeatedly. Seeding does not require Redis, only
// the database repositories in repos.
func Run(ctx context.Context, cfg *config.Config, repos Repos) error {
	tenant, err := repos.Tenants.Upsert(ctx, demoTenantName)
	if err != nil {
		return fmt.Errorf("seed: tenant: %w", err)
	}

	keyHash := auth.HashAPIKey(cfg.AppKeyHashSecret, cfg.DevAPIKey)
	if _, err := repos.Keys.Upsert(ctx, tenant.ID, demoAPIKeyName, keyHash); err != nil {
		return fmt.Errorf("seed: api key: %w", err)
	}

	backends := []models.ModelBackend{
		{
			Name:        fastBackendName,
			BaseURL:     cfg.SeedBackendFastURL,
			ModelName:   demoModelName,
			Kind:        models.BackendKindMock,
			Enabled:     true,
			Priority:    10,
			Weight:      1,
			MaxInFlight: 32,
		},
		{
			Name:        cheapBackendName,
			BaseURL:     cfg.SeedBackendCheapURL,
			ModelName:   demoModelName,
			Kind:        models.BackendKindMock,
			Enabled:     true,
			Priority:    20,
			Weight:      1,
			MaxInFlight: 32,
		},
	}
	for _, b := range backends {
		if _, err := repos.Backends.Upsert(ctx, b); err != nil {
			return fmt.Errorf("seed: backend %q: %w", b.Name, err)
		}
	}

	if _, err := repos.Policies.Upsert(ctx, models.RoutingPolicy{
		Name:      defaultPolicyName,
		ModelName: demoModelName,
		Strategy:  models.StrategyPriorityWeighted,
		Config:    []byte("{}"),
		Enabled:   true,
	}); err != nil {
		return fmt.Errorf("seed: routing policy: %w", err)
	}

	return nil
}
