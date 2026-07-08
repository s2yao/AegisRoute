package routing

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/db"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
)

// ErrNoBackends means no enabled, well-formed backend serves the requested
// model at all — the model name is effectively unknown to the gateway.
var ErrNoBackends = errors.New("routing: no enabled backend serves this model")

// ErrNoCapacity means backends serving the model exist, but every candidate
// was skipped: its circuit is open or its per-process max_in_flight
// semaphore is full. The request may succeed if retried later.
var ErrNoCapacity = errors.New("routing: all backends for this model are unavailable")

// BackendStore is the subset of the backend repository the Selector depends
// on. Satisfied by *db.BackendRepo.
type BackendStore interface {
	ListByModelEnabled(ctx context.Context, modelName string) ([]models.ModelBackend, error)
}

// PolicyStore is the subset of the routing-policy repository the Selector
// depends on. GetForModel reports a missing enabled policy as
// db.ErrNotFound, which the Selector maps to the in-memory fallback policy.
// Satisfied by *db.RoutingPolicyRepo.
type PolicyStore interface {
	GetForModel(ctx context.Context, modelName string) (models.RoutingPolicy, error)
}

// fallback is the in-memory policy applied when no enabled routing policy
// exists for a model, so routing always has a defined strategy.
const (
	fallbackPolicyName = "default"
	fallbackStrategy   = models.StrategyPriorityWeighted
)

// Selection is the routing decision for one request: the chosen backend plus
// the policy name and strategy that produced it (surfaced to clients via the
// X-AegisRoute-Backend and X-AegisRoute-Routing-Policy headers).
type Selection struct {
	Backend    models.ModelBackend
	PolicyName string
	Strategy   string
}

// Selector picks a healthy backend for a model: it applies the enabled
// routing policy (or the fallback), filters out disabled/invalid rows and
// open circuits, enforces each backend's max_in_flight with a per-process
// semaphore, prefers lower priority, and breaks priority ties by weighted
// random choice.
//
// max_in_flight is a per-process limit: each gateway or worker process
// enforces it independently, so N replicas admit up to N×max_in_flight
// concurrent calls per backend. Distributed concurrency control is an
// explicit non-goal.
type Selector struct {
	backends BackendStore
	policies PolicyStore
	breaker  *Breaker
	metrics  *metrics.Metrics // optional; nil disables selector instrumentation

	mu   sync.Mutex // guards rng and sems
	rng  *rand.Rand
	sems map[uuid.UUID]*semaphore
}

// SelectorOption customizes a Selector at construction.
type SelectorOption func(*Selector)

// WithRandSource injects the randomness behind the weighted tie-break, so
// tests can make selection order deterministic with a seeded source.
func WithRandSource(src rand.Source) SelectorOption {
	return func(s *Selector) { s.rng = rand.New(src) }
}

// WithMetrics wires observability into the Selector: the per-backend in-flight
// gauge and the open-circuit short-circuit counter. Optional — nil (the
// default) leaves the Selector uninstrumented, which is what unit tests use.
func WithMetrics(m *metrics.Metrics) SelectorOption {
	return func(s *Selector) { s.metrics = m }
}

