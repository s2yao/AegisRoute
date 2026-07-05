// Package metrics owns the Prometheus registry and every aegisroute_*
// collector.
//
// Where it's used: HTTP middleware, inference, cache, ratelimit, and the
// worker all increment via an injected *Metrics; each binary serves
// Handler() at /metrics.
//
// Details: New registers all collectors into a fresh prometheus.Registry
// (never the global default), so instances are isolated and double
// registration is impossible; exported metric names are lowercase
// snake_case with the aegisroute_ prefix, and the route label is always the
// chi route pattern to keep cardinality bounded.
package metrics
