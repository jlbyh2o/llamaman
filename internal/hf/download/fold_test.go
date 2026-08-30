package download

import (
	"context"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The state fold (DESIGN section 2.7).
//
// "`downloads.state` is a stored fold of its tasks… the fold function in
// `internal/hf/download` is the single writer and a property test asserts stored
// state always equals the fold of the task rows." This is the table half of that
// assertion; the integration test asserts the stored column agrees with it after
// a real run.

func tasks(states ...model.DownloadTaskState) []store.DownloadTask {
	out := make([]store.DownloadTask, 0, len(states))
	for _, s := range states {
		out = append(out, store.DownloadTask{State: s})
	}
	return out
}

func TestFold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []store.DownloadTask
		want model.DownloadState
		why  string
	}{
		{
			name: "no tasks is nothing left to do",
			rows: nil, want: model.DownloadSucceeded,
			why: "the blob short-circuit lands every file without a transfer",
		},
		{
			name: "all queued",
			rows: tasks(model.TaskQueued, model.TaskQueued), want: model.DownloadQueued,
		},
		{
			name: "any running",
			rows: tasks(model.TaskQueued, model.TaskRunning, model.TaskSucceeded),
			want: model.DownloadRunning,
		},
		{
			name: "all succeeded is verifying, not succeeded",
			rows: tasks(model.TaskSucceeded, model.TaskSucceeded),
			want: model.DownloadVerifying,
			why:  "the linking and the model transition still have to happen",
		},
		{
			name: "a cancel wins over a shard that landed a millisecond earlier",
			rows: tasks(model.TaskSucceeded, model.TaskCanceled, model.TaskRunning),
			want: model.DownloadCanceled,
		},
		{
			name: "a pause beats a transfer still winding down",
			rows: tasks(model.TaskPaused, model.TaskRunning),
			want: model.DownloadPaused,
		},
		{
			name: "a dead shard beats four queued ones",
			rows: tasks(model.TaskFailed, model.TaskQueued, model.TaskQueued),
			want: model.DownloadFailed,
			why:  "reporting it as queued would leave it apparently waiting forever",
		},
		{
			name: "verifying beats queued",
			rows: tasks(model.TaskVerifying, model.TaskQueued),
			want: model.DownloadVerifying,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Fold(tc.rows); got != tc.want {
				t.Errorf("Fold = %s, want %s (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestFoldTaskViewsAgreesWithFold keeps the two entry points from drifting: the
// API reads joined view rows and the worker reads plain ones, and they must fold
// identically or the list and the detail page would disagree about one download.
func TestFoldTaskViewsAgreesWithFold(t *testing.T) {
	t.Parallel()

	rows := tasks(model.TaskRunning, model.TaskSucceeded, model.TaskQueued)
	views := make([]store.DownloadTaskView, 0, len(rows))
	for _, r := range rows {
		views = append(views, store.DownloadTaskView{DownloadTask: r})
	}
	if got, want := FoldTaskViews(views), Fold(rows); got != want {
		t.Errorf("FoldTaskViews = %s, Fold = %s", got, want)
	}
}

// TestStoredStateAlwaysEqualsTheFoldOfItsTasks is the property DESIGN section
// 2.7 names outright: "a property test asserts stored state always equals the
// fold of the task rows".
//
// It is a property over the DATABASE, not over the function: it drives a real
// download's task rows through every combination of task states and reads
// `downloads.state` back each time. That is the only shape that can catch the
// failure it exists for — a second writer somewhere in the package naming a
// state literal of its own — because a test that only called Fold would agree
// with Fold by construction.
//
// The combinations are exhaustive rather than random. Seven task states over
// three tasks is 343 cases, which is cheap, reproducible, and covers every
// precedence pair in the fold's ordering without a seed to print in a failure.
func TestStoredStateAlwaysEqualsTheFoldOfItsTasks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	h.origin.add("Model-Q8_0-00001-of-00003.gguf", 900)
	h.origin.add("Model-Q8_0-00002-of-00003.gguf", 900)
	h.origin.add("Model-Q8_0-00003-of-00003.gguf", 900)
	res := h.create([]string{"Model-Q8_0-00001-of-00003.gguf"})
	id := res.Download.ID

	var taskIDs []string
	if err := h.db.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := h.db.DownloadTasks(ctx, tx, id)
		for _, r := range rows {
			taskIDs = append(taskIDs, r.ID)
		}
		return err
	}); err != nil {
		t.Fatalf("read the task rows: %v", err)
	}
	if len(taskIDs) != 3 {
		t.Fatalf("got %d tasks, want 3", len(taskIDs))
	}

	states := model.DownloadTaskStateValues()
	for _, a := range states {
		for _, b := range states {
			for _, c := range states {
				combo := []model.DownloadTaskState{a, b, c}

				if err := h.db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
					for i, tid := range taskIDs {
						if _, err := h.db.SetDownloadTaskState(ctx, tx, tid, combo[i],
							nil, nil, nil); err != nil {
							return err
						}
					}
					_, err := h.svc.writeState(ctx, tx, id, stateWrite{})
					return err
				}); err != nil {
					t.Fatalf("%v: write: %v", combo, err)
				}

				var (
					stored model.DownloadState
					rows   []store.DownloadTask
				)
				if err := h.db.Read(ctx, func(ctx context.Context, tx store.Tx) error {
					d, err := h.db.Download(ctx, tx, id)
					if err != nil {
						return err
					}
					stored = d.State
					rows, err = h.db.DownloadTasks(ctx, tx, id)
					return err
				}); err != nil {
					t.Fatalf("%v: read back: %v", combo, err)
				}

				if want := Fold(rows); stored != want {
					t.Fatalf("tasks %v: stored downloads.state = %s, but the fold of the "+
						"task rows is %s", combo, stored, want)
				}
			}
		}
	}
}

// TestVerifiedIsTheOnlyPromotionOfTheFold pins the one deviation the property
// above allows, so it cannot quietly become two. Every file is on disk in both
// `verifying` and `succeeded`; what separates them is download-level work the
// task rows do not record, and exactly one commit is allowed to say it is done.
func TestVerifiedIsTheOnlyPromotionOfTheFold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 512)
	res := h.create([]string{"Model-Q4_K_M.gguf"})
	id := res.Download.ID

	for _, tc := range []struct {
		name  string
		task  model.DownloadTaskState
		want  model.DownloadState
		why   string
		verif bool
	}{
		{
			name: "all succeeded, not yet verified", task: model.TaskSucceeded,
			want: model.DownloadVerifying, verif: false,
			why: "the linking and the model transition still have to happen",
		},
		{
			name: "all succeeded and verified", task: model.TaskSucceeded,
			want: model.DownloadSucceeded, verif: true,
			why: "the links are written and the models are ready",
		},
		{
			name: "verified does not promote anything else", task: model.TaskRunning,
			want: model.DownloadRunning, verif: true,
			why: "a download with a live transfer is running whatever the caller believes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got model.DownloadState
			if err := h.db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
				rows, err := h.db.DownloadTasks(ctx, tx, id)
				if err != nil {
					return err
				}
				for _, r := range rows {
					if _, err := h.db.SetDownloadTaskState(ctx, tx, r.ID, tc.task,
						nil, nil, nil); err != nil {
						return err
					}
				}
				got, err = h.svc.writeState(ctx, tx, id, stateWrite{Verified: tc.verif})
				return err
			}); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got != tc.want {
				t.Errorf("state = %s, want %s (%s)", got, tc.want, tc.why)
			}
		})
	}
}
