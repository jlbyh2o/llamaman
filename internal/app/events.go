package app

import (
	"context"

	"github.com/jlbyh2o/llamaman/internal/api"
	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// eventLog satisfies api.EventLogService: the durable read behind
// `GET /api/v1/events/log` (DESIGN section 3.14).
//
// It is a four-line adapter over the store rather than a service because there
// is no domain logic to have: the filter is the query string, the rows are the
// rows, and D49's first invariant puts the SQL in internal/store. The SSE
// endpoint beside it replays from the same table through `Last-Event-ID`, which
// is why the two must never grow separate notions of what an event is.
type eventLog struct{ st *store.Store }

func (e eventLog) Events(ctx context.Context, f store.EventFilter) ([]model.Event, error) {
	var out []model.Event
	err := e.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = e.st.Events(ctx, tx, f)
		return err
	})
	return out, err
}

// jobPublisher satisfies jobs.Publisher: the `jobs` SSE topic of section 3.14.
//
// A job frame is a PATCH keyed by the job's id — "patches keyed by entity id, so
// the client merges into its query cache without refetching" — and it is
// published on two topics rather than one:
//
//   - `jobs`, which is what the dashboard's active-jobs strip and every progress
//     bar subscribe to; and
//   - the topic of the SUBJECT's subsystem (`llamacpp`, `downloads`, `bench`),
//     because a screen watching a llama.cpp install does not
//     subscribe to `jobs` to learn that its build moved — it subscribes to
//     `llamacpp`, and the job row IS the progress of that build.
//
// The second frame carries no patch, so the client treats it as a signal and
// re-reads that family. That is the honest shape: the job row's fields are not
// the version row's fields, so "something moved over there" is all this
// publisher can truthfully say about the subject.
type jobPublisher struct{ rec *events.Recorder }

// jobFrame is the wire shape of a `jobs` patch. It is a projection of model.Job
// rather than the row itself, for the same reason every other DTO is: the column
// set changes for storage reasons and the field set changes for client reasons.
type jobFrame struct {
	Kind        string  `json:"kind"`
	State       string  `json:"state"`
	SubjectType string  `json:"subject_type"`
	SubjectID   string  `json:"subject_id"`
	Progress    *string `json:"progress_json"`
	Attempts    int     `json:"attempts"`
	MaxAttempts int     `json:"max_attempts"`
	// ErrorCode and ErrorMessage are what turn a spinner into a diagnosis. A
	// failed job that published only its state would leave a screen able to say
	// "Failed" and nothing else, which is the difference between an error a user
	// can act on and one they file a bug about.
	ErrorCode    *string `json:"error_code"`
	ErrorMessage *string `json:"error_message"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
}

func (p jobPublisher) PublishJob(j model.Job) {
	if p.rec == nil {
		return
	}
	p.rec.PublishPatch(events.TopicJobs, "job.state", j.ID, jobFrame{
		Kind:         string(j.Kind),
		State:        string(j.State),
		SubjectType:  string(j.SubjectType),
		SubjectID:    j.SubjectID,
		Progress:     j.ProgressJSON,
		Attempts:     j.Attempts,
		MaxAttempts:  j.MaxAttempts,
		ErrorCode:    j.ErrorCode,
		ErrorMessage: j.ErrorMessage,
		StartedAt:    api.TimePtr(j.StartedAt),
		FinishedAt:   api.TimePtr(j.FinishedAt),
	})

	if topic, ok := topicForSubject(j.SubjectType); ok {
		p.rec.PublishSignal(topic, "job."+string(j.Kind), j.SubjectID)
	}
}

// topicForSubject maps a job's subject onto the SSE topic of the screen that
// shows it. A subject with no live screen — a maintenance pass, a toolchain
// probe — reports false and travels on `jobs` alone.
func topicForSubject(t model.JobSubjectType) (events.Topic, bool) {
	switch t {
	case model.SubjectLlamacppVersion:
		return events.TopicLlamacpp, true
	case model.SubjectDownload:
		return events.TopicDownloads, true
	case model.SubjectBenchRun:
		return events.TopicBench, true
	default:
		return "", false
	}
}
