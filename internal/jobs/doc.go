// Package jobs is the durable job queue. It handles enqueue, lease, heartbeat,
// progress reporting, cancellation and retry, recovers orphaned jobs whose
// worker died on a previous boot, and keeps the registry that maps a job kind to
// the Worker that runs it (DESIGN section 1).
package jobs
