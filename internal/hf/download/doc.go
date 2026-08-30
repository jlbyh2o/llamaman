// Package download is the resumable Hugging Face downloader: the
// `model_download` job worker, the rows it writes, and the guards in front of
// `POST /api/v1/downloads` (DESIGN sections 2.7, 3.8, 7.2, 7.3, 7.4; D26, D27).
//
// # One connection per file, three files at a time (D26)
//
// There is no striping here and there never will be. Single-stream makes resume
// a one-variable problem — an offset — instead of interval bookkeeping, and,
// decisively, it keeps the partial byte-for-byte compatible with
// `huggingface_hub`'s own `.incomplete` semantics, which a striped file could
// never be. A user who starts a download here and finishes it with `hf download`
// loses nothing, and that is the whole of SPEC section 3.2's shared-cache
// promise on the write side. Sharded models still progress on several shards at
// once, through a FILE-level pool shared across every download this daemon is
// running.
//
// # Three rows, one transaction (section 2.3a)
//
// A `jobs` row carries scheduling, `downloads.state` carries domain state, and
// `download_tasks` carry per-file state and are folded upward by Fold — the
// single writer of that column, which is what makes the property "stored state
// equals the fold of the task rows" testable rather than aspirational. Create
// writes all three in one transaction, so `idx_jobs_one_live_per_subject` makes
// a second live job for one download structurally impossible.
//
// # Pause needs no signal
//
// The queue's Pause writes `jobs.state='paused'` and releases the lease. The
// running worker's next heartbeat finds the lease gone, cuts its own context and
// unwinds; the queue drops whatever outcome it returns, because writing to a job
// you no longer own is the one thing a worker must never do. So the
// `.incomplete` files stand exactly where they are and Resume continues from the
// byte each one reached. That mechanism is the reason `paused` is a job state
// rather than a downloads-only flag.
//
// # What makes resume actually work
//
// Four things, and the design is explicit that getting any of them wrong fails
// silently:
//
//  1. The conditional header is chosen by section 7.4's three-row rule, in
//     hf.OpenParams.ConditionalHeader. The common case on Hugging Face is to
//     send NO `If-Range` at all, because `resolve/` redirects to a CDN whose
//     `ETag` need not equal `x-linked-etag` and may differ between two requests
//     for the same bytes.
//  2. A `200` where a `206` was expected means the origin ignored the range or
//     the file changed. The partial is discarded, the stale validator cleared,
//     and the transfer restarts.
//  3. `Content-Range`'s total must equal the size the download was planned
//     against, or the task fails as `size_mismatch` — fatal, not retryable,
//     because the bytes upstream are not the bytes this download was sized from.
//  4. The SHA-256 covers the WHOLE file: the hasher is rebuilt by re-reading the
//     existing `.incomplete` bytes before a single new byte is appended. That
//     digest, checked against the blob name, is the real integrity gate — which
//     is precisely why `If-Range` can be treated as an optimization rather than
//     as the correctness mechanism.
//
// # The interop lock (D27)
//
// `<hub>/.locks/<repo_folder>/<etag>.lock`, taken with `flock(2)` through
// internal/hf/cache — the same path and the same syscall `huggingface_hub`
// takes. It is held across the transfer and the rename. A lock another process
// holds is not a failure: the task stays `running` with
// `last_error='waiting_for_lock'`, the UI says "another tool is downloading this
// file", and the worker retries every second for thirty minutes before failing
// as `lock_timeout` with the partial intact.
package download
