package routing

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/models"
)

// fakeBackendStore returns a fixed candidate list per model, mimicking the
// repository's ListByModelEnabled.
type fakeBackendStore struct {
	byModel map[string][]models.ModelBackend
	err     error
}

func (f *fakeBackendStore) ListByModelEnabled(_ context.Context, model string) ([]models.ModelBackend, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byModel[model], nil
}

// fakePolicyStore returns one fixed policy or error for every model.
type fakePolicyStore struct {
	policy models.RoutingPolicy
	err    error
}

func (f *fakePolicyStore) GetForModel(context.Context, string) (models.RoutingPolicy, error) {
	if f.err != nil {
		return models.RoutingPolicy{}, f.err
	}
	return f.policy, nil
}

// be builds an enabled test backend with sane defaults.
func be(name string, priority, weight, maxInFlight int) models.ModelBackend {
	return models.ModelBackend{
		ID:          uuid.New(),
		Name:        name,
		BaseURL:     "http://" + name,
		ModelName:   "llama-fast",
		Kind:        models.BackendKindMock,
		Enabled:     true,
		Priority:    priority,
		Weight:      weight,
		MaxInFlight: maxInFlight,
	}
}

func newTestSelector(backends []models.ModelBackend, policies PolicyStore, seed int64) *Selector {
	return NewSelector(
		&fakeBackendStore{byModel: map[string][]models.ModelBackend{"llama-fast": backends}},
		policies,
		NewBreaker(3, time.Minute),
		WithRandSource(rand.NewSource(seed)),
	)
}

// notFoundPolicies is the "no enabled policy" store.
var notFoundPolicies = &fakePolicyStore{err: db.ErrNotFound}

func TestSelectAppliesEnabledPolicy(t *testing.T) {
	s := newTestSelector([]models.ModelBackend{be("a", 10, 1, 4)}, &fakePolicyStore{
		policy: models.RoutingPolicy{Name: "prod-routing", Strategy: models.StrategyPriorityWeighted},
	}, 1)

	sel, release, err := s.Select(context.Background(), "llama-fast")
	require.NoError(t, err)
	defer release()
	assert.Equal(t, "prod-routing", sel.PolicyName, "the enabled policy's name feeds the response header")
	assert.Equal(t, models.StrategyPriorityWeighted, sel.Strategy)
	assert.Equal(t, "a", sel.Backend.Name)
}

func TestSelectFallsBackToDefaultPolicy(t *testing.T) {
	s := newTestSelector([]models.ModelBackend{be("a", 10, 1, 4)}, notFoundPolicies, 1)

	sel, release, err := s.Select(context.Background(), "llama-fast")
	require.NoError(t, err)
	defer release()
	assert.Equal(t, "default", sel.PolicyName)
	assert.Equal(t, models.StrategyPriorityWeighted, sel.Strategy)
}

func TestSelectPolicyStoreErrorPropagates(t *testing.T) {
	s := newTestSelector([]models.ModelBackend{be("a", 10, 1, 4)},
		&fakePolicyStore{err: errors.New("pg down")}, 1)

	_, _, err := s.Select(context.Background(), "llama-fast")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoBackends)
	assert.NotErrorIs(t, err, ErrNoCapacity)
}

func TestSelectBackendStoreErrorPropagates(t *testing.T) {
	s := NewSelector(&fakeBackendStore{err: errors.New("pg down")}, notFoundPolicies,
		NewBreaker(3, time.Minute))
	_, _, err := s.Select(context.Background(), "llama-fast")
	require.Error(t, err)
}

func TestSelectNoBackendsForModel(t *testing.T) {
	s := newTestSelector(nil, notFoundPolicies, 1)
	_, _, err := s.Select(context.Background(), "llama-fast")
	assert.ErrorIs(t, err, ErrNoBackends)
}

func TestSelectSkipsDisabledAndInvalidRows(t *testing.T) {
	disabled := be("disabled", 1, 1, 4)
	disabled.Enabled = false
	zeroWeight := be("zero-weight", 1, 0, 4)
	negPriority := be("neg-priority", -1, 1, 4)
	zeroInFlight := be("zero-in-flight", 1, 1, 0)
	valid := be("valid", 99, 1, 4)

	s := newTestSelector([]models.ModelBackend{
		disabled, zeroWeight, negPriority, zeroInFlight, valid}, notFoundPolicies, 1)

	sel, release, err := s.Select(context.Background(), "llama-fast")
	require.NoError(t, err)
	defer release()
	assert.Equal(t, "valid", sel.Backend.Name,
		"defensive filter must skip disabled/invalid rows even at worse priority")

	// With only invalid rows the model is effectively unserved.
	s2 := newTestSelector([]models.ModelBackend{disabled, zeroWeight}, notFoundPolicies, 1)
	_, _, err = s2.Select(context.Background(), "llama-fast")
	assert.ErrorIs(t, err, ErrNoBackends)
}

func TestSelectPrefersLowerPriority(t *testing.T) {
	primary := be("primary", 10, 1, 8)
	secondary := be("secondary", 20, 100, 8)
	s := newTestSelector([]models.ModelBackend{secondary, primary}, notFoundPolicies, 42)

	for range 20 {
		sel, release, err := s.Select(context.Background(), "llama-fast")
		require.NoError(t, err)
		assert.Equal(t, "primary", sel.Backend.Name,
			"lower priority number always wins regardless of weight")
		release()
	}
}

