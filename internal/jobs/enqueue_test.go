package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// TestEnqueueSubjectMapping pins §2.3a's closed mapping at the enqueue boundary,
// including the part that is easy to get wrong: the two `system` kinds ignore
// the domain id they are handed and take the fixed synthetic id instead, which
// is what makes idx_jobs_one_live_per_subject bind them at all.
func TestEnqueueSubjectMapping(t *testing.T) {
	tests := []struct {
		name            string
		kind            model.JobKind
		domainID        string
		wantSubjectType model.JobSubjectType
		wantSubjectID   string
	}{
		{"install", model.JobLlamacppInstall, "ver-1", model.SubjectLlamacppVersion, "ver-1"},
		{"activate", model.JobLlamacppActivate, "ver-2", model.SubjectLlamacppVersion, "ver-2"},
		{"delete", model.JobLlamacppDelete, "ver-3", model.SubjectLlamacppVersion, "ver-3"},
		{"download", model.JobModelDownload, "dl-1", model.SubjectDownload, "dl-1"},
		{"verify", model.JobModelVerify, "mod-1", model.SubjectModel, "mod-1"},
		{"model delete", model.JobModelDelete, "mod-2", model.SubjectModel, "mod-2"},
		{"cache scan", model.JobCacheScan, "scan-1", model.SubjectCacheScan, "scan-1"},
		{"bench", model.JobBenchRun, "bench-1", model.SubjectBenchRun, "bench-1"},
		{"self update", model.JobSelfUpdate, "su-1", model.SubjectSelfUpdate, "su-1"},
		{"toolchain probe", model.JobToolchainProbe, "ignored", model.SubjectSystem, model.SubjectIDToolchain},
		{"maintenance", model.JobMaintenance, "ignored", model.SubjectSystem, model.SubjectIDMaintenance},
	}

	q, _, _ := newTestQueue(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := mustEnqueue(t, q, EnqueueParams{Kind: tc.kind, DomainID: tc.domainID})
			if j.SubjectType != tc.wantSubjectType || j.SubjectID != tc.wantSubjectID {
				t.Errorf("subject = (%s, %s), want (%s, %s)",
					j.SubjectType, j.SubjectID, tc.wantSubjectType, tc.wantSubjectID)
			}
			if j.State != model.JobQueued {
				t.Errorf("state = %s, want queued", j.State)
			}
		})
	}
}

// TestEnqueueOneLiveJobPerSubject is D39's guarantee: a second live job on one
// subject is impossible, and the refusal NAMES the row it collided with. The
// table runs every kind, because the two `system` kinds reach the same subject
// from two different domain ids and would slip past a per-kind check.
func TestEnqueueOneLiveJobPerSubject(t *testing.T) {
	tests := []struct {
		name           string
		kind           model.JobKind
		firstDomainID  string
		secondDomainID string
	}{
		{"install", model.JobLlamacppInstall, "ver-1", "ver-1"},
		{"download", model.JobModelDownload, "dl-1", "dl-1"},
		{"verify", model.JobModelVerify, "mod-1", "mod-1"},
		{"cache scan", model.JobCacheScan, "scan-1", "scan-1"},
		{"bench", model.JobBenchRun, "bench-1", "bench-1"},
		{"self update", model.JobSelfUpdate, "su-1", "su-1"},
		// Two probes minted from two different domain ids still land on the one
		// synthetic subject `toolchain`, which is exactly why that constant exists.
		{"toolchain probe", model.JobToolchainProbe, "probe-a", "probe-b"},
		{"maintenance", model.JobMaintenance, "pass-a", "pass-b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, _, _ := newTestQueue(t)
			first := mustEnqueue(t, q, EnqueueParams{Kind: tc.kind, DomainID: tc.firstDomainID})

			_, err := q.Enqueue(context.Background(),
				EnqueueParams{Kind: tc.kind, DomainID: tc.secondDomainID})
			if err == nil {
				t.Fatal("a second live job on one subject was accepted")
			}
			me := asModelError(t, err)
			if me.Code != model.CodeJobInFlight {
				t.Errorf("code = %s, want %s", me.Code, model.CodeJobInFlight)
			}
			if got := me.Details["job_id"]; got != first.ID {
				t.Errorf("details.job_id = %v, want the colliding job %s", got, first.ID)
			}
		})
	}
}

// TestEnqueueLiveStatesHoldTheSubject proves the refusal covers every state
// LiveJobStates names — `paused` and `interrupted` included, which is the whole
// reason they are job states: a paused download that freed its subject would let
// a duplicate job start on it.
func TestEnqueueLiveStatesHoldTheSubject(t *testing.T) {
	for _, state := range model.LiveJobStates() {
		t.Run(string(state), func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			insertOrphan(t, s, clock, "j-held", model.JobModelDownload, "dl-1", state, testBootID)

			_, err := q.Enqueue(context.Background(),
				EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"})
			if err == nil {
				t.Fatalf("a %s job did not hold its subject", state)
			}
			if code := asModelError(t, err).Code; code != model.CodeJobInFlight {
				t.Errorf("code = %s, want %s", code, model.CodeJobInFlight)
			}
		})
	}
}

