package events

// Topic is the `?topics=` filter axis of `GET /api/v1/events` (DESIGN §3.14).
// It is not a database enum — nothing in §2 closes it with a CHECK — because it
// names channels of an in-process stream, not a column: some topics (`instances`,
// `downloads`, `llamacpp`, `bench`) correspond to an `events.category` whose rows
// a subsystem also wants pushed live, others (`gpu`) are live telemetry that is
// never persisted at all (§6, "They live in a per-GPU in-memory ring and stream
// over SSE"), `jobs` is progress off the job queue rather than the events table
// (§3.14, "progress arrives over SSE"), `notifications` is its own table, and
// `events` is the generic firehose: every appended `events` row, regardless of
// category, is published on it (see Recorder.Publish) — it is what backs the
// `/events` audit-log view's live update and what `Last-Event-ID` resumes.
type Topic string

const (
	TopicInstances     Topic = "instances"
	TopicDownloads     Topic = "downloads"
	TopicLlamacpp      Topic = "llamacpp"
	TopicGPU           Topic = "gpu"
	TopicBench         Topic = "bench"
	TopicJobs          Topic = "jobs"
	TopicEvents        Topic = "events"
	TopicNotifications Topic = "notifications"
)

// Topics lists every topic `GET /api/v1/events` accepts in `?topics=`, in the
// order §3.14 lists them.
func Topics() []Topic {
	return []Topic{
		TopicInstances, TopicDownloads, TopicLlamacpp, TopicGPU,
		TopicBench, TopicJobs, TopicEvents, TopicNotifications,
	}
}

// Valid reports whether t is one of the topics §3.14 names.
func (t Topic) Valid() bool {
	for _, v := range Topics() {
		if t == v {
			return true
		}
	}
	return false
}
