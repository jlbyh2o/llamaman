package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// newJob builds a queued job for a distinct subject, so the one-live-job-per-
// subject index never interferes with a test that is about something else.
func newJob(id string, kind model.JobKind, subjectID string) model.Job {
	subjectType, sid := model.SubjectFor(kind, subjectID)
	return model.Job{
		ID: id, Kind: kind, SubjectType: subjectType, SubjectID: sid,
		State: model.JobQueued, Priority: 100, RunAfter: 1000,
		MaxAttempts: 3, CreatedAt: 1000,
	}
}

// TestJobRoundTrip proves every column survives the write/read boundary,
// nullable ones included — a pointer field silently dropped on the way out would
// make a whole aggregate look empty.
func TestJobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := newJob("j1", model.JobModelDownload, "dl-1")
	want.LeaseOwner = ptr("boot-1")
	want.LeaseExpiresAt = ptr(int64(2000))
	want.CancelRequested = true
	want.IdempotencyKey = ptr("idem-1")
	want.ProgressJSON = ptr(`{"pct":42}`)
	want.ParamsJSON = ptr(`{"repo":"org/model"}`)
	want.ErrorCode = ptr("network")
	want.ErrorMessage = ptr("connection reset")
	want.StartedAt = ptr(int64(1100))
	want.FinishedAt = ptr(int64(1200))
	want.Attempts = 2
	want.State = model.JobRunning

	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, want) })

	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("job mismatch (-want +got):\n%s", diff)
	}

	if _, err := s.Job(ctx, s.RO, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Job on a missing id = %v, want ErrNotFound", err)
	}
}

// TestLeaseNextJobOrdering proves the queue's ordering rule — lower priority
// first, then the job ready longest — and that a job whose run_after is in the
// future is not claimed, which is what makes backoff work at all.
func TestLeaseNextJobOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	low := newJob("j-low", model.JobModelDownload, "dl-1")
	low.Priority = 50
	low.RunAfter = 900

	normal := newJob("j-normal", model.JobCacheScan, "scan-1")
	normal.Priority = 100
	normal.RunAfter = 100

	future := newJob("j-future", model.JobBenchRun, "bench-1")
	future.Priority = 1
	future.RunAfter = 9999

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, j := range []model.Job{normal, future, low} {
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		return nil
	})

	var order []string
	for range 2 {
		var leased model.Job
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			var err error
			leased, err = s.LeaseNextJob(ctx, tx, LeaseParams{
				Owner: "boot-1", Now: 1000, LeaseExpiresAt: 1030,
			})
			return err
		})
		order = append(order, leased.ID)
	}
	if diff := cmp.Diff([]string{"j-low", "j-normal"}, order); diff != "" {
		t.Errorf("lease order mismatch (-want +got):\n%s", diff)
	}

	// The future job is the highest priority of the three and is still not
	// claimed, because run_after has not arrived.
	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		_, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "boot-1", Now: 1000, LeaseExpiresAt: 1030})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("leasing with nothing ready = %v, want ErrNotFound", err)
	}
}

// TestLeaseNextJobStampsLeaseAndCountsTheAttempt: the lease is the moment work
// is claimed, so it is the moment `attempts` moves — the backoff counts attempts
// made, not jobs created.
func TestLeaseNextJobStampsLeaseAndCountsTheAttempt(t *testing.T) {
	s := newTestStore(t)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1"))
	})

	var leased model.Job
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		leased, err = s.LeaseNextJob(ctx, tx, LeaseParams{
			Owner: "boot-1", Now: 2000, LeaseExpiresAt: 2030,
		})
		return err
	})

	if leased.State != model.JobLeased {
		t.Errorf("state = %q, want leased", leased.State)
	}
	if leased.LeaseOwner == nil || *leased.LeaseOwner != "boot-1" {
		t.Errorf("lease owner = %v, want boot-1", leased.LeaseOwner)
	}
	if leased.LeaseExpiresAt == nil || *leased.LeaseExpiresAt != 2030 {
		t.Errorf("lease expiry = %v, want 2030", leased.LeaseExpiresAt)
	}
	if leased.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", leased.Attempts)
	}
}

