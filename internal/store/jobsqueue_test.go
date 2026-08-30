package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// leasedJob is a job as the queue engine holds it: claimed, one attempt spent,
// a lease horizon stamped with a boot id.
func leasedJob(id string, kind model.JobKind, subjectID, owner string) model.Job {
	j := newJob(id, kind, subjectID)
	j.State = model.JobRunning
	j.Attempts = 1
	j.LeaseOwner = ptr(owner)
	j.LeaseExpiresAt = ptr(int64(2000))
	return j
}

// TestTouchJobLease proves the one statement a heartbeat is: it extends the
// lease and answers "has somebody asked me to stop" together, and it answers
// ErrNotFound for every way a worker can have lost the job — which is the signal
// to abandon the work rather than keep writing to a job somebody else now owns.
func TestTouchJobLease(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(j *model.Job)
		owner          string
		wantNotFound   bool
		wantCanceled   bool
		wantExpiration int64
	}{
		{name: "the holder", owner: "boot-1", wantExpiration: 5000},
		{
			name:         "cancel requested",
			mutate:       func(j *model.Job) { j.CancelRequested = true },
			owner:        "boot-1",
			wantCanceled: true, wantExpiration: 5000,
		},
		{name: "another boot", owner: "boot-2", wantNotFound: true, wantExpiration: 2000},
		{
			name:         "the lease was released by a pause",
			mutate:       func(j *model.Job) { j.State = model.JobPaused; j.LeaseOwner = nil },
			owner:        "boot-1",
			wantNotFound: true,
		},
		{
			name:         "the job already finished",
			mutate:       func(j *model.Job) { j.State = model.JobSucceeded },
			owner:        "boot-1",
			wantNotFound: true, wantExpiration: 2000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			j := leasedJob("j1", model.JobModelDownload, "dl-1", "boot-1")
			if tc.mutate != nil {
				tc.mutate(&j)
			}
			mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, j) })

			var (
				canceled bool
				err      error
			)
			werr := s.Write(ctx, func(ctx context.Context, tx Tx) error {
				canceled, err = s.TouchJobLease(ctx, tx, "j1", tc.owner, 5000)
				return nil
			})
			if werr != nil {
				t.Fatalf("write: %v", werr)
			}

			if tc.wantNotFound {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("TouchJobLease = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TouchJobLease: %v", err)
			}
			if canceled != tc.wantCanceled {
				t.Errorf("cancel_requested = %v, want %v", canceled, tc.wantCanceled)
			}
			got, err := s.Job(ctx, s.RO, "j1")
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if got.LeaseExpiresAt == nil || *got.LeaseExpiresAt != tc.wantExpiration {
				t.Errorf("lease_expires_at = %v, want %d", got.LeaseExpiresAt, tc.wantExpiration)
			}
		})
	}
}

// TestRequeueAndDeferJob pins the one column that separates a retry from a
// queue. A retry KEEPS the attempt it spent and the error that caused it — a job
// sitting in `queued` for 32 s with no stated reason is indistinguishable from
// one that never ran. A deferral hands the attempt BACK and wears no error,
// because §2.3's build-lease wait is a queue and not a failure.
func TestRequeueAndDeferJob(t *testing.T) {
	tests := []struct {
		name         string
		requeue      func(s *Store) func(context.Context, Tx) error
		wantAttempts int
		wantCode     *string
	}{
		{
			name: "requeue keeps the attempt and the error",
			requeue: func(s *Store) func(context.Context, Tx) error {
				return func(ctx context.Context, tx Tx) error {
					return s.RequeueJob(ctx, tx, "j1", 7000, ptr("network"), ptr("connection reset"))
				}
			},
			wantAttempts: 2,
			wantCode:     ptr("network"),
		},
		{
			name: "defer hands the attempt back and clears the error",
			requeue: func(s *Store) func(context.Context, Tx) error {
				return func(ctx context.Context, tx Tx) error {
					return s.DeferJob(ctx, tx, "j1", 7000)
				}
			},
			wantAttempts: 1,
			wantCode:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			j := leasedJob("j1", model.JobLlamacppInstall, "ver-1", "boot-1")
			j.Attempts = 2
			j.ErrorCode = ptr("stale")
			j.ErrorMessage = ptr("from a previous attempt")
			mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, j) })

			mustWrite(t, s, tc.requeue(s))

			got, err := s.Job(ctx, s.RO, "j1")
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if got.State != model.JobQueued {
				t.Errorf("state = %s, want queued", got.State)
			}
			if got.RunAfter != 7000 {
				t.Errorf("run_after = %d, want 7000", got.RunAfter)
			}
			if got.Attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", got.Attempts, tc.wantAttempts)
			}
			if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
				t.Error("the lease survived a requeue")
			}
			switch {
			case tc.wantCode == nil && got.ErrorCode != nil:
				t.Errorf("error_code = %q, want NULL", *got.ErrorCode)
			case tc.wantCode != nil && (got.ErrorCode == nil || *got.ErrorCode != *tc.wantCode):
				t.Errorf("error_code = %v, want %q", got.ErrorCode, *tc.wantCode)
			}
		})
	}
}

// TestDeferJobNeverGoesNegative: a deferral of a job that has not been claimed
// yet must leave attempts at zero rather than at -1, which the column would
// happily hold and the backoff would then read.
func TestDeferJobNeverGoesNegative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobCacheScan, "scan-1"))
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.DeferJob(ctx, tx, "j1", 7000) })

	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", got.Attempts)
	}
}

