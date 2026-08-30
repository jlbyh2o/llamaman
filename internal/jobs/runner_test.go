package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// runOnce claims and runs exactly one job, and insists that it found one.
func runOnce(t *testing.T, q *Queue) {
	t.Helper()
	ran, err := q.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !ran {
		t.Fatal("RunOnce found nothing to run")
	}
}

// TestRunOnceOutcomes walks a worker's whole vocabulary of endings and asserts
// the job row and the domain row moved together each time (§2.3a).
func TestRunOnceOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		outcome     func() (Outcome, error)
		maxAttempts int
		wantState   model.JobState
		wantCode    string
		wantDomain  []model.JobState
	}{
		{
			name:       "succeeded",
			outcome:    func() (Outcome, error) { return Succeeded(nil), nil },
			wantState:  model.JobSucceeded,
			wantDomain: []model.JobState{model.JobSucceeded},
		},
		{
			name:       "failed with a code of its own",
			outcome:    func() (Outcome, error) { return Failed("delete_incomplete", "half a tree", nil), nil },
			wantState:  model.JobFailed,
			wantCode:   "delete_incomplete",
			wantDomain: []model.JobState{model.JobFailed},
		},
		{
			name:       "canceled",
			outcome:    func() (Outcome, error) { return Canceled(nil), nil },
			wantState:  model.JobCanceled,
			wantDomain: []model.JobState{model.JobCanceled},
		},
		{
			// A bare error is the failure a worker did not anticipate: retryable,
			// and wearing the fallback code rather than daemon_restarted.
			name:       "bare error",
			outcome:    func() (Outcome, error) { return Outcome{}, errors.New("the disk went away") },
			wantState:  model.JobFailed,
			wantCode:   CodeInternalError,
			wantDomain: []model.JobState{model.JobFailed},
		},
		{
			// A retryable failure on the attempt that exhausts the budget is
			// terminal: §2.3 retries only while attempts < max_attempts.
			name:        "retryable failure out of budget",
			outcome:     func() (Outcome, error) { return RetryableFailure("network", "reset", nil), nil },
			maxAttempts: 1,
			wantState:   model.JobFailed,
			wantCode:    "network",
			wantDomain:  []model.JobState{model.JobFailed},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, s, _ := newTestQueue(t)
			w := register(t, q, model.JobModelVerify)
			w.run = func(ctx context.Context, task *Task) (Outcome, error) { return tc.outcome() }

			j := mustEnqueue(t, q, EnqueueParams{
				Kind: model.JobModelVerify, DomainID: "mod-1", MaxAttempts: tc.maxAttempts,
			})
			runOnce(t, q)

			got := jobRow(t, s, j.ID)
			if got.State != tc.wantState {
				t.Errorf("state = %s, want %s", got.State, tc.wantState)
			}
			if tc.wantCode != "" && (got.ErrorCode == nil || *got.ErrorCode != tc.wantCode) {
				t.Errorf("error_code = %v, want %q", got.ErrorCode, tc.wantCode)
			}
			if got.FinishedAt == nil {
				t.Error("finished_at is NULL on a terminal job")
			}
			if got.LeaseOwner != nil {
				t.Errorf("lease_owner = %q on a terminal job, want NULL", *got.LeaseOwner)
			}
			if got.StartedAt == nil {
				t.Error("started_at is NULL on a job that ran")
			}
			if diff := stateDiff(w.domainStates(), tc.wantDomain); diff != "" {
				t.Errorf("domain states: %s", diff)
			}
		})
	}
}

