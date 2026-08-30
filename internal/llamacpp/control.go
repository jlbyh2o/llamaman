package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The three control verbs of DESIGN section 3.5 that act on a build in flight or
// a build that stopped: cancel, retry, and the log both of them are read
// through.
//
// They are here rather than in service.go because all three answer the same
// question first — "which `jobs` row belongs to this version" — and because the
// log is not a database operation at all: it is a file, plus the in-memory ring
// of the build that is writing it.

// Cancel is `POST /api/v1/llamacpp/versions/{id}/cancel` (§3.5).
//
// It is section 2.5's cancel edge: "any pre-`ready` | cancel | `canceled` |
// SIGTERM process group, remove partial dirs". The SIGTERM and the removal are
// the worker's — this cuts its context through the queue and the worker decides
// when it can stop — which is why the answer is `202` and not `204`.
//
// The job it cancels is the LIVE one for this version. `idx_jobs_one_live_per_subject`
// makes at most one exist, so there is nothing to choose between; a version with
// none is `build_not_cancelable`, which is the honest answer for a build that
// already finished.
func (s *Service) Cancel(ctx context.Context, id string) (model.Job, error) {
	j, err := s.liveJobFor(ctx, id, CodeBuildNotCancelable, "cannot be canceled")
	if err != nil {
		return model.Job{}, err
	}
	out, err := s.queue.Cancel(ctx, j.ID)
	if errors.Is(err, jobs.ErrNotCancelable) {
		return model.Job{}, errorf(CodeBuildNotCancelable,
			"the job for llama.cpp %s has already finished", id)
	}
	return out, err
}

// Retry is `POST /api/v1/llamacpp/versions/{id}/retry` (§3.5): "resumes an
// `interrupted` build against warm objects (D4)".
//
// It is the same reuse-and-reset §2.5's table gives a re-POST of a terminal id,
// reached without re-resolving the request: the stored `params_json` already
// names the tag, the asset and the flags this build was resolved to, and
// resolving again would let a nightly that moved in the meantime silently change
// what "retry" means.
//
// The queue accepts exactly the three states a job can stop in without being
// finished with — `failed`, `canceled` and `interrupted` — and moves the version
// row back to `pending` in the same transaction through the install worker's
// DomainWriter. The build directory and the cmake cache are untouched, which is
// what makes the rerun warm.
func (s *Service) Retry(ctx context.Context, id string) (model.Job, error) {
	j, err := s.lastInstallJobFor(ctx, id)
	if err != nil {
		return model.Job{}, err
	}
	out, err := s.queue.Retry(ctx, j.ID)
	if errors.Is(err, jobs.ErrNotRetryable) {
		return model.Job{}, errorf(CodeBuildNotRetryable,
			"the last install of llama.cpp %s is in state %q, which is not a state a retry acts on",
			id, j.State)
	}
	return out, err
}

// liveJobFor returns the live job for a version, or a refusal carrying code.
func (s *Service) liveJobFor(ctx context.Context, id string, code model.ErrorCode,
	verb string) (model.Job, error) {

	var out model.Job
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.LlamacppVersion(ctx, tx, id); err != nil {
			return err
		}
		rows, err := s.store.Jobs(ctx, tx, store.JobFilter{
			SubjectType: model.SubjectLlamacppVersion, SubjectID: id,
			States: model.LiveJobStates(), Limit: 1,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errorf(code, "no job for llama.cpp %s is running, so it %s", id, verb)
		}
		out = rows[0]
		return nil
	})
	return out, err
}

// lastInstallJobFor returns the newest `llamacpp_install` job for a version.
//
// It is the newest INSTALL rather than the newest job of any kind, because a
// version's subject is also carried by its activate and delete jobs, and
// "retry the build" must not reach for a delete that failed last week.
func (s *Service) lastInstallJobFor(ctx context.Context, id string) (model.Job, error) {
	var out model.Job
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.LlamacppVersion(ctx, tx, id); err != nil {
			return err
		}
		rows, err := s.store.Jobs(ctx, tx, store.JobFilter{
			SubjectType: model.SubjectLlamacppVersion, SubjectID: id,
			Kinds: []model.JobKind{model.JobLlamacppInstall}, Limit: 1,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return errorf(CodeBuildNotRetryable,
				"llama.cpp %s has no install job to retry", id)
		}
		out = rows[0]
		return nil
	})
	return out, err
}

