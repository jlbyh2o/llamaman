package jobs

import (
	"context"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The `jobs` SSE topic (DESIGN section 3.14).
//
// Section 3.14 lists `jobs` among the topics `GET /api/v1/events` accepts and
// says of every long action that "progress arrives over SSE". Nothing published
// on it, and the consequence was not a missing nicety: every screen in section 4
// that narrates work — the wizard's llama.cpp step, the Downloads queue, the
// build log, the bench progress bar, the dashboard's active-jobs strip — sat on
// whatever it had read when it mounted. A build that failed three seconds in
// still read "Working — started 40 seconds ago" forty seconds later, which a
// user cannot tell from an install that has hung.
//
// So the queue publishes, and it publishes at the four moments a job's row
// actually moves: when it is enqueued, when a worker reports progress, when it
// changes hands (pause, resume, cancel, retry) and when it closes. Those are the
// same moments it WRITES, which is the property that matters — a publisher that
// fired on a timer would be a poll with extra steps, and section 4 is explicit
// that there are to be no polling loops where a topic exists.
//
// Every publish happens AFTER the transaction that wrote the row has committed,
// for the reason internal/events.Recorder states: a subscriber told about a row
// that then rolled back has been told something that did not happen.

// Publisher is the SSE seam, declared here because the consumer owns it
// (section 1). internal/app satisfies it over the events hub.
type Publisher interface {
	// PublishJob fans one job row out on the `jobs` topic, and on the topic of
	// the subsystem the job's subject belongs to when there is one.
	PublishJob(j model.Job)
}

// publishTimeout bounds the read a publish needs. It is short because the row is
// in SQLite on the same host and this is decoration on a write that already
// succeeded: a publish that cannot complete promptly is dropped, and the client
// re-reads on its next ordinary query.
const publishTimeout = 2 * time.Second

// notify reads a job by id and publishes it.
//
// It re-reads rather than publishing the in-memory copy the caller happens to
// hold, and that is deliberate: the caller's copy is from BEFORE the write, so
// publishing it would broadcast the state the job just left. The read is one
// indexed lookup, and it runs on a context detached from the caller's so a
// client that disconnected mid-request still gets its frame out to everyone else.
func (q *Queue) notify(ctx context.Context, id string) {
	if q.publisher == nil || id == "" {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	var j model.Job
	if err := q.s.Read(nctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = q.s.Job(ctx, tx, id)
		return err
	}); err != nil {
		q.log.Debug("could not read a job to publish it", "job", id, "error", err)
		return
	}
	q.publisher.PublishJob(j)
}
