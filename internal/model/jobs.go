package model

import "time"

// Jobs — the scheduling spine (DESIGN section 2.3 / 2.3a).
//
// A `jobs` row is the SCHEDULING record: lease, retry, backoff, cancellation,
// progress. The domain row it names through (SubjectType, SubjectID) is the
// DOMAIN record and holds the state the UI reads. There is exactly one live
// `jobs` row per domain row, both written in the same transaction by the same
// worker, which is what keeps them from drifting; `idx_jobs_one_live_per_subject`
// makes a second one impossible rather than merely discouraged (D39).

// JobKind is `jobs.kind` (§2.3).
type JobKind string

const (
	JobLlamacppInstall  JobKind = "llamacpp_install"
	JobLlamacppActivate JobKind = "llamacpp_activate"
	JobLlamacppDelete   JobKind = "llamacpp_delete"
	JobModelDownload    JobKind = "model_download"
	JobModelVerify      JobKind = "model_verify"
	JobModelDelete      JobKind = "model_delete"
	JobCacheScan        JobKind = "cache_scan"
	JobBenchRun         JobKind = "bench_run"
	JobSelfUpdate       JobKind = "self_update"
	JobToolchainProbe   JobKind = "toolchain_probe"
	JobMaintenance      JobKind = "maintenance"
)

// JobKindValues lists the members of the `jobs.kind` CHECK constraint, in order.
func JobKindValues() []JobKind {
	return []JobKind{
		JobLlamacppInstall, JobLlamacppActivate, JobLlamacppDelete,
		JobModelDownload, JobModelVerify, JobModelDelete, JobCacheScan,
		JobBenchRun, JobSelfUpdate, JobToolchainProbe, JobMaintenance,
	}
}

// Valid reports whether k is a member of the CHECK constraint.
func (k JobKind) Valid() bool { return valid(k, JobKindValues()) }

// JobSubjectType is `jobs.subject_type` (§2.3a).
type JobSubjectType string

const (
	SubjectLlamacppVersion JobSubjectType = "llamacpp_version"
	SubjectModel           JobSubjectType = "model"
	SubjectDownload        JobSubjectType = "download"
	SubjectCacheScan       JobSubjectType = "cache_scan"
	SubjectBenchRun        JobSubjectType = "bench_run"
	SubjectSelfUpdate      JobSubjectType = "self_update"
	SubjectSystem          JobSubjectType = "system"
)

// JobSubjectTypeValues lists the members of the `jobs.subject_type` CHECK
// constraint, in order.
func JobSubjectTypeValues() []JobSubjectType {
	return []JobSubjectType{
		SubjectLlamacppVersion, SubjectModel, SubjectDownload, SubjectCacheScan,
		SubjectBenchRun, SubjectSelfUpdate, SubjectSystem,
	}
}

// Valid reports whether t is a member of the CHECK constraint.
func (t JobSubjectType) Valid() bool { return valid(t, JobSubjectTypeValues()) }

// The two fixed synthetic subject ids of the `system` subject type. They exist
// precisely so `idx_jobs_one_live_per_subject` still means something for the two
// activities that have no domain row: at most one toolchain probe and at most
// one maintenance pass may be live at a time (§2.3a).
const (
	SubjectIDToolchain   = "toolchain"
	SubjectIDMaintenance = "maintenance"
)

