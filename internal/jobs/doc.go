// Package jobs owns the batch-job domain: the pure job/item status machines,
// the JobStore contract (transactional creation with a pending outbox row,
// atomic item claims, immutable terminal item results, derived job status),
// and the in-memory MemStore used by Docker-free tests. The Postgres
// implementation is db.JobRepo; the Queue hand-off itself lives in
// internal/redisstore.
package jobs
