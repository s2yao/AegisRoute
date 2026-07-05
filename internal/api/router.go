package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/example/aegisroute/internal/auth"
	"github.com/example/aegisroute/internal/httperror"
	"github.com/example/aegisroute/internal/metrics"
	"github.com/example/aegisroute/internal/models"
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

	// Bearer-authenticated tenant routes. Only /v1/models today; Stage 4 adds
	// /v1/chat/completions to this same group.
	r.Group(func(br chi.Router) {
		br.Use(auth.BearerAuth(deps.KeyHashSecret, deps.Keys))
		br.Get("/v1/models", listModels(deps))
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