// TestLeaseNextJobHonorsKinds: a worker registry claims only the kinds it can
// run, so a host with no bench worker must not strand a bench job in `leased`.
func TestLeaseNextJobHonorsKinds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobBenchRun, "bench-1"))
	})

	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		_, err := s.LeaseNextJob(ctx, tx, LeaseParams{
			Owner: "boot-1", Now: 2000, LeaseExpiresAt: 2030,
			Kinds: []model.JobKind{model.JobModelDownload},
		})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("leasing a kind we did not ask for = %v, want ErrNotFound", err)
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		got, err := s.LeaseNextJob(ctx, tx, LeaseParams{
			Owner: "boot-1", Now: 2000, LeaseExpiresAt: 2030,
			Kinds: []model.JobKind{model.JobModelDownload, model.JobBenchRun},
		})
		if err != nil {
			return err
		}
		if got.ID != "j1" {
			t.Errorf("leased %q, want j1", got.ID)
		}
		return nil
	})
}

// TestHeartbeatOnlyForTheHolder: a worker whose lease was taken over must learn
// it from the heartbeat rather than keep writing to somebody else's job.
func TestHeartbeatOnlyForTheHolder(t *testing.T) {
	s := newTestStore(t)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1"))
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "boot-1", Now: 2000, LeaseExpiresAt: 2030})
		return err
	})

	tests := []struct {
		name  string
		id    string
		owner string
		want  bool
	}{
		{name: "the holder extends it", id: "j1", owner: "boot-1", want: true},
		{name: "another boot cannot", id: "j1", owner: "boot-2", want: false},
		{name: "a missing job cannot", id: "nope", owner: "boot-1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ok bool
			mustWrite(t, s, func(ctx context.Context, tx Tx) error {
				var err error
				ok, err = s.HeartbeatJob(ctx, tx, tt.id, tt.owner, 3000)
				return err
			})
			if ok != tt.want {
				t.Errorf("HeartbeatJob = %v, want %v", ok, tt.want)
			}
		})
	}
}

// TestStartJobStampsStartedAtOnce: a retried job keeps the instant its first
// attempt began, so the history reads as one activity rather than several.
func TestStartJobStampsStartedAtOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1"))
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "b", Now: 2000, LeaseExpiresAt: 2030}); err != nil {
			return err
		}
		ok, err := s.StartJob(ctx, tx, "j1", "b", 2001)
		if err != nil {
			return err
		}
		if !ok {
			t.Error("StartJob on a leased job = false")
		}
		return nil
	})

	// Fail it, retry it, lease and start it again.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.FinishJob(ctx, tx, "j1", model.JobFailed, ptr("net"), ptr("reset"), 2100); err != nil {
			return err
		}
		return s.RescheduleJob(ctx, tx, "j1", 2200)
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "b", Now: 2300, LeaseExpiresAt: 2330}); err != nil {
			return err
		}
		_, err := s.StartJob(ctx, tx, "j1", "b", 2301)
		return err
	})

	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.StartedAt == nil || *got.StartedAt != 2001 {
		t.Errorf("started_at = %v, want the first attempt's 2001", got.StartedAt)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
}

// TestFinishJobReleasesTheLease and refuses a non-terminal state, which would
// otherwise leave a job that looks finished but still holds its subject.
func TestFinishJobReleasesTheLease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1")); err != nil {
			return err
		}
		_, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "b", Now: 2000, LeaseExpiresAt: 2030})
		return err
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.FinishJob(ctx, tx, "j1", model.JobSucceeded, nil, nil, 2500)
	})

	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.State != model.JobSucceeded {
		t.Errorf("state = %q, want succeeded", got.State)
	}
	if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Errorf("lease was not released: owner=%v expires=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
	if got.FinishedAt == nil || *got.FinishedAt != 2500 {
		t.Errorf("finished_at = %v, want 2500", got.FinishedAt)
	}

	err = s.Write(ctx, func(ctx context.Context, tx Tx) error {
		return s.FinishJob(ctx, tx, "j1", model.JobRunning, nil, nil, 2600)
	})
	if err == nil {
		t.Error("FinishJob accepted a non-terminal state")
	}
}