// SubjectFor returns the (subject_type, subject_id) pair §2.3a's mapping table
// fixes for a kind, given the domain row's id. For the two `system` kinds the
// domain id is ignored and the fixed synthetic id is returned instead, because
// that is what makes the one-live-job-per-subject index bind them.
//
//	kind                                          subject_type       subject_id
//	llamacpp_install|_activate|_delete            llamacpp_version   llamacpp_versions.id
//	model_download                                download           downloads.id
//	model_verify|model_delete                     model              models.id
//	cache_scan                                    cache_scan         cache_scans.id
//	bench_run                                     bench_run          bench_runs.id
//	self_update                                   self_update        self_updates.id
//	toolchain_probe                               system             'toolchain'
//	maintenance                                   system             'maintenance'
func SubjectFor(kind JobKind, domainID string) (JobSubjectType, string) {
	switch kind {
	case JobLlamacppInstall, JobLlamacppActivate, JobLlamacppDelete:
		return SubjectLlamacppVersion, domainID
	case JobModelDownload:
		return SubjectDownload, domainID
	case JobModelVerify, JobModelDelete:
		return SubjectModel, domainID
	case JobCacheScan:
		return SubjectCacheScan, domainID
	case JobBenchRun:
		return SubjectBenchRun, domainID
	case JobSelfUpdate:
		return SubjectSelfUpdate, domainID
	case JobToolchainProbe:
		return SubjectSystem, SubjectIDToolchain
	case JobMaintenance:
		return SubjectSystem, SubjectIDMaintenance
	}
	return "", ""
}

// JobState is `jobs.state` (§2.3).
//
// Lifecycle:
//
//	queued → leased → running → succeeded|failed|canceled
//	running ⇄ paused                         (downloads only)
//	failed → queued while attempts < max_attempts, with the backoff of JobBackoff
//
// `paused` and `interrupted` both count as LIVE for
// `idx_jobs_one_live_per_subject`, so nothing else can start on the same subject
// until the finalizer, the user or a retry resolves it. A pause is a user
// decision that must survive a restart, which is why it is a `jobs` state rather
// than a downloads-only concept: without it a paused download would either hold
// a lease forever or free its subject for a duplicate job.
type JobState string

const (
	JobQueued      JobState = "queued"
	JobLeased      JobState = "leased"
	JobRunning     JobState = "running"
	JobPaused      JobState = "paused"
	JobInterrupted JobState = "interrupted"
	JobSucceeded   JobState = "succeeded"
	JobFailed      JobState = "failed"
	JobCanceled    JobState = "canceled"
)

// JobStateValues lists the members of the `jobs.state` CHECK constraint, in order.
func JobStateValues() []JobState {
	return []JobState{
		JobQueued, JobLeased, JobRunning, JobPaused, JobInterrupted,
		JobSucceeded, JobFailed, JobCanceled,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s JobState) Valid() bool { return valid(s, JobStateValues()) }

// LiveJobStates is the WHERE clause of `idx_jobs_one_live_per_subject`,
// expressed once so Go and the partial index cannot disagree about which states
// hold a subject.
func LiveJobStates() []JobState {
	return []JobState{JobQueued, JobLeased, JobRunning, JobPaused, JobInterrupted}
}

// IsLive reports whether a row in this state holds its subject against the
// one-live-job-per-subject index.
func (s JobState) IsLive() bool { return valid(s, LiveJobStates()) }

// IsTerminal reports whether a row in this state is finished. Terminal rows are
// the ones the nightly maintenance pass may prune after 90 days; a live or
// `interrupted` row is never pruned (§2.11 retention).
func (s JobState) IsTerminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCanceled
}

