package model

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestJobStateLiveness pins the partition the one-live-job-per-subject index
// depends on. Every state is either live or terminal and never both, and the
// two that are easiest to get wrong are `paused` and `interrupted`: both are
// LIVE, which is what stops a duplicate job starting while a pause or an
// unresolved finalizer stands.
func TestJobStateLiveness(t *testing.T) {
	tests := []struct {
		state        JobState
		wantLive     bool
		wantTerminal bool
	}{
		{JobQueued, true, false},
		{JobLeased, true, false},
		{JobRunning, true, false},
		{JobPaused, true, false},
		{JobInterrupted, true, false},
		{JobSucceeded, false, true},
		{JobFailed, false, true},
		{JobCanceled, false, true},
	}

	if len(tests) != len(JobStateValues()) {
		t.Fatalf("the table covers %d of %d states", len(tests), len(JobStateValues()))
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsLive(); got != tt.wantLive {
				t.Errorf("IsLive = %v, want %v", got, tt.wantLive)
			}
			if got := tt.state.IsTerminal(); got != tt.wantTerminal {
				t.Errorf("IsTerminal = %v, want %v", got, tt.wantTerminal)
			}
			if tt.state.IsLive() && tt.state.IsTerminal() {
				t.Error("a state cannot be both live and terminal")
			}
		})
	}

	if diff := cmp.Diff(
		[]JobState{JobQueued, JobLeased, JobRunning, JobPaused, JobInterrupted},
		LiveJobStates(),
	); diff != "" {
		t.Errorf("LiveJobStates mismatch (-want +got):\n%s", diff)
	}
}

// TestJobBootTriage is section 2.3's three-outcome table, kind by kind. Each
// case carries the reason, because the reasons are the design: an `interrupted`
// row exists so a domain finalizer still has its input, and putting a kind in
// the wrong bucket is how bench-stopped production instances would be left down.
func TestJobBootTriage(t *testing.T) {
	tests := []struct {
		kind JobKind
		want JobState
		why  string
	}{
		{JobModelDownload, JobQueued, "idempotent and resumable; re-run from the top"},
		{JobCacheScan, JobQueued, "idempotent and resumable"},
		{JobToolchainProbe, JobQueued, "idempotent and resumable"},
		{JobLlamacppInstall, JobInterrupted, "D4: the build directory is warm and Retry reuses it"},
		{JobLlamacppActivate, JobInterrupted, "§6.6's boot reconciliation is the finalizer"},
		{JobBenchRun, JobInterrupted, "§10 restores stopped production instances from running+restore_done=0"},
		{JobSelfUpdate, JobInterrupted, "§12.3's confirmation gate is the finalizer"},
		{JobLlamacppDelete, JobFailed, "nothing durable is owed the row does not already describe"},
		{JobModelVerify, JobFailed, "nothing durable is owed"},
		{JobModelDelete, JobFailed, "nothing durable is owed"},
		{JobMaintenance, JobFailed, "the job row IS the record"},
	}

	if len(tests) != len(JobKindValues()) {
		t.Fatalf("the table covers %d of %d kinds", len(tests), len(JobKindValues()))
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := JobBootTriage(tt.kind); got != tt.want {
				t.Errorf("JobBootTriage = %q, want %q — %s", got, tt.want, tt.why)
			}
		})
	}

	if got := JobBootTriage("not_a_kind"); got != "" {
		t.Errorf("JobBootTriage on an unknown kind = %q, want the empty state", got)
	}

	// Every kind has an outcome, and no outcome is a state that would leave the
	// subject in an impossible place.
	for _, kind := range JobKindValues() {
		out := JobBootTriage(kind)
		if out == "" {
			t.Errorf("kind %q has no boot outcome", kind)
		}
		if !out.Valid() {
			t.Errorf("kind %q triages to %q, which is not a job state", kind, out)
		}
	}
}