func TestSelectExcludesTriedBackends(t *testing.T) {
	primary := be("primary", 10, 1, 8)
	secondary := be("secondary", 20, 1, 8)
	s := newTestSelector([]models.ModelBackend{primary, secondary}, notFoundPolicies, 1)
	ctx := context.Background()

	// No exclusion: the preferred (lower-priority) backend is chosen.
	sel1, release1, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	assert.Equal(t, "primary", sel1.Backend.Name)
	release1()

	// Excluding the primary hands back the untried secondary — the mechanism
	// the handler uses to fail over within one request.
	sel2, release2, err := s.Select(ctx, "llama-fast", primary.ID)
	require.NoError(t, err)
	assert.Equal(t, "secondary", sel2.Backend.Name)
	release2()

	// Excluding every backend for the model leaves no fresh candidate.
	_, _, err = s.Select(ctx, "llama-fast", primary.ID, secondary.ID)
	assert.ErrorIs(t, err, ErrNoCapacity,
		"once every candidate is excluded, failover is exhausted")
}

func TestSelectExcludeFreesSlotOfSkippedBackend(t *testing.T) {
	// A backend skipped by exclusion must not have its semaphore consumed, or
	// failover would leak capacity on the very backends it skips.
	primary := be("primary", 10, 1, 1)
	secondary := be("secondary", 20, 1, 1)
	s := newTestSelector([]models.ModelBackend{primary, secondary}, notFoundPolicies, 1)
	ctx := context.Background()

	sel, release, err := s.Select(ctx, "llama-fast", primary.ID)
	require.NoError(t, err)
	assert.Equal(t, "secondary", sel.Backend.Name)
	assert.Equal(t, 0, s.InFlight(primary.ID), "an excluded backend's slot is never taken")
	assert.Equal(t, 1, s.InFlight(secondary.ID))
	release()
}

func TestSelectSkipsOpenCircuit(t *testing.T) {
	primary := be("primary", 10, 1, 8)
	secondary := be("secondary", 20, 1, 8)
	breaker := NewBreaker(1, time.Minute)
	s := NewSelector(
		&fakeBackendStore{byModel: map[string][]models.ModelBackend{
			"llama-fast": {primary, secondary}}},
		notFoundPolicies, breaker, WithRandSource(rand.NewSource(1)))

	breaker.ReportFailure("primary") // threshold 1 → open

	sel, release, err := s.Select(context.Background(), "llama-fast")
	require.NoError(t, err)
	defer release()
	assert.Equal(t, "secondary", sel.Backend.Name)

	// With every circuit open the model has backends but no capacity.
	breaker.ReportFailure("secondary")
	_, _, err = s.Select(context.Background(), "llama-fast")
	assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestSelectRespectsMaxInFlight(t *testing.T) {
	primary := be("primary", 10, 1, 1)
	secondary := be("secondary", 20, 1, 1)
	s := newTestSelector([]models.ModelBackend{primary, secondary}, notFoundPolicies, 1)
	ctx := context.Background()

	sel1, release1, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	assert.Equal(t, "primary", sel1.Backend.Name)
	assert.Equal(t, 1, s.InFlight(primary.ID))

	sel2, release2, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	assert.Equal(t, "secondary", sel2.Backend.Name,
		"a full semaphore fails over to the next candidate")

	_, _, err = s.Select(ctx, "llama-fast")
	assert.ErrorIs(t, err, ErrNoCapacity, "all semaphores full")

	release1()
	assert.Equal(t, 0, s.InFlight(primary.ID), "release frees the slot")
	sel3, release3, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	assert.Equal(t, "primary", sel3.Backend.Name, "freed capacity is selectable again")

	release3()
	release2()
}

func TestSelectReleaseIsIdempotent(t *testing.T) {
	primary := be("primary", 10, 1, 1)
	s := newTestSelector([]models.ModelBackend{primary}, notFoundPolicies, 1)
	ctx := context.Background()

	_, release, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	release()
	release() // double release must not free a slot twice

	_, release2, err := s.Select(ctx, "llama-fast")
	require.NoError(t, err)
	assert.Equal(t, 1, s.InFlight(primary.ID))
	_, _, err = s.Select(ctx, "llama-fast")
	assert.ErrorIs(t, err, ErrNoCapacity,
		"capacity 1 still admits only one holder after a double release")
	release2()
}

func TestSelectWeightedTieBreakDeterministicWithSeed(t *testing.T) {
	sequence := func(seed int64) []string {
		heavy := be("heavy", 10, 3, 100)
		light := be("light", 10, 1, 100)
		s := newTestSelector([]models.ModelBackend{heavy, light}, notFoundPolicies, seed)
		var names []string
		for range 100 {
			sel, release, err := s.Select(context.Background(), "llama-fast")
			require.NoError(t, err)
			names = append(names, sel.Backend.Name)
			release()
		}
		return names
	}

	first, second := sequence(7), sequence(7)
	assert.Equal(t, first, second, "same seed must reproduce the same pick sequence")

	heavyCount := 0
	for _, n := range first {
		if n == "heavy" {
			heavyCount++
		}
	}
	assert.Greater(t, heavyCount, 50, "weight 3 vs 1 must skew picks toward the heavy backend")
	assert.Less(t, heavyCount, 100, "weighted draw is not a constant winner")
}
