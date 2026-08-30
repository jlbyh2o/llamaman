package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// TestRecoverOrphansPerKind is §2.3's three-outcome table, kind by kind. The
// wanted states are spelled out here rather than read back from
// model.JobBootTriage on purpose: a test that asked the code under test what it
// expects would agree with any answer, including a wrong one.
func TestRecoverOrphansPerKind(t *testing.T) {
	tests := []struct {
		name string
		kind model.JobKind
		want model.JobState
	}{
		// Idempotent and resumable: re-run from the top, and the domain row
		// returns to `queued` with the job.
		{"download", model.JobModelDownload, model.JobQueued},
		{"cache scan", model.JobCacheScan, model.JobQueued},
		{"toolchain probe", model.JobToolchainProbe, model.JobQueued},

		// Durable state exists outside the job row that only that subsystem knows
		// how to settle, so a domain finalizer resolves it and the domain row keeps
		// its state.
		{"install", model.JobLlamacppInstall, model.JobInterrupted},
		{"activate", model.JobLlamacppActivate, model.JobInterrupted},
		{"bench", model.JobBenchRun, model.JobInterrupted},
		{"self update", model.JobSelfUpdate, model.JobInterrupted},

		// Nothing durable is owed that the row does not already describe.
		{"llamacpp delete", model.JobLlamacppDelete, model.JobFailed},
		{"model verify", model.JobModelVerify, model.JobFailed},
		{"model delete", model.JobModelDelete, model.JobFailed},
		{"maintenance", model.JobMaintenance, model.JobFailed},
	}

	for _, tc := range tests {
		for _, wasState := range []model.JobState{model.JobLeased, model.JobRunning} {
			t.Run(tc.name+" was "+string(wasState), func(t *testing.T) {
				q, s, clock := newTestQueue(t)
				w := register(t, q, tc.kind)
				insertOrphan(t, s, clock, "j-orphan", tc.kind, "dom-1", wasState, deadBootID)

				triaged, err := q.RecoverOrphans(context.Background())
				if err != nil {
					t.Fatalf("RecoverOrphans: %v", err)
				}
				if len(triaged) != 1 {
					t.Fatalf("triaged %d jobs, want 1", len(triaged))
				}
				if triaged[0].State != tc.want {
					t.Errorf("reported state = %s, want %s", triaged[0].State, tc.want)
				}

				got := jobRow(t, s, "j-orphan")
				if got.State != tc.want {
					t.Errorf("state = %s, want %s", got.State, tc.want)
				}
				if got.LeaseOwner != nil {
					t.Errorf("lease_owner = %q after triage, want NULL", *got.LeaseOwner)
				}

				// The domain row moves in the SAME transaction, and the state it is
				// asked for is the one the job row got — which for `interrupted` is
				// the signal that the correct write is a no-op (§2.3).
				if want := []model.JobState{tc.want}; stateDiff(w.domainStates(), want) != "" {
					t.Errorf("domain states = %v, want %v", w.domainStates(), want)
				}

				switch tc.want {
				case model.JobFailed:
					if got.ErrorCode == nil || *got.ErrorCode != string(model.CodeDaemonRestarted) {
						t.Errorf("error_code = %v, want %q", got.ErrorCode, model.CodeDaemonRestarted)
					}
					if got.FinishedAt == nil {
						t.Error("finished_at is NULL on a failed triage")
					}
				case model.JobQueued:
					// The attempt that died is handed back: nothing was attempted, the
					// daemon went away.
					if got.Attempts != 0 {
						t.Errorf("attempts = %d after a requeue triage, want 0", got.Attempts)
					}
				case model.JobInterrupted:
					// `interrupted` counts as live, so nothing else can claim this
					// subject until the finalizer, the user or a retry resolves it.
					if !got.State.IsLive() {
						t.Error("an interrupted job does not hold its subject")
					}
					_, err := q.Enqueue(context.Background(),
						EnqueueParams{Kind: tc.kind, DomainID: "dom-1"})
					if err == nil {
						t.Error("a second job started on an interrupted subject")
					}
				}
			})
		}
	}
}

