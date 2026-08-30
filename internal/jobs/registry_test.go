package jobs

import (
	"errors"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// TestRegistryRegister covers the two registrations that are composition bugs: a
// kind outside the CHECK constraint, and a second worker for a kind that already
// has one — which would mean two subsystems moving the same domain row, the
// drift §2.3a exists to prevent.
func TestRegistryRegister(t *testing.T) {
	tests := []struct {
		name    string
		worker  Worker
		wantErr bool
	}{
		{"a valid kind", &fakeWorker{kind: model.JobBenchRun}, false},
		{"a kind outside the CHECK constraint", &fakeWorker{kind: model.JobKind("not_a_kind")}, true},
		{"the empty kind", &fakeWorker{kind: model.JobKind("")}, true},
		{"a nil worker", nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			err := r.Register(tc.worker)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Register = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if r.Len() != 0 {
					t.Errorf("a refused registration left %d workers behind", r.Len())
				}
				return
			}
			if err := r.Register(tc.worker); err == nil {
				t.Error("a second worker for one kind was accepted")
			}
			if r.Len() != 1 {
				t.Errorf("Len = %d, want 1", r.Len())
			}
		})
	}
}

// TestRegistryKindsOrder pins the lease query's argument list to the order of the
// `jobs.kind` CHECK constraint, so it is stable across polls rather than
// reordered by map iteration.
func TestRegistryKindsOrder(t *testing.T) {
	r := NewRegistry()
	for _, kind := range []model.JobKind{
		model.JobMaintenance, model.JobCacheScan, model.JobLlamacppInstall,
	} {
		if err := r.Register(&fakeWorker{kind: kind}); err != nil {
			t.Fatalf("Register(%s): %v", kind, err)
		}
	}

	want := []model.JobKind{model.JobLlamacppInstall, model.JobCacheScan, model.JobMaintenance}
	for range 5 {
		got := r.Kinds()
		if len(got) != len(want) {
			t.Fatalf("Kinds = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("Kinds = %v, want %v", got, want)
			}
		}
	}

	if _, ok := r.Worker(model.JobCacheScan); !ok {
		t.Error("Worker(cache_scan) reported missing")
	}
	if _, ok := r.Worker(model.JobBenchRun); ok {
		t.Error("Worker(bench_run) reported present")
	}
}

// TestNewRequiresABootID: a queue that leased under the wrong owner would make
// the next boot's triage lie in both directions — abandoning this daemon's live
// work, and adopting a dead daemon's.
func TestNewRequiresABootID(t *testing.T) {
	s := newTestStore(t)

	if _, err := New(s, Options{}); err == nil {
		t.Error("New accepted an empty boot id")
	}
	if _, err := New(nil, Options{BootID: testBootID}); err == nil {
		t.Error("New accepted a nil store")
	}

	q, err := New(s, Options{BootID: testBootID})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if q.BootID() != testBootID {
		t.Errorf("BootID = %q, want %q", q.BootID(), testBootID)
	}
	if q.Registry() == nil {
		t.Error("Registry is nil")
	}
}

// TestNewDefaults pins the knobs §2.3 leaves to policy rather than to contract.
func TestNewDefaults(t *testing.T) {
	s := newTestStore(t)

	q, err := New(s, Options{BootID: testBootID})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"lease TTL", q.leaseTTL, DefaultLeaseTTL},
		{"heartbeat", q.heartbeatEvery, DefaultHeartbeatEvery},
		{"poll", q.pollEvery, DefaultPollEvery},
		{"concurrency", q.concurrency, DefaultConcurrency},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	custom, err := New(s, Options{
		BootID: testBootID, LeaseTTL: time.Second, HeartbeatEvery: 2 * time.Second,
		PollEvery: 3 * time.Second, Concurrency: 7,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if custom.leaseTTL != time.Second || custom.concurrency != 7 {
		t.Errorf("Options were not honored: leaseTTL=%s concurrency=%d",
			custom.leaseTTL, custom.concurrency)
	}
}

// TestDeferralUnwraps keeps the sentinel Defer travels as readable from both the
// Starter path and a Run that returns it.
func TestDeferralUnwraps(t *testing.T) {
	err := Defer(15 * time.Second)
	d, ok := asDeferral(err)
	if !ok || d != 15*time.Second {
		t.Fatalf("asDeferral = (%s, %v), want (15s, true)", d, ok)
	}
	if _, ok := asDeferral(errors.New("plain")); ok {
		t.Error("a plain error unwrapped as a deferral")
	}
	if err.Error() == "" {
		t.Error("a deferral has no message")
	}
}

// TestOutcomeConstructors pins the five endings a worker can name.
func TestOutcomeConstructors(t *testing.T) {
	tests := []struct {
		name          string
		out           Outcome
		wantState     model.JobState
		wantRetryable bool
	}{
		{"succeeded", Succeeded(nil), model.JobSucceeded, false},
		{"failed", Failed("bad_flags", "no", nil), model.JobFailed, false},
		{"retryable", RetryableFailure("network", "reset", nil), model.JobFailed, true},
		{"canceled", Canceled(nil), model.JobCanceled, false},
		{"deferred", Deferred(time.Minute), model.JobQueued, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.out.State != tc.wantState {
				t.Errorf("State = %s, want %s", tc.out.State, tc.wantState)
			}
			if tc.out.Retryable != tc.wantRetryable {
				t.Errorf("Retryable = %v, want %v", tc.out.Retryable, tc.wantRetryable)
			}
		})
	}
	if Deferred(time.Minute).After != time.Minute {
		t.Error("Deferred lost its delay")
	}
}