// TestSubjectFor is §2.3a's mapping table, including the two fixed synthetic
// ids: they exist so the unique index still means "at most one toolchain probe
// and at most one maintenance pass may be live at a time".
func TestSubjectFor(t *testing.T) {
	tests := []struct {
		kind        JobKind
		domainID    string
		wantType    JobSubjectType
		wantSubject string
	}{
		{JobLlamacppInstall, "b10621-cuda-src", SubjectLlamacppVersion, "b10621-cuda-src"},
		{JobLlamacppActivate, "b10621-cuda-src", SubjectLlamacppVersion, "b10621-cuda-src"},
		{JobLlamacppDelete, "b10621-cuda-src", SubjectLlamacppVersion, "b10621-cuda-src"},
		{JobModelDownload, "dl-1", SubjectDownload, "dl-1"},
		{JobModelVerify, "model-1", SubjectModel, "model-1"},
		{JobModelDelete, "model-1", SubjectModel, "model-1"},
		{JobCacheScan, "scan-1", SubjectCacheScan, "scan-1"},
		{JobBenchRun, "run-1", SubjectBenchRun, "run-1"},
		{JobSelfUpdate, "update-1", SubjectSelfUpdate, "update-1"},
		{JobToolchainProbe, "ignored", SubjectSystem, SubjectIDToolchain},
		{JobMaintenance, "ignored", SubjectSystem, SubjectIDMaintenance},
	}

	if len(tests) != len(JobKindValues()) {
		t.Fatalf("the table covers %d of %d kinds", len(tests), len(JobKindValues()))
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			gotType, gotSubject := SubjectFor(tt.kind, tt.domainID)
			if gotType != tt.wantType || gotSubject != tt.wantSubject {
				t.Errorf("SubjectFor = (%q, %q), want (%q, %q)",
					gotType, gotSubject, tt.wantType, tt.wantSubject)
			}
			if !gotType.Valid() {
				t.Errorf("subject type %q is not a member of the CHECK constraint", gotType)
			}
		})
	}

	if gotType, gotSubject := SubjectFor("not_a_kind", "x"); gotType != "" || gotSubject != "" {
		t.Errorf("SubjectFor on an unknown kind = (%q, %q), want empties", gotType, gotSubject)
	}
}

// TestJobBackoff is §2.3's retry delay: `min(60s, 2^attempts × 2s)`. The cap
// matters as much as the curve — without it the eighth attempt would be an hour
// out and a transient network failure would look like a stuck download.
func TestJobBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{-1, 2 * time.Second},
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 32 * time.Second},
		{5, 60 * time.Second},
		{6, 60 * time.Second},
		{100, 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run("attempts="+strconv.Itoa(tt.attempts), func(t *testing.T) {
			if got := JobBackoff(tt.attempts); got != tt.want {
				t.Errorf("JobBackoff(%d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}

// TestSelfUpdateCancelCutoff is D96: a cancel is accepted only before the
// `staged` commit, because from that instant the marker is on disk and the swap
// is a unit systemd owns.
func TestSelfUpdateCancelCutoff(t *testing.T) {
	tests := []struct {
		state        SelfUpdateState
		wantCancel   bool
		wantTerminal bool
	}{
		{UpdatePlanned, true, false},
		{UpdateDownloading, true, false},
		{UpdateVerifying, true, false},
		{UpdateStaged, false, false},
		{UpdateSwapping, false, false},
		{UpdateSucceeded, false, true},
		{UpdateFailed, false, true},
		{UpdateCanceled, false, true},
	}

	if len(tests) != len(SelfUpdateStateValues()) {
		t.Fatalf("the table covers %d of %d states", len(tests), len(SelfUpdateStateValues()))
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.Cancelable(); got != tt.wantCancel {
				t.Errorf("Cancelable = %v, want %v", got, tt.wantCancel)
			}
			if got := tt.state.IsTerminal(); got != tt.wantTerminal {
				t.Errorf("IsTerminal = %v, want %v", got, tt.wantTerminal)
			}
		})
	}
}

// TestVersionTerminalFailure covers D71's reuse-and-reset entry condition: which
// states a POST or a Retry may bring back to `pending`.
func TestVersionTerminalFailure(t *testing.T) {
	tests := []struct {
		state VersionState
		want  bool
	}{
		{VersionPending, false},
		{VersionResolving, false},
		{VersionFetching, false},
		{VersionBuilding, false},
		{VersionVerifying, false},
		{VersionReady, false},
		{VersionFailed, true},
		{VersionFailedVerification, true},
		{VersionCanceled, true},
		{VersionDeleting, false},
		{VersionDeleted, true},
	}

	if len(tests) != len(VersionStateValues()) {
		t.Fatalf("the table covers %d of %d states", len(tests), len(VersionStateValues()))
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsTerminalFailure(); got != tt.want {
				t.Errorf("IsTerminalFailure = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSetupClaimClaimed keeps the one predicate `llamaman status` and §11.1
// step 8 both branch on honest.
func TestSetupClaimClaimed(t *testing.T) {
	var unclaimed SetupClaim
	if unclaimed.Claimed() {
		t.Error("a row with no claimed_at reads as claimed")
	}
	at := int64(1700)
	claimed := SetupClaim{ClaimedAt: &at}
	if !claimed.Claimed() {
		t.Error("a row with claimed_at does not read as claimed")
	}
}

// TestErrorEnvelope pins the wire shape every non-2xx response uses.
func TestErrorEnvelope(t *testing.T) {
	e := Error{Code: CodePortUnavailable, Message: "port 5526 is the management UI",
		Details: map[string]any{"port": 5526, "reason": string(PortReservedManagement)}}
	if got, want := e.Error(), "port_unavailable: port 5526 is the management UI"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var err error = e
	if err.Error() == "" {
		t.Error("model.Error does not satisfy error usefully")
	}
}
