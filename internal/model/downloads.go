package model

// Downloads (DESIGN section 2.7).
//
// There are THREE layers over one download and §2.3a fixes their relationship so
// they cannot drift: exactly one `jobs` row (subject_type='download') carries
// scheduling, `downloads.state` carries the domain state, and `download_tasks`
// carry per-file state and are folded upward. The download worker writes the job
// row and the domain row in the SAME transaction, and pause/resume moves both —
// which is why `paused` is a `jobs` state and not a downloads-only concept.

// DownloadState is `downloads.state` (§2.7). It is a STORED FOLD of its tasks:
// any running → running; all succeeded → verifying → succeeded; any failed past
// retries → failed. It is stored so list queries stay single-table, the fold
// function in internal/hf/download is the single writer, and a property test
// asserts the stored state always equals the fold of the task rows.
//
// The §2.3a pairing with the job row:
//
//	jobs.state          downloads.state
//	------------------- ---------------------------------
//	queued              queued
//	leased|running      resolving|running|verifying
//	paused              paused
//	interrupted         — downloads return to `queued` on boot, because a
//	                      download is idempotent and resumable
//	succeeded           succeeded
//	failed              failed
//	canceled            canceled
type DownloadState string

const (
	DownloadQueued    DownloadState = "queued"
	DownloadResolving DownloadState = "resolving"
	DownloadRunning   DownloadState = "running"
	DownloadPaused    DownloadState = "paused"
	DownloadVerifying DownloadState = "verifying"
	DownloadSucceeded DownloadState = "succeeded"
	DownloadFailed    DownloadState = "failed"
	DownloadCanceled  DownloadState = "canceled"
)

// DownloadStateValues lists the members of the `downloads.state` CHECK
// constraint, in order.
func DownloadStateValues() []DownloadState {
	return []DownloadState{
		DownloadQueued, DownloadResolving, DownloadRunning, DownloadPaused,
		DownloadVerifying, DownloadSucceeded, DownloadFailed, DownloadCanceled,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s DownloadState) Valid() bool { return valid(s, DownloadStateValues()) }

// DownloadTaskState is `download_tasks.state` (§2.7): one file's transfer.
// `resolving` is absent — resolution is a property of the download as a whole,
// not of an individual file.
type DownloadTaskState string

const (
	TaskQueued    DownloadTaskState = "queued"
	TaskRunning   DownloadTaskState = "running"
	TaskPaused    DownloadTaskState = "paused"
	TaskVerifying DownloadTaskState = "verifying"
	TaskSucceeded DownloadTaskState = "succeeded"
	TaskFailed    DownloadTaskState = "failed"
	TaskCanceled  DownloadTaskState = "canceled"
)

// DownloadTaskStateValues lists the members of the `download_tasks.state` CHECK
// constraint, in order.
func DownloadTaskStateValues() []DownloadTaskState {
	return []DownloadTaskState{
		TaskQueued, TaskRunning, TaskPaused, TaskVerifying, TaskSucceeded,
		TaskFailed, TaskCanceled,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s DownloadTaskState) Valid() bool { return valid(s, DownloadTaskStateValues()) }
