// Package redisstore owns the shared Redis client and the identifiers of the
// batch-job stream. Later stages all build on it: the response cache, the
// rate limiter, the idempotency store, and the batch worker share the one
// client constructed here rather than each opening their own. Stream
// read/write helpers arrive with the batch pipeline in Stage 6; until then
// this package only constructs the client and names the stream.
package redisstore