// TestRunOnceRetriesWithBackoff walks a job through the whole of §2.3's retry
// rule: back to `queued` at `now + min(60s, 2^attempts x 2s)` while the budget
// lasts, wearing the error that caused it, and terminal on the attempt that
// spends the last of it.
func TestRunOnceRetriesWithBackoff(t *testing.T) {
	q, s, clock := newTestQueue(t)
	w := register(t, q, model.JobCacheScan)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		return RetryableFailure("scan_failed", "the cache moved", nil), nil
	}

	j := mustEnqueue(t, q, EnqueueParams{
		Kind: model.JobCacheScan, DomainID: "scan-1", MaxAttempts: 3,
	})

	for attempt := 1; attempt <= 2; attempt++ {
		runOnce(t, q)
		got := jobRow(t, s, j.ID)

		if got.State != model.JobQueued {
			t.Fatalf("attempt %d: state = %s, want queued", attempt, got.State)
		}
		if got.Attempts != attempt {
			t.Errorf("attempt %d: attempts = %d", attempt, got.Attempts)
		}
		wantRunAfter := clock.now().Add(model.JobBackoff(attempt)).UnixMilli()
		if got.RunAfter != wantRunAfter {
			t.Errorf("attempt %d: run_after = %d, want %d (backoff %s)",
				attempt, got.RunAfter, wantRunAfter, model.JobBackoff(attempt))
		}
		if got.ErrorCode == nil || *got.ErrorCode != "scan_failed" {
			t.Errorf("attempt %d: a retry lost the error that caused it", attempt)
		}
		if got.FinishedAt != nil {
			t.Errorf("attempt %d: finished_at is set on a job that will run again", attempt)
		}

		// The backoff is real: nothing is claimable until the clock reaches it.
		if ran, err := q.RunOnce(context.Background()); err != nil || ran {
			t.Fatalf("attempt %d: a job inside its backoff was claimed (ran=%v, err=%v)",
				attempt, ran, err)
		}
		clock.advance(model.JobBackoff(attempt))
	}

	runOnce(t, q)
	got := jobRow(t, s, j.ID)
	if got.State != model.JobFailed {
		t.Errorf("state after the budget = %s, want failed", got.State)
	}
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
	if want := []model.JobState{model.JobQueued, model.JobQueued, model.JobFailed}; stateDiff(w.domainStates(), want) != "" {
		t.Errorf("domain states = %v, want %v", w.domainStates(), want)
	}
}

// TestStarterDefersWithoutSpendingBudget is §2.3's build-lease queue: a Starter
// that cannot take the singleton lease leaves its job `queued` for 15 s, and the
// UI says "waiting for the running build", which is a queue and not an error. It
// must therefore wear no error and spend no part of the attempt budget.
func TestStarterDefersWithoutSpendingBudget(t *testing.T) {
	q, s, clock := newTestQueue(t)
	w := register(t, q, model.JobLlamacppInstall)

	deferrals := 0
	w.start = func(ctx context.Context, tx store.Tx, j model.Job) error {
		deferrals++
		if deferrals == 1 {
			return Defer(15 * time.Second)
		}
		return nil
	}

	j := mustEnqueue(t, q, EnqueueParams{
		Kind: model.JobLlamacppInstall, DomainID: "ver-1", MaxAttempts: 1,
	})

	runOnce(t, q)
	got := jobRow(t, s, j.ID)
	if got.State != model.JobQueued {
		t.Fatalf("state = %s, want queued", got.State)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d after a deferral, want 0 — a queue is not an attempt", got.Attempts)
	}
	if got.ErrorCode != nil {
		t.Errorf("error_code = %q after a deferral, want NULL", *got.ErrorCode)
	}
	if want := clock.now().Add(15 * time.Second).UnixMilli(); got.RunAfter != want {
		t.Errorf("run_after = %d, want %d", got.RunAfter, want)
	}
	if w.runCount() != 0 {
		t.Errorf("the worker ran %d times on a deferral, want 0", w.runCount())
	}

	// The budget survived, so the job still runs when the lease frees up.
	clock.advance(15 * time.Second)
	runOnce(t, q)
	if got := jobRow(t, s, j.ID); got.State != model.JobSucceeded {
		t.Errorf("state after the deferral cleared = %s, want succeeded", got.State)
	}
}

