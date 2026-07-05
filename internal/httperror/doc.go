// Package httperror writes the one true JSON error shape:
// {"error":{"code","message","request_id"}}.
//
// Where it's used: all handlers and middleware emit every non-2xx response
// through Write.
//
// Details: the request id is read from the request context (set by the
// request-id middleware via internal/observability); Code is always one of
// the named Code* constants so clients never parse free-form errors.
package httperror
