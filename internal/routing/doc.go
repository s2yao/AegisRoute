// Package routing selects which backend serves an inference request: it
// applies the model's enabled routing policy (or the in-memory "default"
// priority_weighted fallback), skips disabled backends and open circuits,
// enforces per-process max_in_flight semaphores, prefers lower priority, and
// breaks ties by weighted random choice. It also owns the per-backend
// circuit breaker.
package routing