// TestStarterCommitsWithTheRunningTransition proves a Starter's write lands in
// the same transaction that moves the job to `running` — §2.3a for the one
// transition the worker makes on the way IN rather than on the way out — and
// that a Starter's error fails the job rather than silently running it.
func TestStarterCommitsWithTheRunningTransition(t *testing.T) {
	q, s, _ := newTestQueue(t)
	w := register(t, q, model.JobBenchRun)

	var sawState model.JobState
	w.start = func(ctx context.Context, tx store.Tx, j model.Job) error {
		sawState = j.State
		return nil
	}
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		if task.Job().State != model.JobRunning {
			t.Errorf("the task's job is %s, want running", task.Job().State)
		}
		return Succeeded(nil), nil
	}

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobBenchRun, DomainID: "bench-1"})
	runOnce(t, q)

	if sawState != model.JobRunning {
		t.Errorf("Start saw state %s, want running", sawState)
	}
	if got := jobRow(t, s, j.ID); got.State != model.JobSucceeded {
		t.Errorf("state = %s, want succeeded", got.State)
	}
}

// TestWorkerPanicBecomesAFailure keeps one worker's bug from taking the daemon
// down with it, and records it distinguishably from daemon_restarted.
func TestWorkerPanicBecomesAFailure(t *testing.T) {
	q, s, _ := newTestQueue(t)
	w := register(t, q, model.JobModelDelete)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) { panic("nil map") }

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelDelete, DomainID: "mod-1"})
	runOnce(t, q)

	got := jobRow(t, s, j.ID)
	if got.State != model.JobFailed {
		t.Errorf("state = %s, want failed", got.State)
	}
	if got.ErrorCode == nil || *got.ErrorCode != CodeInternalError {
		t.Errorf("error_code = %v, want %q", got.ErrorCode, CodeInternalError)
	}
}

// TestProgressReporting writes the column §6.5 streams build phases into and
// §10 streams point counts into.
func TestProgressReporting(t *testing.T) {
	q, s, _ := newTestQueue(t)
	w := register(t, q, model.JobBenchRun)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		if err := task.SetProgress(ctx, map[string]int{"points_done": 3, "points_total": 8}); err != nil {
			return Outcome{}, err
		}
		return Succeeded(nil), nil
	}

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobBenchRun, DomainID: "bench-1"})
	runOnce(t, q)

	got := jobRow(t, s, j.ID)
	if got.ProgressJSON == nil || *got.ProgressJSON != `{"points_done":3,"points_total":8}` {
		t.Errorf("progress_json = %v", got.ProgressJSON)
	}
}

// TestShutdownLeavesTheJobRunning is the rule that makes boot triage meaningful:
// a daemon on its way out does not decide what a running job becomes. It leaves
// the row `running`, and the NEXT boot triages it against §2.3's table.
func TestShutdownLeavesTheJobRunning(t *testing.T) {
	q, s, _ := newTestQueue(t)
	w := register(t, q, model.JobModelDownload)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		close(started)
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	}

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := q.RunOnce(ctx); err != nil {
			t.Errorf("RunOnce: %v", err)
		}
	}()

	<-started
	cancel()
	<-done

	got := jobRow(t, s, j.ID)
	if got.State != model.JobRunning {
		t.Errorf("state = %s, want running — shutdown is not a transition", got.State)
	}
	if len(w.domainStates()) != 0 {
		t.Errorf("shutdown moved the domain row to %v", w.domainStates())
	}

	// §9.4 step 5: the lease is brought to its end without moving any state.
	n, err := q.ReleaseLeases(context.Background())
	if err != nil {
		t.Fatalf("ReleaseLeases: %v", err)
	}
	if n != 1 {
		t.Errorf("ReleaseLeases released %d leases, want 1", n)
	}
	if got := jobRow(t, s, j.ID); got.State != model.JobRunning {
		t.Errorf("ReleaseLeases changed the state to %s", got.State)
	}
}