// TestEnqueueTerminalStatesFreeTheSubject is the other half: once the activity
// is over, the same subject may be worked again. Without it the first download
// of a file would be its last.
func TestEnqueueTerminalStatesFreeTheSubject(t *testing.T) {
	for _, state := range []model.JobState{model.JobSucceeded, model.JobFailed, model.JobCanceled} {
		t.Run(string(state), func(t *testing.T) {
			q, s, clock := newTestQueue(t)
			first := mustEnqueue(t, q,
				EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"})

			at := clock.now().UnixMilli()
			err := s.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return s.FinishJob(ctx, tx, first.ID, state, nil, nil, at)
			})
			if err != nil {
				t.Fatalf("finish first job: %v", err)
			}

			second := mustEnqueue(t, q,
				EnqueueParams{Kind: model.JobModelDownload, DomainID: "dl-1"})
			if second.ID == first.ID {
				t.Fatal("the second enqueue returned the first job")
			}
		})
	}
}

// TestEnqueueDomainWriteCommitsWithTheJob is §2.3a's transaction rule from both
// sides: the domain write sees the job row it is paired with, and a domain write
// that fails takes the job row down with it — a job with no domain row is the
// drift the rule exists to prevent.
func TestEnqueueDomainWriteCommitsWithTheJob(t *testing.T) {
	q, _, _ := newTestQueue(t)
	ctx := context.Background()

	var seen model.Job
	j := mustEnqueue(t, q, EnqueueParams{
		Kind: model.JobCacheScan, DomainID: "scan-1",
		Domain: func(ctx context.Context, tx store.Tx, j model.Job) error {
			seen = j
			return nil
		},
	})
	if seen.ID != j.ID {
		t.Errorf("the domain write saw job %q, want %q", seen.ID, j.ID)
	}

	boom := errors.New("the domain row could not be written")
	_, err := q.Enqueue(ctx, EnqueueParams{
		Kind: model.JobBenchRun, DomainID: "bench-1",
		Domain: func(ctx context.Context, tx store.Tx, j model.Job) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Enqueue error = %v, want the domain error", err)
	}
	got, err := q.List(ctx, store.JobFilter{Kinds: []model.JobKind{model.JobBenchRun}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a failed domain write left %d job rows behind", len(got))
	}
}

// TestEnqueueIdempotentReplay is D65's window, end to end: inside it the same
// key returns the ORIGINAL job rather than a 409, which is what makes a
// double-clicked Build a replay instead of an error.
func TestEnqueueIdempotentReplay(t *testing.T) {
	q, _, _ := newTestQueue(t)
	ctx := context.Background()

	idem := &Idempotency{Key: "k-1", Route: "POST /api/v1/llamacpp/versions", RequestFingerprint: "fp-1"}
	first, err := q.Enqueue(ctx, EnqueueParams{
		Kind: model.JobLlamacppInstall, DomainID: "ver-1", Idempotency: idem,
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if first.Replayed {
		t.Error("the first enqueue reported a replay")
	}
	if first.Job.IdempotencyKey == nil || *first.Job.IdempotencyKey != idem.Key {
		t.Errorf("jobs.idempotency_key = %v, want %q", first.Job.IdempotencyKey, idem.Key)
	}

	// The same request again, on a subject the first job still holds. Without the
	// window this is `409 job_in_flight`; with it, it is the same job and a 200.
	second, err := q.Enqueue(ctx, EnqueueParams{
		Kind: model.JobLlamacppInstall, DomainID: "ver-1", Idempotency: idem,
	})
	if err != nil {
		t.Fatalf("replay Enqueue: %v", err)
	}
	if !second.Replayed {
		t.Error("the replay was not reported as one")
	}
	if second.Job.ID != first.Job.ID {
		t.Errorf("replay returned job %s, want the original %s", second.Job.ID, first.Job.ID)
	}
}

// TestEnqueueIdempotencyMismatch covers the two hits that are a client bug
// rather than a replay: one key, two different requests.
func TestEnqueueIdempotencyMismatch(t *testing.T) {
	tests := []struct {
		name   string
		second Idempotency
	}{
		{"different fingerprint", Idempotency{Key: "k-1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-2"}},
		{"different route", Idempotency{Key: "k-1", Route: "POST /api/v1/benchmarks", RequestFingerprint: "fp-1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, _, _ := newTestQueue(t)
			ctx := context.Background()

			first, err := q.Enqueue(ctx, EnqueueParams{
				Kind: model.JobModelDownload, DomainID: "dl-1",
				Idempotency: &Idempotency{Key: "k-1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-1"},
			})
			if err != nil {
				t.Fatalf("first Enqueue: %v", err)
			}

			_, err = q.Enqueue(ctx, EnqueueParams{
				Kind: model.JobModelDownload, DomainID: "dl-2", Idempotency: &tc.second,
			})
			me := asModelError(t, err)
			if me.Code != model.CodeIdempotencyKeyReused {
				t.Fatalf("code = %s, want %s", me.Code, model.CodeIdempotencyKeyReused)
			}
			if got := me.Details["job_id"]; got != first.Job.ID {
				t.Errorf("details.job_id = %v, want %s", got, first.Job.ID)
			}
		})
	}
}

// TestEnqueueIdempotencyWindowExpires is why D65 replaced a unique index with a
// table: after the window the SAME key must be able to create a NEW job, or a
// client with one fixed key would collide forever.
func TestEnqueueIdempotencyWindowExpires(t *testing.T) {
	q, s, clock := newTestQueue(t)
	ctx := context.Background()

	idem := &Idempotency{Key: "k-1", Route: "POST /api/v1/cache/scans", RequestFingerprint: "fp-1"}
	first, err := q.Enqueue(ctx, EnqueueParams{
		Kind: model.JobCacheScan, DomainID: "scan-1", Idempotency: idem,
	})
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	// Free the subject, then step past the window.
	err = s.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.FinishJob(ctx, tx, first.Job.ID, model.JobSucceeded, nil, nil, clock.now().UnixMilli())
	})
	if err != nil {
		t.Fatalf("finish first job: %v", err)
	}
	clock.advance(model.IdempotencyWindow + time.Second)

	second, err := q.Enqueue(ctx, EnqueueParams{
		Kind: model.JobCacheScan, DomainID: "scan-2", Idempotency: idem,
	})
	if err != nil {
		t.Fatalf("Enqueue after the window: %v", err)
	}
	if second.Replayed {
		t.Error("an expired key replayed")
	}
	if second.Job.ID == first.Job.ID {
		t.Error("an expired key returned the original job")
	}
}

// TestEnqueueDefaults pins the three columns an enqueue may leave to the schema's
// own defaults, and the params round-trip.
func TestEnqueueDefaults(t *testing.T) {
	q, s, clock := newTestQueue(t)

	j := mustEnqueue(t, q, EnqueueParams{
		Kind: model.JobModelDownload, DomainID: "dl-1",
		Params: map[string]string{"repo_id": "org/model"},
	})
	got := jobRow(t, s, j.ID)

	if got.Priority != DefaultPriority {
		t.Errorf("priority = %d, want %d", got.Priority, DefaultPriority)
	}
	if got.MaxAttempts != 1 {
		t.Errorf("max_attempts = %d, want 1", got.MaxAttempts)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", got.Attempts)
	}
	if got.RunAfter != clock.now().UnixMilli() {
		t.Errorf("run_after = %d, want now (%d)", got.RunAfter, clock.now().UnixMilli())
	}
	if got.ParamsJSON == nil || *got.ParamsJSON != `{"repo_id":"org/model"}` {
		t.Errorf("params_json = %v, want the marshaled params", got.ParamsJSON)
	}

	// Nil params must stay SQL NULL rather than becoming the four bytes "null",
	// which would read back as a value.
	bare := mustEnqueue(t, q, EnqueueParams{Kind: model.JobMaintenance})
	if row := jobRow(t, s, bare.ID); row.ParamsJSON != nil {
		t.Errorf("params_json = %v, want NULL", *row.ParamsJSON)
	}
}

// TestEnqueueUnknownKind refuses a kind outside the CHECK constraint before the
// database has to, so the caller gets a sentence rather than a constraint error.
func TestEnqueueUnknownKind(t *testing.T) {
	q, _, _ := newTestQueue(t)
	if _, err := q.Enqueue(context.Background(),
		EnqueueParams{Kind: model.JobKind("not_a_kind"), DomainID: "x"}); !errors.Is(err, errUnknownKind) {
		t.Fatalf("Enqueue error = %v, want errUnknownKind", err)
	}
	if _, err := q.Enqueue(context.Background(),
		EnqueueParams{Kind: model.JobModelDownload}); err == nil {
		t.Fatal("a download job with no download id was accepted")
	}
}

// TestLiveCountByKind is the guard §3.14 states over a KIND: two installs on two
// different version ids are legal under the per-subject index, so only a count
// can answer "is a build live".
func TestLiveCountByKind(t *testing.T) {
	q, _, _ := newTestQueue(t)
	ctx := context.Background()

	mustEnqueue(t, q, EnqueueParams{Kind: model.JobLlamacppInstall, DomainID: "ver-1"})
	mustEnqueue(t, q, EnqueueParams{Kind: model.JobLlamacppInstall, DomainID: "ver-2"})

	n, err := q.LiveCountByKind(ctx, model.JobLlamacppInstall)
	if err != nil {
		t.Fatalf("LiveCountByKind: %v", err)
	}
	if n != 2 {
		t.Errorf("live installs = %d, want 2", n)
	}
	if n, err = q.LiveCountByKind(ctx, model.JobSelfUpdate); err != nil || n != 0 {
		t.Errorf("live self-updates = %d (err %v), want 0", n, err)
	}
}