// TestRecoverOrphansLeavesRowsItDoesNotOwn covers the two rows boot triage must
// walk past: a `paused` job, which is a user decision that must survive a
// restart, and a row this very boot still holds.
func TestRecoverOrphansLeavesRowsItDoesNotOwn(t *testing.T) {
	tests := []struct {
		name  string
		state model.JobState
		owner string
	}{
		{"paused under a dead boot", model.JobPaused, deadBootID},
		{"running under this boot", model.JobRunning, testBootID},
		{"queued", model.JobQueued, deadBootID},
		{"already succeeded", model.JobSucceeded, deadBootID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			w := register(t, q, model.JobModelDownload)
			insertOrphan(t, s, clock, "j-1", model.JobModelDownload, "dl-1", tc.state, tc.owner)

			triaged, err := q.RecoverOrphans(context.Background())
			if err != nil {
				t.Fatalf("RecoverOrphans: %v", err)
			}
			if len(triaged) != 0 {
				t.Fatalf("triaged %d jobs, want 0", len(triaged))
			}
			if got := jobRow(t, s, "j-1"); got.State != tc.state {
				t.Errorf("state = %s, want %s untouched", got.State, tc.state)
			}
			if len(w.domainStates()) != 0 {
				t.Errorf("the domain row was moved to %v", w.domainStates())
			}
		})
	}
}

// TestRecoverOrphansIsOneTransaction: a crash partway through must not leave half
// the fleet triaged and half of it claimable, so a domain write that fails takes
// the whole pass with it.
func TestRecoverOrphansIsOneTransaction(t *testing.T) {
	q, s, clock := newTestQueue(t)
	register(t, q, model.JobModelDownload)

	boom := errors.New("the domain row could not be written")
	bad := &fakeWorker{kind: model.JobCacheScan}
	if err := q.Register(&guardedWorker{fakeWorker: bad, domainErr: boom}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	insertOrphan(t, s, clock, "j-download", model.JobModelDownload, "dl-1", model.JobRunning, deadBootID)
	insertOrphan(t, s, clock, "j-scan", model.JobCacheScan, "scan-1", model.JobRunning, deadBootID)

	if _, err := q.RecoverOrphans(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("RecoverOrphans error = %v, want the domain error", err)
	}
	for _, id := range []string{"j-download", "j-scan"} {
		if got := jobRow(t, s, id); got.State != model.JobRunning {
			t.Errorf("%s = %s, want running — the pass must be all or nothing", id, got.State)
		}
	}
}

// guardedWorker is a fakeWorker whose domain write fails, for the rollback test.
type guardedWorker struct {
	*fakeWorker
	domainErr error
}

func (w *guardedWorker) SetDomainState(ctx context.Context, tx store.Tx, j model.Job, state model.JobState) error {
	return w.domainErr
}

// TestCancelWhenNobodyHoldsTheJob: with no worker to ask, the queue closes the
// job and its domain row itself, in one transaction (§2.3a). All three live
// states with no lease reach the same place.
func TestCancelWhenNobodyHoldsTheJob(t *testing.T) {
	tests := []struct {
		name  string
		state model.JobState
		owner string
	}{
		{"queued", model.JobQueued, ""},
		{"paused", model.JobPaused, ""},
		{"interrupted", model.JobInterrupted, ""},
		// A row still stamped with a dead boot's id is nobody's either.
		{"leased by a dead boot", model.JobLeased, deadBootID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			w := register(t, q, model.JobLlamacppInstall)
			insertOrphan(t, s, clock, "j-1", model.JobLlamacppInstall, "ver-1", tc.state, tc.owner)

			out, err := q.Cancel(context.Background(), "j-1")
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if out.State != model.JobCanceled {
				t.Errorf("returned state = %s, want canceled", out.State)
			}

			got := jobRow(t, s, "j-1")
			if got.State != model.JobCanceled {
				t.Errorf("state = %s, want canceled", got.State)
			}
			if !got.CancelRequested {
				t.Error("cancel_requested = 0 on a canceled job")
			}
			if got.FinishedAt == nil {
				t.Error("finished_at is NULL on a canceled job")
			}
			if got.LeaseOwner != nil {
				t.Errorf("lease_owner = %q, want NULL", *got.LeaseOwner)
			}
			if want := []model.JobState{model.JobCanceled}; stateDiff(w.domainStates(), want) != "" {
				t.Errorf("domain states = %v, want %v", w.domainStates(), want)
			}
		})
	}
}