// TestLeaseLostDropsTheOutcome is the one thing a worker must never do: write to
// a job it no longer owns. A pause releases the lease under a running download
// (§2.3a); the heartbeat notices, cuts the worker's context, and whatever the
// worker then says about the job is discarded — the pause must survive it.
func TestLeaseLostDropsTheOutcome(t *testing.T) {
	q, s, _ := newTestQueue(t, func(o *Options) {
		o.HeartbeatEvery = 2 * time.Millisecond
	})

	started := make(chan struct{})
	w := register(t, q, model.JobModelDownload)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		close(started)
		<-ctx.Done()
		if !task.LeaseLost() {
			t.Error("the task does not report the lease it lost")
		}
		return Succeeded(nil), nil
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
	if err := q.Pause(context.Background(), j.ID, nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never noticed the lost lease")
	}

	got := jobRow(t, s, j.ID)
	if got.State != model.JobPaused {
		t.Errorf("state = %s, want paused — the outcome must be dropped", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease_owner = %q, want NULL", *got.LeaseOwner)
	}
	if want := []model.JobState{model.JobPaused}; stateDiff(w.domainStates(), want) != "" {
		t.Errorf("domain states = %v, want %v — a worker that lost its lease wrote one",
			w.domainStates(), want)
	}
}

// TestRunDrivesTheQueue exercises the background loop rather than the RunOnce
// seam, so the wake path and the concurrency slots are covered too.
func TestRunDrivesTheQueue(t *testing.T) {
	q, _, _ := newTestQueue(t, func(o *Options) {
		o.PollEvery = 5 * time.Millisecond
		o.Concurrency = 2
	})

	done := make(chan string, 3)
	w := register(t, q, model.JobModelVerify)
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		done <- task.Job().ID
		return Succeeded(nil), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		q.Run(ctx)
	}()

	ids := make(map[string]bool, 3)
	for i := range 3 {
		j := mustEnqueue(t, q, EnqueueParams{
			Kind: model.JobModelVerify, DomainID: string(rune('a' + i)),
		})
		ids[j.ID] = true
	}

	deadline := time.After(10 * time.Second)
	for range 3 {
		select {
		case id := <-done:
			if !ids[id] {
				t.Errorf("ran an unexpected job %s", id)
			}
			delete(ids, id)
		case <-deadline:
			t.Fatalf("timed out with %d jobs unrun", len(ids))
		}
	}

	cancel()
	<-runDone

	jobs, err := q.List(context.Background(), store.JobFilter{
		States: []model.JobState{model.JobSucceeded},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("%d succeeded jobs, want 3", len(jobs))
	}
}

// TestClaimSkipsUnregisteredKinds keeps a daemon from burning the attempt budget
// of work it cannot run: a kind with no worker is never leased at all.
func TestClaimSkipsUnregisteredKinds(t *testing.T) {
	q, s, _ := newTestQueue(t)
	register(t, q, model.JobCacheScan)

	orphaned := mustEnqueue(t, q, EnqueueParams{Kind: model.JobSelfUpdate, DomainID: "su-1"})
	runnable := mustEnqueue(t, q, EnqueueParams{Kind: model.JobCacheScan, DomainID: "scan-1"})

	runOnce(t, q)
	if got := jobRow(t, s, runnable.ID); got.State != model.JobSucceeded {
		t.Errorf("the registered kind is %s, want succeeded", got.State)
	}

	if ran, err := q.RunOnce(context.Background()); err != nil || ran {
		t.Fatalf("claimed a job with no worker (ran=%v, err=%v)", ran, err)
	}
	got := jobRow(t, s, orphaned.ID)
	if got.State != model.JobQueued || got.Attempts != 0 {
		t.Errorf("the unregistered kind is %s with %d attempts, want queued with 0",
			got.State, got.Attempts)
	}
}

// TestEmptyRegistryClaimsNothing is the same rule at its limit — a lease with no
// kind filter would claim everything.
func TestEmptyRegistryClaimsNothing(t *testing.T) {
	q, _, _ := newTestQueue(t)
	mustEnqueue(t, q, EnqueueParams{Kind: model.JobMaintenance})

	if ran, err := q.RunOnce(context.Background()); err != nil || ran {
		t.Fatalf("an empty registry claimed work (ran=%v, err=%v)", ran, err)
	}
}

// TestBareWorkerNeedsNoDomainRow covers `maintenance`, the one kind whose job row
// IS the record: a Worker that implements nothing else must still close cleanly.
func TestBareWorkerNeedsNoDomainRow(t *testing.T) {
	q, s, _ := newTestQueue(t)
	if err := q.Register(&bareWorker{kind: model.JobMaintenance}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	j := mustEnqueue(t, q, EnqueueParams{Kind: model.JobMaintenance})
	runOnce(t, q)

	if got := jobRow(t, s, j.ID); got.State != model.JobSucceeded {
		t.Errorf("state = %s, want succeeded", got.State)
	}
}

// TestLeasePriorityOrder pins the queue's ordering at the engine level: lower
// priority first, then the job that has been ready longest.
func TestLeasePriorityOrder(t *testing.T) {
	q, _, clock := newTestQueue(t)
	w := register(t, q, model.JobModelVerify)

	var order []string
	w.run = func(ctx context.Context, task *Task) (Outcome, error) {
		order = append(order, task.Job().SubjectID)
		return Succeeded(nil), nil
	}

	mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelVerify, DomainID: "normal", Priority: 100})
	clock.advance(time.Millisecond)
	mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelVerify, DomainID: "urgent", Priority: 10})
	clock.advance(time.Millisecond)
	mustEnqueue(t, q, EnqueueParams{Kind: model.JobModelVerify, DomainID: "later", Priority: 100})

	for range 3 {
		runOnce(t, q)
	}
	want := []string{"urgent", "normal", "later"}
	if stateDiffStrings(order, want) != "" {
		t.Errorf("run order = %v, want %v", order, want)
	}
}

