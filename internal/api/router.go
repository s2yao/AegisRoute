package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/cache"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/idempotency"
	"github.com/example/aegisroute/internal/inference"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
	"github.com/example/aegisroute/internal/routing"
)

// BackendStore is the subset of the backend repository the API depends on.
// Declaring it here (consumer-side) keeps the handlers testable with an
// in-memory fake and free of any concrete database type. Get/Update report a
// missing row as db.ErrNotFound. It is satisfied by *db.BackendRepo.
type BackendStore interface {
	Insert(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error)
	Update(ctx context.Context, b models.ModelBackend) (models.ModelBackend, error)
	GetByID(ctx context.Context, id uuid.UUID) (models.ModelBackend, error)
	List(ctx context.Context) ([]models.ModelBackend, error)
	ListEnabled(ctx context.Context) ([]models.ModelBackend, error)
}

// PolicyStore is the subset of the routing-policy repository the API depends
// on. Satisfied by *db.RoutingPolicyRepo.
type PolicyStore interface {
	Insert(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error)
	Update(ctx context.Context, p models.RoutingPolicy) (models.RoutingPolicy, error)
	GetByID(ctx context.Context, id uuid.UUID) (models.RoutingPolicy, error)
	List(ctx context.Context) ([]models.RoutingPolicy, error)
}

// Pinger is a single dependency's liveness check, used by /readyz. Keeping it
// a tiny interface (rather than a concrete pool or client) lets readiness be
// tested with fakes and no live Postgres or Redis.
type Pinger interface {
	Ping(ctx context.Context) error
}

// ChatSelector picks a backend for a model and reserves an in-flight slot on
// it; the returned release must be called when the backend call finishes.
// exclude carries the IDs of backends already tried in this request so
// Select can hand back a fresh one for intra-request failover. Satisfied by
// *routing.Selector.
type ChatSelector interface {
	Select(ctx context.Context, model string, exclude ...uuid.UUID) (routing.Selection, func(), error)
}

// InferenceDoer executes one outbound backend call with retry and timeout.
// Satisfied by *inference.Client.
type InferenceDoer interface {
	Do(ctx context.Context, backend models.ModelBackend, body []byte) (*inference.Response, error)
}

// CircuitReporter receives per-backend call outcomes. It must be the same
// breaker instance the ChatSelector consults, or selection and outcome
// reporting drift apart. ReportCanceled marks a call ended by the caller's
// own context — verdict-free, but it returns a reserved half-open probe.
// Satisfied by *routing.Breaker.
type CircuitReporter interface {
	ReportSuccess(backend string)
	ReportFailure(backend string)
	ReportCanceled(backend string)
}

// InferenceRequestStore appends inference_requests ledger rows. It is the
// synchronous store the AsyncLedger drains into (not called from the request
// hot path). Satisfied by *db.InferenceRequestRepo.
type InferenceRequestStore interface {
	Insert(ctx context.Context, row models.InferenceRequest) (models.InferenceRequest, error)
}

// ResponseCache stores eligible completion responses. Get reports a miss as
// (nil, nil); errors are for the handler to log and fail open on. Satisfied
// by *cache.Cache.
type ResponseCache interface {
	Get(ctx context.Context, key string) (*cache.Entry, error)
	Put(ctx context.Context, key string, e cache.Entry) error
}

// RateLimiter caps requests per API key. Errors are for the handler to fail
// open on — a Redis outage degrades rate limiting, never availability.
// Satisfied by *ratelimit.Limiter.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}

// IdempotencyGate drives the Idempotency-Key flow: Check before rate
// limiting (completed replays are free), Begin after it (only admitted new
// work opens a pending record), Complete with the final response. Satisfied
// by *idempotency.Coordinator.
type IdempotencyGate interface {
	Check(ctx context.Context, scope, key, requestHash string) (idempotency.Decision, error)
	Begin(ctx context.Context, scope, key, requestHash string) (idempotency.Decision, error)
	Complete(ctx context.Context, recordID uuid.UUID, status int, headers map[string]string, body []byte) error
	// Release discards an opened record so a same-key retry is fresh work
	// (used for retryable 5xx outcomes).
	Release(ctx context.Context, recordID uuid.UUID) error
}