// NewSelector builds a Selector over the given stores and circuit breaker.
func NewSelector(backends BackendStore, policies PolicyStore, breaker *Breaker, opts ...SelectorOption) *Selector {
	s := &Selector{
		backends: backends,
		policies: policies,
		breaker:  breaker,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		sems:     map[uuid.UUID]*semaphore{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Select picks a backend for the model and reserves one max_in_flight slot
// on it. On success the caller must invoke release exactly once when the
// backend call finishes (releasing is idempotent); the returned Selection
// carries the backend and the policy applied. Candidates whose circuit is
// open, whose semaphore is full, or whose ID is in exclude are skipped in
// favor of the next one. exclude lets a caller fail over within one request:
// pass the IDs of backends already tried so a fresh, untried backend is
// chosen. Errors: ErrNoBackends when the model has no usable backends at all,
// ErrNoCapacity when every candidate was skipped (excluded, full, or open),
// or a wrapped store error.
func (s *Selector) Select(ctx context.Context, model string, exclude ...uuid.UUID) (Selection, func(), error) {
	excluded := make(map[uuid.UUID]struct{}, len(exclude))
	for _, id := range exclude {
		excluded[id] = struct{}{}
	}

	policyName, strategy := fallbackPolicyName, fallbackStrategy
	policy, err := s.policies.GetForModel(ctx, model)
	switch {
	case err == nil:
		policyName, strategy = policy.Name, policy.Strategy
	case !errors.Is(err, db.ErrNotFound):
		return Selection{}, nil, fmt.Errorf("routing: load policy for %q: %w", model, err)
	}

	list, err := s.backends.ListByModelEnabled(ctx, model)
	if err != nil {
		return Selection{}, nil, fmt.Errorf("routing: list backends for %q: %w", model, err)
	}

	// Defensively drop rows that can't be routed to, even though the schema
	// CHECKs should prevent them: a zero weight breaks the weighted draw, a
	// non-positive max_in_flight admits nothing, and a disabled row must
	// never receive traffic even if a store hands one back.
	candidates := make([]models.ModelBackend, 0, len(list))
	for _, b := range list {
		if !b.Enabled || b.Weight <= 0 || b.Priority < 0 || b.MaxInFlight <= 0 {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return Selection{}, nil, ErrNoBackends
	}

	// priority_weighted is the only implemented strategy (the schema CHECK
	// admits nothing else); the policy's declared strategy is still surfaced
	// on the Selection for the response header.
	for _, b := range s.order(candidates) {
		if _, skip := excluded[b.ID]; skip {
			continue
		}
		sem := s.semaphoreFor(b)
		if !sem.tryAcquire() {
			continue
		}
		// Acquire the semaphore before consulting the breaker: Allow reserves
		// the single half-open probe slot, so it must only be consumed by a
		// candidate that already holds capacity to actually make the call.
		if !s.breaker.Allow(b.Name) {
			// Skipped because the circuit is open (or its half-open probe is
			// already taken): a provable reliability event, not just the gauge.
			s.observeShortCircuit(b.Name)
			sem.release()
			continue
		}
		s.observeInFlight(b.Name, 1)
		var once sync.Once
		release := func() {
			once.Do(func() {
				sem.release()
				s.observeInFlight(b.Name, -1)
			})
		}
		return Selection{Backend: b, PolicyName: policyName, Strategy: strategy}, release, nil
	}
	return Selection{}, nil, ErrNoCapacity
}

// order arranges candidates lowest-priority-number first; backends sharing a
// priority are ordered by repeated weighted draws without replacement, so a
// weight-3 backend is three times as likely as a weight-1 peer to be tried
// first within its tier.
func (s *Selector) order(candidates []models.ModelBackend) []models.ModelBackend {
	sorted := make([]models.ModelBackend, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	out := make([]models.ModelBackend, 0, len(sorted))
	for start := 0; start < len(sorted); {
		end := start
		for end < len(sorted) && sorted[end].Priority == sorted[start].Priority {
			end++
		}
		out = append(out, s.weightedShuffle(sorted[start:end])...)
		start = end
	}
	return out
}

// weightedShuffle orders one priority tier by successive weighted draws
// without replacement. The rng is guarded because Select can run
// concurrently and rand.Rand is not thread-safe.
func (s *Selector) weightedShuffle(tier []models.ModelBackend) []models.ModelBackend {
	if len(tier) == 1 {
		return tier
	}
	pool := make([]models.ModelBackend, len(tier))
	copy(pool, tier)
	out := make([]models.ModelBackend, 0, len(pool))

	s.mu.Lock()
	defer s.mu.Unlock()
	for len(pool) > 0 {
		total := 0
		for _, b := range pool {
			total += b.Weight
		}
		n := s.rng.Intn(total)
		for i, b := range pool {
			n -= b.Weight
			if n < 0 {
				out = append(out, b)
				pool = append(pool[:i], pool[i+1:]...)
				break
			}
		}
	}
	return out
}

// semaphoreFor returns the per-backend in-flight semaphore, creating it on
// first sight and re-creating it when max_in_flight changes. Holders of a
// replaced semaphore release into the old instance, so a capacity change
// can briefly over-admit; the window closes as those requests drain.
func (s *Selector) semaphoreFor(b models.ModelBackend) *semaphore {
	s.mu.Lock()
	defer s.mu.Unlock()
	sem, ok := s.sems[b.ID]
	if !ok || sem.capacity != b.MaxInFlight {
		sem = newSemaphore(b.MaxInFlight)
		s.sems[b.ID] = sem
	}
	return sem
}

// observeShortCircuit counts a candidate skipped because its circuit was open.
func (s *Selector) observeShortCircuit(backend string) {
	if s.metrics != nil {
		s.metrics.CircuitShortCircuitsTotal.WithLabelValues(backend).Inc()
	}
}

// observeInFlight moves the per-backend in-flight gauge by delta (+1 on
// selection, -1 on release), balanced by the release func's sync.Once.
func (s *Selector) observeInFlight(backend string, delta float64) {
	if s.metrics != nil {
		s.metrics.BackendInFlight.WithLabelValues(backend).Add(delta)
	}
}

// InFlight reports how many requests currently hold a slot for the backend
// with the given id (0 for an unseen backend).
func (s *Selector) InFlight(backendID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sem, ok := s.sems[backendID]
	if !ok {
		return 0
	}
	return len(sem.slots)
}

// semaphore is a non-blocking counting semaphore sized to a backend's
// max_in_flight. Acquisition never blocks: a full semaphore fails over to
// the next candidate instead of queueing.
type semaphore struct {
	capacity int
	slots    chan struct{}
}

func newSemaphore(capacity int) *semaphore {
	return &semaphore{capacity: capacity, slots: make(chan struct{}, capacity)}
}

func (s *semaphore) tryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *semaphore) release() {
	select {
	case <-s.slots:
	default: // over-release is a programming error; make it harmless
	}
}
