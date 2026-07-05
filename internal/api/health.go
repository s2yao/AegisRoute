package api

import (
	"context"
	"net/http"
	"time"

	"github.com/example/aegisroute/internal/httperror"
)

// readyzTimeout bounds each dependency ping so a hung backend cannot make the
// readiness probe hang indefinitely.
const readyzTimeout = 2 * time.Second

// healthz is the liveness probe: if this handler runs, the process is up. It
// deliberately checks no dependencies, so a transient Postgres or Redis
// outage never causes an orchestrator to kill an otherwise-healthy process.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is the readiness probe: it returns 200 only when both Postgres and
// Redis answer a ping, and 503 with the standard JSON error shape otherwise,
// so traffic is routed away until every dependency is reachable.
func readyz(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		defer cancel()

		if err := deps.DBPinger.Ping(ctx); err != nil {
			httperror.Write(w, r, http.StatusServiceUnavailable,
				httperror.CodeUpstreamUnavailable, "database not ready")
			return
		}
		if err := deps.RedisPinger.Ping(ctx); err != nil {
			httperror.Write(w, r, http.StatusServiceUnavailable,
				httperror.CodeUpstreamUnavailable, "redis not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