// TestCancelOfALiveJobIsARequest: while this daemon holds the lease the cancel is
// a REQUEST — the worker decides when it can stop and closes the job itself. The
// queue must not close it out from under a running worker.
func TestCancelOfALiveJobIsARequest(t *testing.T) {
	for _, state := range []model.JobState{model.JobLeased, model.JobRunning} {
		t.Run(string(state), func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			w := register(t, q, model.JobBenchRun)
			insertOrphan(t, s, clock, "j-1", model.JobBenchRun, "bench-1", state, testBootID)

			out, err := q.Cancel(context.Background(), "j-1")
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if out.State != state {
				t.Errorf("returned state = %s, want %s", out.State, state)
			}
			if !out.CancelRequested {
				t.Error("the returned job does not carry the cancel request")
			}

			got := jobRow(t, s, "j-1")
			if got.State != state {
				t.Errorf("state = %s, want %s — a cancel does not close a job a worker holds",
					got.State, state)
			}
			if !got.CancelRequested {
				t.Error("cancel_requested = 0")
			}
			if got.FinishedAt != nil {
				t.Error("finished_at is set on a job that is still running")
			}
			if len(w.domainStates()) != 0 {
				t.Errorf("the domain row was moved to %v by a cancel request", w.domainStates())
			}
		})
	}
}

// TestCancelTerminalAndGuarded covers the two refusals: a job that has nothing
// left to cancel, and a kind that carries its own cut-off (§3.14, D96).
func TestCancelTerminalAndGuarded(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		for _, state := range []model.JobState{model.JobSucceeded, model.JobFailed, model.JobCanceled} {
			q, s, clock := newTestQueue(t)
			register(t, q, model.JobBenchRun)
			insertOrphan(t, s, clock, "j-1", model.JobBenchRun, "bench-1", state, "")

			if _, err := q.Cancel(context.Background(), "j-1"); !errors.Is(err, ErrNotCancelable) {
				t.Errorf("Cancel of a %s job = %v, want ErrNotCancelable", state, err)
			}
		}
	})

	t.Run("guard refuses", func(t *testing.T) {
		q, s, clock := newTestQueue(t)
		w := register(t, q, model.JobSelfUpdate)
		refusal := model.Error{
			Code:    model.CodeSelfUpdateNotCancelable,
			Message: "the marker is on disk and the swap belongs to systemd",
		}
		w.guard = func(ctx context.Context, tx store.Tx, j model.Job) error { return refusal }
		insertOrphan(t, s, clock, "j-1", model.JobSelfUpdate, "su-1", model.JobRunning, testBootID)

		_, err := q.Cancel(context.Background(), "j-1")
		if code := asModelError(t, err).Code; code != model.CodeSelfUpdateNotCancelable {
			t.Errorf("code = %s, want %s", code, model.CodeSelfUpdateNotCancelable)
		}
		got := jobRow(t, s, "j-1")
		if got.CancelRequested {
			t.Error("a refused cancel still raised cancel_requested")
		}
		if got.State != model.JobRunning {
			t.Errorf("state = %s, want running", got.State)
		}
	})

	t.Run("guard accepts", func(t *testing.T) {
		q, s, clock := newTestQueue(t)
		w := register(t, q, model.JobLlamacppActivate)
		var sawJob string
		w.guard = func(ctx context.Context, tx store.Tx, j model.Job) error {
			sawJob = j.ID
			return nil
		}
		insertOrphan(t, s, clock, "j-1", model.JobLlamacppActivate, "ver-1", model.JobQueued, "")

		if _, err := q.Cancel(context.Background(), "j-1"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if sawJob != "j-1" {
			t.Errorf("the guard saw %q, want j-1", sawJob)
		}
		if got := jobRow(t, s, "j-1"); got.State != model.JobCanceled {
			t.Errorf("state = %s, want canceled", got.State)
		}
	})

	t.Run("missing job", func(t *testing.T) {
		q, _, _ := newTestQueue(t)
		if _, err := q.Cancel(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Cancel of a missing job = %v, want ErrNotFound", err)
		}
	})
}