// stateDiff reports a readable difference between two state sequences.
func stateDiff(got, want []model.JobState) string {
	if len(got) != len(want) {
		return "got " + statesString(got) + ", want " + statesString(want)
	}
	for i := range got {
		if got[i] != want[i] {
			return "got " + statesString(got) + ", want " + statesString(want)
		}
	}
	return ""
}

func statesString(s []model.JobState) string {
	out := "["
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += string(v)
	}
	return out + "]"
}

func stateDiffStrings(got, want []string) string {
	if len(got) != len(want) {
		return "length"
	}
	for i := range got {
		if got[i] != want[i] {
			return "order"
		}
	}
	return ""
}

// TestStartFailureDoesNotStrandTheSubject covers the transition with the
// smallest window and the worst failure mode: a Starter that returns a plain
// error. The start transaction has rolled back, so the row is still `leased`
// under this boot's own id — a state nothing else in this process looks at,
// because LeaseNextJob claims only `queued`, Retry accepts only a job that has
// stopped, and boot triage keys off a lease_owner that is NOT this boot's. The
// subject is live for `idx_jobs_one_live_per_subject` all the while, so leaving
// it there wedges the subject — D70's build_lease is acquired inside Start, so
// one store error would block that `llamacpp_versions` id until a restart.
// Nothing ran, so it is an ordinary failed attempt: §2.3's retry while the
// budget lasts, terminal `failed` when it is spent.
func TestStartFailureDoesNotStrandTheSubject(t *testing.T) {
	tests := []struct {
		name        string
		maxAttempts int
		wantState   model.JobState
		wantDomain  []model.JobState
	}{
		{
			name:        "budget left: back to queued with the error visible",
			maxAttempts: 3,
			wantState:   model.JobQueued,
			wantDomain:  []model.JobState{model.JobQueued},
		},
		{
			name:        "budget spent: terminal failure, subject released",
			maxAttempts: 1,
			wantState:   model.JobFailed,
			wantDomain:  []model.JobState{model.JobFailed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			w := register(t, q, model.JobLlamacppInstall)
			w.start = func(ctx context.Context, tx store.Tx, j model.Job) error {
				return errors.New("acquire build_lease: database is locked")
			}

			j := mustEnqueue(t, q, EnqueueParams{
				Kind: model.JobLlamacppInstall, DomainID: "ver-1", MaxAttempts: tt.maxAttempts,
			})
			runOnce(t, q)

			got := jobRow(t, s, j.ID)
			if got.State != tt.wantState {
				t.Errorf("state = %s, want %s", got.State, tt.wantState)
			}
			if got.LeaseOwner != nil {
				t.Errorf("lease_owner = %q, want the lease released", *got.LeaseOwner)
			}
			if got.ErrorCode == nil || *got.ErrorCode != CodeInternalError {
				t.Errorf("error_code = %v, want %q", got.ErrorCode, CodeInternalError)
			}
			if n := w.runCount(); n != 0 {
				t.Errorf("the worker ran %d times; Start never succeeded", n)
			}
			if d := stateDiff(w.domainStates(), tt.wantDomain); d != "" {
				t.Errorf("domain states: %s", d)
			}

			// The subject is usable again, which is the whole point: either the
			// queue can claim this job once its backoff elapses, or — once the
			// job is terminal — a new one may take the subject.
			if tt.wantState == model.JobQueued {
				clock.advance(time.Minute)
				runOnce(t, q)
			} else {
				mustEnqueue(t, q, EnqueueParams{Kind: model.JobLlamacppInstall, DomainID: "ver-1"})
			}
		})
	}
}

