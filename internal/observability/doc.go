// Package observability builds the slog JSON logger, carries request ids
// through contexts, and decides which header/variable names must be redacted.
//
// Where it's used: every binary builds its logger here; logging middleware
// uses the request-id helpers and Redact.
//
// Details: Redact matches secret markers (authorization, cookie, token,
// secret, password, api-key, x-admin-token) as case-insensitive substrings,
// preferring false positives over leaking a secret value into logs.
package observability