// -----------------------------------------------------------------------------
// The build log (§3.5's `GET /llamacpp/versions/{id}/log`)
// -----------------------------------------------------------------------------

// DefaultLogLimit bounds one `?limit=` page of build log. A CUDA compile writes
// a few megabytes; a page is a screenful plus room to scroll, and a client that
// wants the whole thing pages through it with the `next_offset` it is handed.
const DefaultLogLimit = 256 << 10

// MaxLogLimit is the largest page this endpoint will assemble at once.
const MaxLogLimit = 4 << 20

// LogChunk is one page of a build log, plus what a client needs to ask for the
// next one and to know whether there will be one.
type LogChunk struct {
	// Text is the log bytes themselves, plain text.
	Text string
	// Offset is where this chunk starts and NextOffset where the next one does.
	// Both are byte offsets into the file, which is what makes paging a build
	// log a subtraction rather than a line count over a file that is growing
	// while it is read.
	Offset     int64
	NextOffset int64
	// Size is the file's length at the moment it was read.
	Size int64
	// Live reports that a build is writing this log right now, so a client that
	// reached the end should follow it over SSE rather than poll.
	Live bool
}

// Log reads a page of one build's log (§3.5).
//
// The FILE is the source, not the in-memory ring. The ring is 5000 lines and
// exists for the live tail; the file is the whole build, survives a restart, and
// is what a user reading a failure from three days ago needs (F15).
func (s *Service) Log(ctx context.Context, id string, offset, limit int64) (LogChunk, error) {
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.LlamacppVersion(ctx, tx, id)
		return err
	}); err != nil {
		return LogChunk{}, err
	}

	switch {
	case limit <= 0:
		limit = DefaultLogLimit
	case limit > MaxLogLimit:
		limit = MaxLogLimit
	}
	if offset < 0 {
		offset = 0
	}

	path := s.layout.LogPath(id)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// A version that has not started building has no log, and that is not an
		// error: an empty chunk lets the UI render the panel and wait.
		return LogChunk{Live: s.buildIsLive(id)}, nil
	}
	if err != nil {
		return LogChunk{}, fmt.Errorf("llamacpp: open the build log for %s: %w", id, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return LogChunk{}, fmt.Errorf("llamacpp: stat the build log for %s: %w", id, err)
	}
	out := LogChunk{Offset: offset, NextOffset: offset, Size: info.Size(), Live: s.buildIsLive(id)}
	if offset >= info.Size() {
		return out, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return LogChunk{}, fmt.Errorf("llamacpp: seek the build log for %s: %w", id, err)
	}
	buf := make([]byte, min64(limit, info.Size()-offset))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return LogChunk{}, fmt.Errorf("llamacpp: read the build log for %s: %w", id, err)
	}
	out.Text = string(buf[:n])
	out.NextOffset = offset + int64(n)
	return out, nil
}

// FollowLog subscribes to the live tail of a running build, returning the
// channel of rendered lines and the function that stops it. `ok` is false when no
// build is running for this id, which is the caller's cue to serve the file and
// close the stream rather than hold a connection open forever.
//
// The channel is LOSSY by construction (§6.5's log sink): a browser that stops
// reading its SSE stream must not be able to slow a compile down, so a full
// subscriber drops frames rather than blocking the build. Whoever needs every
// line reads the file, which is why Log exists beside this.
func (s *Service) FollowLog(id string) (lines <-chan string, stop func(), ok bool) {
	if s.logs == nil {
		return nil, nil, false
	}
	sink, live := s.logs.Sink(id)
	if !live {
		return nil, nil, false
	}
	entries, unsubscribe := sink.Subscribe(0)
	out := make(chan string, cap(entries))
	go func() {
		defer close(out)
		for e := range entries {
			select {
			case out <- e.String():
			default:
			}
		}
	}()
	return out, unsubscribe, true
}

// LogTail returns the last n lines of a running build from the in-memory ring,
// so a client that opens the live stream has something on screen before the next
// line is written. It is empty when no build is running.
func (s *Service) LogTail(id string, n int) []string {
	if s.logs == nil {
		return nil
	}
	sink, ok := s.logs.Sink(id)
	if !ok {
		return nil
	}
	entries := sink.Tail(n)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.String())
	}
	return out
}

// buildIsLive reports whether a build is writing this version's log right now.
func (s *Service) buildIsLive(id string) bool {
	if s.logs == nil {
		return false
	}
	_, ok := s.logs.Sink(id)
	return ok
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
