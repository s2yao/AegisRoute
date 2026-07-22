// Package worker consumes job-level messages from the batch queue and
// processes their items with a bounded pool: claim an item atomically in
// Postgres, run it through the shared routing selector and inference client,
// write the terminal result durably, and ack the message only after the
// job's status is recomputed. Delivery is at-least-once; idempotency comes
// from Postgres item state (terminal items are never claimable again).
package worker
