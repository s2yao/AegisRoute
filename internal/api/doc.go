// Package api builds the gateway's chi router: the shared middleware chain
// (recover, request-id, redacted logging, metrics, query-credential
// rejection), the public health/metrics endpoints, the bearer-authenticated
// tenant routes, and the admin-token control-plane handlers.
package api