// TestSetJobStatePauseAndResume is the pause half of §2.3a: the pause releases
// the lease but the row stays live, so nothing else can claim the subject.
func TestSetJobStatePauseAndResume(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1")); err != nil {
			return err
		}
		_, err := s.LeaseNextJob(ctx, tx, LeaseParams{Owner: "b", Now: 2000, LeaseExpiresAt: 2030})
		return err
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.SetJobState(ctx, tx, "j1", model.JobPaused)
	})
	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.State != model.JobPaused {
		t.Errorf("state = %q, want paused", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("a paused job still holds a lease: %v", got.LeaseOwner)
	}
	if !got.State.IsLive() {
		t.Error("a paused job must still hold its subject")
	}

	if _, err := s.LiveJobForSubject(ctx, s.RO, model.SubjectDownload, "dl-1"); err != nil {
		t.Errorf("LiveJobForSubject on a paused job: %v", err)
	}

	err = s.Write(ctx, func(ctx context.Context, tx Tx) error {
		return s.SetJobState(ctx, tx, "j1", model.JobSucceeded)
	})
	if err == nil {
		t.Error("SetJobState accepted a terminal state")
	}
}

// TestRequestJobCancel raises the flag on a live job and refuses one that is
// already over, where a cancel has nothing to act on.
func TestRequestJobCancel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.InsertJob(ctx, tx, newJob("j1", model.JobModelDownload, "dl-1"))
	})

	var ok bool
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		ok, err = s.RequestJobCancel(ctx, tx, "j1")
		return err
	})
	if !ok {
		t.Fatal("RequestJobCancel on a queued job = false")
	}
	got, err := s.Job(ctx, s.RO, "j1")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel_requested was not raised")
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		return s.FinishJob(ctx, tx, "j1", model.JobCanceled, nil, nil, 3000)
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		var err error
		ok, err = s.RequestJobCancel(ctx, tx, "j1")
		return err
	})
	if ok {
		t.Error("RequestJobCancel on a terminal job = true")
	}
}

