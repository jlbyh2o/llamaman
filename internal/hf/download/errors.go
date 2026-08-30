package download

import (
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The error codes this package answers with (DESIGN sections 3, 3.6, 3.8).
//
// They are declared beside the code that returns them, which is the precedent
// internal/sse set with `invalid_topic` and internal/api restated: section 3's
// catalog grows as the endpoints that return each code arrive, rather than being
// written out in advance where a code with no code path behind it would be dead
// vocabulary.
//
// Every one of them reaches the wire through internal/api's downloads routes,
// which choose the status. That split is deliberate: a service returns a
// model.Error without knowing anything about HTTP, and the one layer that knows
// what a 409 means chooses it.
const (
	// CodeHFGated is section 3.6's `403 hf_gated`, carrying `{"repo",
	// "request_url"}`. It is the one refusal in this package a user cannot
	// resolve inside the product: access grants are browser-only on Hugging
	// Face's side, so the response exists to link out.
	CodeHFGated model.ErrorCode = "hf_gated"

	// CodeHFPrivate is a repository that is private or does not exist. The Hub
	// answers `RepoNotFound` for both, so the two are one code with a message
	// that changes on whether a token was sent: "sign in", or "this token
	// cannot see it".
	CodeHFPrivate model.ErrorCode = "hf_private"

	// CodeHFUnreachable is a Hub that could not be reached or that answered
	// something unusable. It is a 502 rather than a 500: the failure is
	// upstream, and telling a user their own daemon broke would send them
	// looking in the wrong place.
	CodeHFUnreachable model.ErrorCode = "hf_unreachable"

	// CodeInsufficientDisk is section 7.4's disk guard: free space on the target
	// filesystem must exceed `bytes_total - bytes_done + 1 GiB`. The response
	// carries the numbers, because "not enough space" without them is a message
	// a user cannot act on.
	CodeInsufficientDisk model.ErrorCode = "insufficient_disk"

	// CodeShardSetIncomplete refuses a partial shard set (section 7.3). A
	// repository mid-upload advertises `-00003-of-00005` with two shards
	// missing; queueing it produces an hour of downloading and a model
	// llama.cpp cannot load.
	CodeShardSetIncomplete model.ErrorCode = "shard_set_incomplete"

	// CodeFileNotInRepo names a file the tree does not hold at this revision.
	CodeFileNotInRepo model.ErrorCode = "file_not_in_repo"

	// CodeNoFilesSelected refuses an empty selection. It is deliberately not
	// read as "everything": a repository can hold two hundred gigabytes of
	// quantizations, and no click ever meant all of them.
	CodeNoFilesSelected model.ErrorCode = "no_files_selected"

	// CodeMultipleQuants refuses one download that names two quantizations.
	// `downloads.model_id` is singular (section 2.7), and two logical models
	// sharing one progress bar is not a thing this schema can represent.
	CodeMultipleQuants model.ErrorCode = "multiple_quants"

	// CodeMmprojAmbiguous is section 7.2's picker rather than a guess: several
	// projectors with no preference between them, listed in
	// `details.candidates`.
	CodeMmprojAmbiguous model.ErrorCode = "mmproj_ambiguous"

	// CodeDownloadNotPausable is a pause, resume, retry or cancel that the
	// download's current state does not admit — resuming one that is running,
	// pausing one that already finished.
	CodeDownloadNotPausable model.ErrorCode = "download_not_pausable"

	// CodeNoCacheRoot is a daemon with no primary cache root: the six-rule
	// detection chain runs on first boot, so this is a database that has not
	// finished booting rather than a user error, and it is a 503.
	CodeNoCacheRoot model.ErrorCode = "no_cache_root"
)

// The `download_tasks.last_error` and `downloads.error_code` vocabulary. These
// are not API codes — they are the words a task wears in the database and in the
// UI's per-file line — but they are spelled once, here, for the same reason:
// a string typed twice eventually differs.
const (
	// ErrWaitingForLock is section 7.2a's status while another process holds the
	// D27 interop lock. It is written while the task is `running` and healthy,
	// and the UI renders it as "another tool is downloading this file". It is
	// deliberately not a failure: the transfer has not gone wrong, it is queued
	// behind `hf download`.
	ErrWaitingForLock = "waiting_for_lock"
	// ErrLockTimeout is what that wait becomes after thirty minutes. The task
	// fails and NOTHING IS DISCARDED — the `.incomplete` file stands and a retry
	// resumes from it.
	ErrLockTimeout = "lock_timeout"
	// ErrSizeMismatch is a `Content-Range` total that disagrees with the size
	// the download was planned against: the file upstream is not the file this
	// download was sized from.
	ErrSizeMismatch = "size_mismatch"
	// ErrChecksumMismatch is a completed transfer whose SHA-256 is not the blob
	// name. Section 7.2 step 5: the file is marked corrupt, deleted, and retried
	// once.
	ErrChecksumMismatch = "checksum_mismatch"
	// ErrTransferFailed is a network failure that outlasted the retry budget.
	ErrTransferFailed = "transfer_failed"
	// ErrLinkFailed is a blob that verified and then could not be linked into
	// its snapshot — a full inode table, a filesystem that lost symlink support
	// since registration.
	ErrLinkFailed = "link_failed"
)