// JobBootTriage returns the state a row this kind must be moved to when boot
// finds it `leased`/`running` under a lease_owner that is not the current
// boot_id — the three-outcome table of §2.3:
//
//	outcome        kinds                                        why
//	-------------- -------------------------------------------- ------------------------------------
//	queued         model_download, cache_scan, toolchain_probe   idempotent and resumable; the domain
//	               (re-run from the top)                         row returns to `queued` with it
//	interrupted    llamacpp_install (D4: the build directory is  the activity left durable state
//	               warm and Retry reuses it), llamacpp_activate  outside the job row — object files, a
//	               (§6.6's boot reconciliation is the            symlink that may or may not have been
//	               finalizer), bench_run (the stop-and-restore   flipped, a half-rolled fleet, stopped
//	               finalizer, §10), self_update (§12.3's         production instances, a swapped
//	               confirmation gate)                            binary — that only its own subsystem
//	                                                             knows how to settle, and the DOMAIN
//	                                                             ROW KEEPS ITS STATE because that
//	                                                             state is the finalizer's input
//	failed         llamacpp_delete, model_verify, model_delete,  nothing durable is owed that the row
//	(error_code=   maintenance                                   does not already describe; the domain
//	 daemon_       row is resolved in the same transaction
//	 restarted)
//
// `paused` rows never reach this function: a pause is a user decision that must
// survive a restart, so boot leaves them alone.
//
// Why bench_run, self_update and llamacpp_activate are NOT in the third bucket:
// marking the job `failed` also has to mark the domain row `failed` (§2.3a), and
// that destroys the input of the recovery that follows. §10 restores
// bench-stopped production instances from a run row left `running` with
// restore_done=0 — a condition a `failed` row can never satisfy. §11.1 step 11
// marks the `self_updates` row `succeeded` on a confirmed update, which would
// contradict a job the same boot had just marked `failed`. And an activation
// commits its is_active/previous_active/config_hash transaction BEFORE the
// symlink flip and the canary roll, so a restart can leave the row ahead of the
// filesystem — the exact state §6.6's boot reconciliation exists to repair, and
// the exact state a blanket `failed` would have overwritten with a lie.
func JobBootTriage(kind JobKind) JobState {
	switch kind {
	case JobModelDownload, JobCacheScan, JobToolchainProbe:
		return JobQueued
	case JobLlamacppInstall, JobLlamacppActivate, JobBenchRun, JobSelfUpdate:
		return JobInterrupted
	case JobLlamacppDelete, JobModelVerify, JobModelDelete, JobMaintenance:
		return JobFailed
	}
	return ""
}

// JobBackoff is the retry delay of §2.3: `run_after = now + min(60s, 2^attempts × 2s)`.
// It is a pure function of the attempt count; the caller supplies `now`, because
// nothing in this package reads a clock.
func JobBackoff(attempts int) time.Duration {
	const ceiling = 60 * time.Second
	if attempts < 0 {
		attempts = 0
	}
	// 2^attempts overflows fast and the ceiling is 60s, so stop multiplying early.
	d := 2 * time.Second
	for range attempts {
		d *= 2
		if d >= ceiling {
			return ceiling
		}
	}
	return min(d, ceiling)
}

// Job is one row of `jobs` (§2.3). Pointer fields are the nullable columns:
// NULL is a distinct fact in every one of them — no lease held, never started,
// no error recorded — and collapsing it into a zero value would lose it.
type Job struct {
	ID              string
	Kind            JobKind
	SubjectType     JobSubjectType
	SubjectID       string
	State           JobState
	Priority        int
	RunAfter        int64
	Attempts        int
	MaxAttempts     int
	LeaseOwner      *string
	LeaseExpiresAt  *int64
	CancelRequested bool
	IdempotencyKey  *string
	ProgressJSON    *string
	ParamsJSON      *string
	ErrorCode       *string
	ErrorMessage    *string
	CreatedAt       int64
	StartedAt       *int64
	FinishedAt      *int64
}

// IdempotencyKey is one row of `idempotency_keys` (§2.3, D65): the 10-minute
// replay window over a job-creating request. It is deliberately not a unique
// index on `jobs.idempotency_key`, which is permanent and global and so cannot
// express a window.
type IdempotencyKey struct {
	Key                string
	Route              string // method + pattern; the same key on a different route is a 422
	RequestFingerprint string // sha256 of the canonicalized request body
	JobID              string
	CreatedAt          int64
	ExpiresAt          int64 // created_at + IdempotencyWindow
}

// IdempotencyWindow is the replay window of D65: `expires_at = created_at + 600_000 ms`.
const IdempotencyWindow = 10 * time.Minute
