// Package inference executes outbound calls to model backends: one Client
// shared by the completion handler and the batch worker, with a per-attempt
// timeout, bounded retries with exponential backoff and full jitter on
// transient failures only, and typed errors distinguishing transient from
// permanent outcomes for the circuit breaker.
package inference