// TestCancelObservedByARunningWorker closes the loop the two tests above leave
// open: the heartbeat reads `cancel_requested`, cuts the worker's context, and
// the worker's own Canceled outcome is what closes the job beside its domain row.
func TestCancelObservedByARunningWorker(t *testing.T) {
	q, s, _ := newTestQueue(t, func(o *Options) {
		o.HeartbeatEvery = 2 * time.Millisecond
	})

	started := make(chan struct{})
	w := register(t, q, model.JobModelDownload)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		close(started)
		<-ctx.Done()
		if !task.CancelRequested() {
			t.Error("the task does not report the cancel that stopped it")
		}
		return Canceled(nil), nil
	}

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := q.RunOnce(context.Background()); err != nil {
			t.Errorf("RunOnce: %v", err)
		}
	}()

	<-started
	if _, err := q.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never observed the cancel")
	}

	got := jobRow(t, s, j.ID)
	if got.State != model.JobCanceled {
		t.Errorf("state = %s, want canceled", got.State)
	}
	if want := []model.JobState{model.JobCanceled}; stateDiff(w.domainStates(), want) != "" {
		t.Errorf("domain states = %v, want %v", w.domainStates(), want)
	}
}

// TestCancelBetweenClaimAndStart is the race the start transaction closes: a
// cancel that lands after the lease and before the first line of work closes the
// job without ever calling the worker.
func TestCancelBetweenClaimAndStart(t *testing.T) {
	q, s, _ := newTestQueue(t)
	w := register(t, q, model.JobModelVerify)

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelVerify, DomainID: "mod-1"})

	task, err := q.claim(context.Background())
	if err != nil || task == nil {
		t.Fatalf("claim: task=%v err=%v", task, err)
	}
	if _, err := q.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	q.execute(context.Background(), task)

	if w.runCount() != 0 {
		t.Errorf("the worker ran %d times on a job canceled before it started", w.runCount())
	}
	got := jobRow(t, s, j.ID)
	if got.State != model.JobCanceled {
		t.Errorf("state = %s, want canceled", got.State)
	}
	if want := []model.JobState{model.JobCanceled}; stateDiff(w.domainStates(), want) != "" {
		t.Errorf("domain states = %v, want %v", w.domainStates(), want)
	}
}

// TestRetry covers the three states a job can stop in without being finished
// with, and the two it cannot be retried from.
func TestRetry(t *testing.T) {
	tests := []struct {
		name    string
		state   model.JobState
		owner   string
		wantErr error
	}{
		{"failed", model.JobFailed, "", nil},
		{"canceled", model.JobCanceled, "", nil},
		// D4's warm build directory waits in exactly this state.
		{"interrupted", model.JobInterrupted, "", nil},
		{"succeeded", model.JobSucceeded, "", ErrNotRetryable},
		{"running", model.JobRunning, testBootID, ErrNotRetryable},
		{"queued", model.JobQueued, "", ErrNotRetryable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			w := register(t, q, model.JobLlamacppInstall)

			// The budget is already spent and a cancel already recorded: a job that
			// exhausted its attempts is the most likely thing a human presses Retry
			// on, and a retry that left either alone would be handed straight back to
			// a queue that refuses to run it, or canceled again on the first claim.
			row := jobIn(clock, "j-1", model.JobLlamacppInstall, "ver-1", tc.state, tc.owner)
			row.Attempts, row.MaxAttempts = 3, 3
			row.CancelRequested = true
			insertJob(t, s, row)

			out, err := q.Retry(context.Background(), "j-1")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Retry = %v, want %v", err, tc.wantErr)
				}
				if got := jobRow(t, s, "j-1"); got.State != tc.state {
					t.Errorf("a refused retry moved the state to %s", got.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("Retry: %v", err)
			}
			if out.State != model.JobQueued {
				t.Errorf("returned state = %s, want queued", out.State)
			}

			got := jobRow(t, s, "j-1")
			if got.State != model.JobQueued {
				t.Errorf("state = %s, want queued", got.State)
			}
			if got.MaxAttempts <= got.Attempts {
				t.Errorf("max_attempts = %d with attempts = %d: the retry cannot run",
					got.MaxAttempts, got.Attempts)
			}
			if got.CancelRequested {
				t.Error("cancel_requested survived a retry — the job would cancel itself again")
			}
			if got.ErrorCode != nil || got.FinishedAt != nil {
				t.Error("a retried job still wears its previous ending")
			}
			if want := []model.JobState{model.JobQueued}; stateDiff(w.domainStates(), want) != "" {
				t.Errorf("domain states = %v, want %v", w.domainStates(), want)
			}

			// And it actually runs again.
			runOnce(t, q)
			if got := jobRow(t, s, "j-1"); got.State != model.JobSucceeded {
				t.Errorf("state after the retry ran = %s, want succeeded", got.State)
			}
		})
	}
}