// TestUnclosableJobIsTriagedRatherThanStranded is the same gap at the other end
// of a run: the worker finished but the closing transaction failed — the domain
// write it carries is the likeliest reason. The row is `running` under this
// boot's lease and nothing would ever look at it again, so it is triaged here,
// by the daemon that still owns it, against §2.3's own three-outcome table. The
// `interrupted` bucket matters most: a bench that stopped production instances
// must not be marked `failed`, because that destroys the finalizer's input.
func TestUnclosableJobIsTriagedRatherThanStranded(t *testing.T) {
	tests := []struct {
		name       string
		kind       model.JobKind
		domainID   string
		wantState  model.JobState
		wantDomain []model.JobState
	}{
		{
			name:       "resumable kinds re-run from the top",
			kind:       model.JobModelDownload,
			domainID:   "dl-1",
			wantState:  model.JobQueued,
			wantDomain: []model.JobState{model.JobQueued},
		},
		{
			name:       "a kind that owes durable work is interrupted, never failed",
			kind:       model.JobBenchRun,
			domainID:   "bench-1",
			wantState:  model.JobInterrupted,
			wantDomain: []model.JobState{model.JobInterrupted},
		},
		{
			name:       "a kind that owes nothing durable is closed failed",
			kind:       model.JobModelDelete,
			domainID:   "mod-1",
			wantState:  model.JobFailed,
			wantDomain: []model.JobState{model.JobFailed},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			q, s, _ := newTestQueue(t)
			w := register(t, q, tt.kind)
			w.run = func(ctx context.Context, task *Task) (Outcome, error) {
				return Succeeded(func(ctx context.Context, tx store.Tx, state model.JobState) error {
					return errors.New("the domain row could not be written")
				}), nil
			}

			j := mustEnqueue(t, q, EnqueueParams{Kind: tt.kind, DomainID: tt.domainID, MaxAttempts: 3})
			runOnce(t, q)

			got := jobRow(t, s, j.ID)
			if got.State != tt.wantState {
				t.Errorf("state = %s, want %s", got.State, tt.wantState)
			}
			if got.LeaseOwner != nil {
				t.Errorf("lease_owner = %q, want the lease released", *got.LeaseOwner)
			}
			if d := stateDiff(w.domainStates(), tt.wantDomain); d != "" {
				t.Errorf("domain states: %s", d)
			}

			switch tt.wantState {
			case model.JobQueued:
				// Claimable again, with the attempt refunded.
				if got.Attempts != 0 {
					t.Errorf("attempts = %d, want the claim refunded", got.Attempts)
				}
				runOnce(t, q)
			case model.JobInterrupted:
				// Live for the unique index, and exactly the state the user's
				// Retry accepts — the subject is held by something resolvable
				// rather than by a lease nobody will ever release.
				if _, err := q.Retry(ctx, j.ID); err != nil {
					t.Errorf("Retry on the interrupted job: %v", err)
				}
			default:
				if got.ErrorCode == nil || *got.ErrorCode != CodeInternalError {
					t.Errorf("error_code = %v, want %q", got.ErrorCode, CodeInternalError)
				}
				if got.ErrorCode != nil && *got.ErrorCode == string(model.CodeDaemonRestarted) {
					t.Error("error_code = daemon_restarted, but no daemon restarted")
				}
				mustEnqueue(t, q, EnqueueParams{Kind: tt.kind, DomainID: tt.domainID})
			}
		})
	}
}