// TestBootTriage is section 2.3's three-outcome table proven against a real
// database: one job of every kind, all leased by a boot that is now gone.
func TestBootTriage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Every kind, each on its own subject so all eleven can be live at once.
	subjects := map[model.JobKind]string{
		model.JobLlamacppInstall:  "ver-1",
		model.JobLlamacppActivate: "ver-2",
		model.JobLlamacppDelete:   "ver-3",
		model.JobModelDownload:    "dl-1",
		model.JobModelVerify:      "model-1",
		model.JobModelDelete:      "model-2",
		model.JobCacheScan:        "scan-1",
		model.JobBenchRun:         "bench-1",
		model.JobSelfUpdate:       "update-1",
		model.JobToolchainProbe:   "",
		model.JobMaintenance:      "",
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for _, kind := range model.JobKindValues() {
			j := newJob("job-"+string(kind), kind, subjects[kind])
			j.State = model.JobRunning
			j.LeaseOwner = ptr("boot-previous")
			j.LeaseExpiresAt = ptr(int64(5000))
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		// A paused job and a job leased by THIS boot, neither of which triage
		// may touch.
		paused := newJob("job-paused", model.JobModelDownload, "dl-2")
		paused.State = model.JobPaused
		if err := s.InsertJob(ctx, tx, paused); err != nil {
			return err
		}
		mine := newJob("job-mine", model.JobCacheScan, "scan-2")
		mine.State = model.JobRunning
		mine.LeaseOwner = ptr("boot-current")
		return s.InsertJob(ctx, tx, mine)
	})

	orphans, err := s.OrphanedJobs(ctx, s.RO, "boot-current")
	if err != nil {
		t.Fatalf("OrphanedJobs: %v", err)
	}
	if len(orphans) != len(model.JobKindValues()) {
		t.Fatalf("OrphanedJobs returned %d rows, want %d — a paused job or this "+
			"boot's own lease was swept up", len(orphans), len(model.JobKindValues()))
	}

	want := map[model.JobKind]model.JobState{
		model.JobModelDownload:    model.JobQueued,
		model.JobCacheScan:        model.JobQueued,
		model.JobToolchainProbe:   model.JobQueued,
		model.JobLlamacppInstall:  model.JobInterrupted,
		model.JobLlamacppActivate: model.JobInterrupted,
		model.JobBenchRun:         model.JobInterrupted,
		model.JobSelfUpdate:       model.JobInterrupted,
		model.JobLlamacppDelete:   model.JobFailed,
		model.JobModelVerify:      model.JobFailed,
		model.JobModelDelete:      model.JobFailed,
		model.JobMaintenance:      model.JobFailed,
	}

	for _, orphan := range orphans {
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			got, err := s.TriageOrphanedJob(ctx, tx, orphan, 6000)
			if err != nil {
				return err
			}
			if got != want[orphan.Kind] {
				t.Errorf("kind %q triaged to %q, want %q", orphan.Kind, got, want[orphan.Kind])
			}
			return nil
		})
	}

	t.Run("interrupted rows stay live", func(t *testing.T) {
		j, err := s.Job(ctx, s.RO, "job-"+string(model.JobLlamacppInstall))
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if !j.State.IsLive() {
			t.Errorf("state %q does not hold the subject; a retry could start a second build", j.State)
		}
		if j.LeaseOwner != nil {
			t.Errorf("an interrupted job still holds a lease: %v", j.LeaseOwner)
		}
	})

	t.Run("failed rows carry daemon_restarted", func(t *testing.T) {
		j, err := s.Job(ctx, s.RO, "job-"+string(model.JobMaintenance))
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if j.ErrorCode == nil || *j.ErrorCode != string(model.CodeDaemonRestarted) {
			t.Errorf("error_code = %v, want %q", j.ErrorCode, model.CodeDaemonRestarted)
		}
	})

	t.Run("requeued rows do not spend an attempt", func(t *testing.T) {
		j, err := s.Job(ctx, s.RO, "job-"+string(model.JobModelDownload))
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if j.State != model.JobQueued {
			t.Fatalf("state = %q, want queued", j.State)
		}
		if j.Attempts != 0 {
			t.Errorf("attempts = %d, want 0: the daemon went away, nothing was attempted", j.Attempts)
		}
	})

	t.Run("the paused job is untouched", func(t *testing.T) {
		j, err := s.Job(ctx, s.RO, "job-paused")
		if err != nil {
			t.Fatalf("Job: %v", err)
		}
		if j.State != model.JobPaused {
			t.Errorf("state = %q, want paused: a pause is a user decision that survives a restart", j.State)
		}
	})
}

// TestJobsFilter covers the listing GET /api/v1/jobs is built on.
func TestJobsFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		a := newJob("job-a", model.JobModelDownload, "dl-1")
		b := newJob("job-b", model.JobCacheScan, "scan-1")
		b.State = model.JobSucceeded
		c := newJob("job-c", model.JobBenchRun, "bench-1")
		c.State = model.JobRunning
		for _, j := range []model.Job{a, b, c} {
			if err := s.InsertJob(ctx, tx, j); err != nil {
				return err
			}
		}
		return nil
	})

	tests := []struct {
		name   string
		filter JobFilter
		want   []string
	}{
		{name: "everything, newest first", filter: JobFilter{}, want: []string{"job-c", "job-b", "job-a"}},
		{
			name:   "active only",
			filter: JobFilter{States: model.LiveJobStates()},
			want:   []string{"job-c", "job-a"},
		},
		{
			name:   "by kind",
			filter: JobFilter{Kinds: []model.JobKind{model.JobCacheScan}},
			want:   []string{"job-b"},
		},
		{
			name:   "by subject",
			filter: JobFilter{SubjectType: model.SubjectDownload, SubjectID: "dl-1"},
			want:   []string{"job-a"},
		},
		{name: "limited", filter: JobFilter{Limit: 1}, want: []string{"job-c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Jobs(ctx, s.RO, tt.filter)
			if err != nil {
				t.Fatalf("Jobs: %v", err)
			}
			ids := make([]string, len(got))
			for i, j := range got {
				ids[i] = j.ID
			}
			if diff := cmp.Diff(tt.want, ids); diff != "" {
				t.Errorf("ids mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
