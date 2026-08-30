package download

import (
	"context"
	"errors"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The state fold (DESIGN section 2.7).
//
// "`downloads.state` is a stored fold of its tasks (any running → `running`; all
// succeeded → `verifying` → `succeeded`; any failed past retries → `failed`). It
// is stored so list queries stay single-table; the fold function in
// `internal/hf/download` is the single writer and a property test asserts stored
// state always equals the fold of the task rows."
//
// This is that function. It is pure — rows in, state out — so the property test
// can drive it over generated task sets without a database, and so the worker
// and the control verbs cannot disagree about what a set of task states means.

// Fold computes the download state a set of task rows implies.
//
// The ORDER of the tests is the specification, and it is not arbitrary:
//
//   - `canceled` wins over everything, because a cancel is a user decision and a
//     shard that succeeded a millisecond before it does not un-cancel the
//     download.
//   - `paused` beats `running`, for the same reason: a pause has already been
//     recorded against the job, and one transfer still winding down does not
//     make the download running.
//   - `failed` beats `queued`: a download with one dead shard and four queued
//     ones is failed, and reporting it as queued would leave it apparently
//     waiting forever.
//   - Every task succeeded is `verifying` rather than `succeeded`, because the
//     linking and the model transition still have to happen. The worker moves it
//     to `succeeded` when they have.
//
// An empty task set folds to `succeeded`: a download with no files is a download
// with nothing left to do, which is what the blob short-circuit of section 7.2
// produces when every file was already on disk.
func Fold(tasks []store.DownloadTask) model.DownloadState {
	if len(tasks) == 0 {
		return model.DownloadSucceeded
	}
	var (
		canceled, paused, running, failed, queued, verifying, succeeded int
	)
	for _, t := range tasks {
		switch t.State {
		case model.TaskCanceled:
			canceled++
		case model.TaskPaused:
			paused++
		case model.TaskRunning:
			running++
		case model.TaskFailed:
			failed++
		case model.TaskQueued:
			queued++
		case model.TaskVerifying:
			verifying++
		case model.TaskSucceeded:
			succeeded++
		}
	}
	switch {
	case canceled > 0:
		return model.DownloadCanceled
	case paused > 0:
		return model.DownloadPaused
	case running > 0:
		return model.DownloadRunning
	case failed > 0:
		return model.DownloadFailed
	case verifying > 0:
		return model.DownloadVerifying
	case queued > 0:
		return model.DownloadQueued
	case succeeded == len(tasks):
		return model.DownloadVerifying
	default:
		return model.DownloadQueued
	}
}

// FoldTaskViews is Fold over the joined view rows the API reads, so a caller
// with either shape reaches the same answer through the same code.
func FoldTaskViews(views []store.DownloadTaskView) model.DownloadState {
	rows := make([]store.DownloadTask, 0, len(views))
	for _, v := range views {
		rows = append(rows, v.DownloadTask)
	}
	return Fold(rows)
}

// -----------------------------------------------------------------------------
// The single writer
// -----------------------------------------------------------------------------

// stateWrite carries the two things a fold over task rows cannot know, so that
// nothing outside this file has to name a `downloads.state` value.
type stateWrite struct {
	// StartedAt stamps `downloads.started_at` on the write that begins a run.
	// The COLUMN is the caller's; the STATE is never the caller's.
	StartedAt *int64
	// ErrorCode and ErrorMessage accompany a fold that lands on `failed`.
	ErrorCode    *string
	ErrorMessage *string
	// Verified promotes the all-succeeded fold from `verifying` to `succeeded`.
	//
	// It is the one fact about a download that its task rows genuinely do not
	// hold. Section 2.7's fold is "all succeeded → `verifying` → `succeeded`":
	// every file is on disk in BOTH of those states, and what separates them is
	// work at the download level — the snapshot links, the `models` rows moving
	// to `ready`, D69's config-hash recompute. The worker sets this on the one
	// commit that happens after all of that, and nowhere else.
	Verified bool
}

// writeState is the single writer of `downloads.state` that DESIGN section 2.7
// requires: "the fold function in `internal/hf/download` is the single writer".
//
// It reads the task rows in the caller's transaction, folds them, and writes the
// answer. No other code in this package may pass a `model.DownloadState` to
// `store.SetDownloadState`, which is what makes the property test below the
// whole story rather than a spot check: a state that disagreed with the fold
// would have to come from here, and here there is only the fold.
//
// Every caller that moves a TASK calls this in the same transaction, so the two
// rows can never be observed disagreeing.
func (s *Service) writeState(ctx context.Context, tx store.Tx, id string, w stateWrite) (
	model.DownloadState, error) {

	tasks, err := s.store.DownloadTasks(ctx, tx, id)
	if err != nil {
		return "", err
	}
	state := Fold(tasks)
	if w.Verified && state == model.DownloadVerifying {
		state = model.DownloadSucceeded
	}
	var finished *int64
	if isTerminalDownload(state) {
		now := s.now().UnixMilli()
		finished = &now
	}
	if _, err := s.store.SetDownloadState(ctx, tx, id, state,
		w.StartedAt, finished, w.ErrorCode, w.ErrorMessage); err != nil {
		return "", err
	}
	return state, nil
}

// mentions reports whether a failure's text carries one of the task-error words
// of errors.go. It is a named helper rather than an inline strings.Contains so
// the vocabulary is spelled once: `downloads.error_code` is a word the UI
// renders, and a code that differs from the constant by a character renders as
// nothing at all.
func mentions(failure, code string) bool {
	return strings.Contains(failure, code)
}

func isSizeMismatch(err error) bool {
	var sm *hf.SizeMismatchError
	return errors.As(err, &sm)
}