// TestRetryJob is the Retry button: the three states a job can stop in without
// being finished with, and the budget and cancel flag that would otherwise make
// the retry a no-op the instant a worker claimed it.
func TestRetryJob(t *testing.T) {
	tests := []struct {
		name    string
		state   model.JobState
		wantOK  bool
		wantEnd model.JobState
	}{
		{"failed", model.JobFailed, true, model.JobQueued},
		{"canceled", model.JobCanceled, true, model.JobQueued},
		{"interrupted", model.JobInterrupted, true, model.JobQueued},
		{"succeeded", model.JobSucceeded, false, model.JobSucceeded},
		{"running", model.JobRunning, false, model.JobRunning},
		{"queued", model.JobQueued, false, model.JobQueued},
		{"paused", model.JobPaused, false, model.JobPaused},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()

			j := newJob("j1", model.JobLlamacppInstall, "ver-1")
			j.State = tc.state
			j.Attempts = 3
			j.MaxAttempts = 3
			j.CancelRequested = true
			j.ErrorCode = ptr("build_failed")
			j.ErrorMessage = ptr("ninja exited 1")
			j.FinishedAt = ptr(int64(1500))
			mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, j) })

			var ok bool
			mustWrite(t, s, func(ctx context.Context, tx Tx) error {
				var err error
				ok, err = s.RetryJob(ctx, tx, "j1", 9000)
				return err
			})
			if ok != tc.wantOK {
				t.Fatalf("RetryJob = %v, want %v", ok, tc.wantOK)
			}

			got, err := s.Job(ctx, s.RO, "j1")
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if got.State != tc.wantEnd {
				t.Errorf("state = %s, want %s", got.State, tc.wantEnd)
			}
			if !tc.wantOK {
				return
			}
			if got.MaxAttempts <= got.Attempts {
				t.Errorf("max_attempts = %d with attempts = %d: the retry cannot run",
					got.MaxAttempts, got.Attempts)
			}
			if got.CancelRequested {
				t.Error("cancel_requested survived the retry")
			}
			if got.ErrorCode != nil || got.ErrorMessage != nil || got.FinishedAt != nil {
				t.Error("the retried job still wears its previous ending")
			}
			if got.RunAfter != 9000 {
				t.Errorf("run_after = %d, want 9000", got.RunAfter)
			}
		})
	}
}

// TestExpireJobLeases is §9.4 step 5's shutdown step, and the assertion that
// matters is the one about what it does NOT do: a job that was running is still
// running, because resolving it is the next boot's decision.
func TestExpireJobLeases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mine := leasedJob("j-mine", model.JobModelDownload, "dl-1", "boot-1")
	leased := leasedJob("j-leased", model.JobCacheScan, "scan-1", "boot-1")
	leased.State = model.JobLeased
	theirs := leasedJob("j-theirs", model.JobBenchRun, "bench-1", "boot-2")
	queued := newJob("j-queued", model.JobModelVerify, "mod-1")

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, j := range []model.Job{mine, leased, theirs, queued} {
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		return nil
	})

	var n int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		n, err = s.ExpireJobLeases(ctx, tx, "boot-1", 4242)
		return err
	})
	if n != 2 {
		t.Errorf("ExpireJobLeases = %d, want 2", n)
	}

	for _, tc := range []struct {
		id        string
		wantState model.JobState
		wantExp   int64
	}{
		{"j-mine", model.JobRunning, 4242},
		{"j-leased", model.JobLeased, 4242},
		{"j-theirs", model.JobRunning, 2000},
	} {
		got, err := s.Job(ctx, s.RO, tc.id)
		if err != nil {
			t.Fatalf("Job(%s): %v", tc.id, err)
		}
		if got.State != tc.wantState {
			t.Errorf("%s state = %s, want %s", tc.id, got.State, tc.wantState)
		}
		if got.LeaseExpiresAt == nil || *got.LeaseExpiresAt != tc.wantExp {
			t.Errorf("%s lease_expires_at = %v, want %d", tc.id, got.LeaseExpiresAt, tc.wantExp)
		}
	}
}

// TestCountLiveJobsByKind is the guard §3.14 states over a KIND rather than over
// a subject: two installs on two different version ids are legal under
// idx_jobs_one_live_per_subject, so only a count can answer "is a build live",
// and `interrupted` counts.
func TestCountLiveJobsByKind(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	rows := []struct {
		id    string
		kind  model.JobKind
		subj  string
		state model.JobState
	}{
		{"j1", model.JobLlamacppInstall, "ver-1", model.JobQueued},
		{"j2", model.JobLlamacppInstall, "ver-2", model.JobRunning},
		{"j3", model.JobLlamacppInstall, "ver-3", model.JobInterrupted},
		{"j4", model.JobLlamacppInstall, "ver-4", model.JobSucceeded},
		{"j5", model.JobSelfUpdate, "su-1", model.JobPaused},
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, r := range rows {
			j := newJob(r.id, r.kind, r.subj)
			j.State = r.state
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		return nil
	})

	for _, tc := range []struct {
		kind model.JobKind
		want int
	}{
		{model.JobLlamacppInstall, 3},
		{model.JobSelfUpdate, 1},
		{model.JobBenchRun, 0},
	} {
		var n int
		err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
			var err error
			n, err = s.CountLiveJobsByKind(ctx, tx, tc.kind)
			return err
		})
		if err != nil {
			t.Fatalf("CountLiveJobsByKind(%s): %v", tc.kind, err)
		}
		if n != tc.want {
			t.Errorf("live %s jobs = %d, want %d", tc.kind, n, tc.want)
		}
	}
}