// Deps is everything NewRouter needs to wire the gateway. The composition root
// (cmd/gateway-api) fills it from real repositories, pingers, and config; tests
// fill it with fakes.
type Deps struct {
	Logger        *slog.Logger
	Metrics       *metrics.Metrics
	KeyHashSecret string
	AdminToken    string
	Keys          auth.KeyStore
	Backends      BackendStore
	Policies      PolicyStore
	DBPinger      Pinger
	RedisPinger   Pinger
	Selector      ChatSelector
	Inference     InferenceDoer
	Circuit       CircuitReporter
	Ledger        LedgerRecorder
	Cache         ResponseCache
	Limiter       RateLimiter
	Idempotency   IdempotencyGate
	// InferenceBudget caps the wall-clock time the chat handler spends across
	// all failover attempts for one request; zero means no extra budget beyond
	// the request context (used in tests). Production sets it from
	// config.InferenceBudget().
	InferenceBudget time.Duration
}

// NewRouter builds the gateway's HTTP handler: the shared middleware chain
// followed by the public, bearer-authenticated, and admin-authenticated route
// groups.
//
// Middleware order (outermost first): recover → request-id → logging →
// metrics → reject-query-credentials. Route-scoped auth is applied per group
// so it always runs after the shared chain.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(recoverer(deps.Logger))
	r.Use(requestID)
	r.Use(requestLogger(deps.Logger))
	r.Use(metricsMiddleware(deps.Metrics))
	r.Use(rejectQueryCredentials)

	// Unmatched paths and unsupported methods must still return the canonical
	// error envelope; chi's defaults are a plain-text 404 and an empty-body 405.
	// These run inside the middleware chain, so they carry X-Request-ID too.
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed)

	// Public: liveness, readiness, and the Prometheus scrape endpoint.
	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(deps))
	r.Handle("/metrics", deps.Metrics.Handler())

	// Bearer-authenticated tenant routes. All of them share one per-API-key
	// rate budget: /v1/models goes through the middleware, while the chat
	// handler runs the same check inline at its precedence point — after the
	// idempotency replay lookup (replays are free) and before a pending
	// record is opened. Do not also wrap the chat route or requests would be
	// charged twice.
	r.Group(func(br chi.Router) {
		br.Use(auth.BearerAuth(deps.KeyHashSecret, deps.Keys))
		br.With(rateLimitMiddleware(deps)).Get("/v1/models", listModels(deps))
		br.Post("/v1/chat/completions", chatCompletions(deps))
	})

	// Admin-token control plane. Scoped to exactly the backend and
	// routing-policy trees — batch-jobs are tenant routes and must not be
	// captured here (they arrive in Stage 6 under bearer auth).
	r.Group(func(ar chi.Router) {
		ar.Use(auth.AdminAuth(deps.AdminToken))
		ar.Route("/api/v1/backends", func(br chi.Router) {
			br.Get("/", listBackends(deps))
			br.Post("/", createBackend(deps))
			br.Patch("/{id}", patchBackend(deps))
		})
		ar.Route("/api/v1/routing-policies", func(pr chi.Router) {
			pr.Get("/", listPolicies(deps))
			pr.Post("/", createPolicy(deps))
			pr.Patch("/{id}", patchPolicy(deps))
		})
	})

	return r
}

// notFound answers an unmatched path with the canonical error envelope.
func notFound(w http.ResponseWriter, r *http.Request) {
	httperror.Write(w, r, http.StatusNotFound, httperror.CodeNotFound, "resource not found")
}

// methodNotAllowed answers a known path with an unsupported method using the
// canonical envelope. There is no dedicated error code for 405, so the fixed
// code set's bad_request is used with a 405 status.
func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	httperror.Write(w, r, http.StatusMethodNotAllowed, httperror.CodeBadRequest, "method not allowed")
}