// TestPauseResume is §2.3a's "pause/resume moves both rows", and the reason
// `paused` is a jobs state at all: it releases the lease while still holding the
// subject, so a paused download can neither hold a lease forever nor free its
// subject for a duplicate job.
func TestPauseResume(t *testing.T) {
	q, s, clock := newTestQueue(t)
	ctx := context.Background()
	w := register(t, q, model.JobModelDownload)
	insertOrphan(t, s, clock, "j-1", model.JobModelDownload, "dl-1", model.JobRunning, testBootID)

	if err := q.Pause(ctx, "j-1", nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	got := jobRow(t, s, "j-1")
	if got.State != model.JobPaused {
		t.Errorf("state = %s, want paused", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease_owner = %q on a paused job, want NULL", *got.LeaseOwner)
	}
	if _, err := q.Enqueue(ctx, EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"}); err == nil {
		t.Error("a paused job freed its subject for a duplicate")
	}

	// A caller-supplied domain write wins over the worker's DomainWriter, which
	// is what lets POST /downloads/{id}/pause write the row it knows about.
	var supplied model.JobState
	if err := q.Resume(ctx, "j-1", func(ctx context.Context, tx store.Tx, state model.JobState) error {
		supplied = state
		return nil
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if supplied != model.JobQueued {
		t.Errorf("the supplied domain write saw %s, want queued", supplied)
	}
	if got := jobRow(t, s, "j-1"); got.State != model.JobQueued {
		t.Errorf("state = %s, want queued", got.State)
	}
	if want := []model.JobState{model.JobPaused}; stateDiff(w.domainStates(), want) != "" {
		t.Errorf("domain states = %v, want %v", w.domainStates(), want)
	}
}

// TestLiveJobForSubject is the read behind a `409 job_in_flight` that names the
// job it collided with before the caller has built a whole request.
func TestLiveJobForSubject(t *testing.T) {
	q, _, _ := newTestQueue(t)
	ctx := context.Background()

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobBenchRun, DomainID: "bench-1"})

	got, err := q.LiveJobFor(ctx, model.JobBenchRun, "bench-1")
	if err != nil {
		t.Fatalf("LiveJobFor: %v", err)
	}
	if got.ID != j.ID {
		t.Errorf("LiveJobFor = %s, want %s", got.ID, j.ID)
	}
	if _, err := q.LiveJobFor(ctx, model.JobBenchRun, "bench-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LiveJobFor on a free subject = %v, want ErrNotFound", err)
	}
	if _, err := q.LiveJobFor(ctx, model.JobKind("nope"), "x"); !errors.Is(err, errUnknownKind) {
		t.Errorf("LiveJobFor on an unknown kind = %v, want errUnknownKind", err)
	}
}

// TestJobAndList cover the two reads behind §3.14's job endpoints.
func TestJobAndList(t *testing.T) {
	q, _, _ := newTestQueue(t)
	ctx := context.Background()

	a := mustEnqueue(t, q, EnqueueParams{Kind: model.JobCacheScan, DomainID: "scan-1"})
	b := mustEnqueue(t, q, EnqueueParams{Kind: model.JobBenchRun, DomainID: "bench-1"})

	got, err := q.Job(ctx, a.ID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("Job = (%s, %v), want %s", got.ID, err, a.ID)
	}
	if _, err := q.Job(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Job on a missing id = %v, want ErrNotFound", err)
	}

	// `?state=active` is the live states.
	active, err := q.List(ctx, store.JobFilter{States: model.LiveJobStates()})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("%d active jobs, want 2", len(active))
	}
	// Newest first.
	if active[0].ID != b.ID {
		t.Errorf("List[0] = %s, want the newest job %s", active[0].ID, b.ID)
	}
}
