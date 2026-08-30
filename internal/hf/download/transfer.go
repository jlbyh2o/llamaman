package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// One file's transfer (DESIGN section 7.2's write path, section 7.4's resume,
// D26, D27).
//
// The write path, in the order the design states it:
//
//  1. HEAD the resolve URL, recording the BLOB NAME and the HTTP VALIDATOR as
//     two different strings that must never be conflated.
//  2. If `blobs/<etag>` already exists at the right size, skip straight to
//     linking. Another tool may have put it there, and this short-circuit is
//     what actually makes the shared cache shared.
//  3. flock the D27 lock path for the duration of the transfer.
//  4. Stream into `blobs/<etag>.incomplete`, computing SHA-256 inline; fsync.
//  5. Verify: for an LFS object the digest must equal `<etag>`. A mismatch
//     marks the file corrupt, deletes it, and retries once.
//  6. rename `.incomplete` → `blobs/<etag>` — same directory, so atomic and
//     never a 40 GB copy.
//  7. mkdir -p `snapshots/<commit>` and create the RELATIVE symlink; write
//     `refs/<branch>`.
//  8. Files 0644, directories 0755.
//
// # One connection per file (D26)
//
// There is no striping here and there never will be. Single-stream makes resume
// a one-variable problem — an offset — instead of interval bookkeeping, and,
// decisively, it keeps the partial file byte-for-byte compatible with
// `huggingface_hub`'s own `.incomplete` semantics, which a striped file could
// never be. Sharded models still progress on several shards at once, through the
// FILE-level pool in service.go.

// transferAttempts is section 7.4's "network failures retry up to 5 times with
// backoff". Each attempt resumes from whatever is on disk, so the fifth attempt
// of a 40 GB file is not a fifth download — it is the last few percent.
const transferAttempts = 5

// hashRebuildBuffer is how much of an existing `.incomplete` is read at a time
// when rebuilding the hasher state. Section 7.4: "the hasher state is rebuilt by
// re-reading the existing bytes from disk before continuing — a sequential read
// at gigabytes per second, negligible beside the network."
const hashRebuildBuffer = 1 << 20

// task is everything one file's transfer needs, assembled by the worker.
type task struct {
	row store.DownloadTask
	// file is the `model_files` row this task fills.
	file model.ModelFile
	// modelID is the file's own model, which for a projector is not the
	// download's model (section 7.3).
	modelID string
	// repo, revision are the identity the URL and the cache paths are built
	// from. revision is the RESOLVED commit.
	repo     string
	revision string
	// refName is the branch the request named, for the `refs/` entry. Empty
	// when the request named a commit, in which case no ref is written: a
	// `refs/<sha>` file would invent a branch that does not exist.
	refName string
	layout  cache.Layout

	// progress is the live byte counter this task contributes to the download's
	// total. It is atomic because the 1 Hz aggregator reads it from another
	// goroutine.
	progress atomic.Int64
	// startedAt is the offset this task resumed from, which is what makes the
	// download's ETA honest.
	startedAt atomic.Int64
	// done marks a task that finished in this run, so the aggregator stops
	// counting it twice.
	done atomic.Bool
}

// runTask performs one file's transfer, with the retry budget of section 7.4.
//
// It returns nil when the file is on disk, verified and linked. Every other
// return is a task-level failure whose code is one of errors.go's task
// vocabulary; the worker folds it upward.
func (s *Service) runTask(ctx context.Context, t *task, deleteOnCancel func() bool) error {
	var last error
	for attempt := 1; attempt <= transferAttempts; attempt++ {
		err := s.transferOnce(ctx, t, deleteOnCancel)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			// A canceled context is a pause, a cancel or a shutdown. None of
			// them is a network failure and none of them spends an attempt.
			return err
		case !retryableTransfer(err):
			return err
		}
		last = err
		s.log.Warn("hf/download: transfer failed, retrying",
			"file", t.file.Filename, "attempt", attempt, "error", err)
		if err := s.recordTaskError(ctx, t.row.ID, err.Error()); err != nil {
			return err
		}
		if attempt == transferAttempts {
			break
		}
		if err := s.sleep(ctx, transferBackoff(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s: %w", ErrTransferFailed, last)
}

// transferBackoff is the wait between transfer attempts: 250 ms, 500 ms, 1 s,
// 2 s, capped at eight seconds.
//
// It is deliberately shorter than model.JobBackoff, which governs whole JOBS.
// The failure being retried here is a dropped connection on a transfer that is
// already 38 GB in and holds a file-pool slot while it waits; four seconds of
// idling — the job backoff's first step — buys nothing and costs a user real
// time on every flaky link. A job that has exhausted these five attempts still
// falls back to the job-level retry, which is where the longer wait belongs.
func transferBackoff(attempt int) time.Duration {
	d := 250 * time.Millisecond << (attempt - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// transferOnce is one attempt at one file.
func (s *Service) transferOnce(ctx context.Context, t *task, deleteOnCancel func() bool) error {
	// --- step 1: what is this file called on disk, and how big is it?
	meta, err := s.resolveMeta(ctx, t)
	if err != nil {
		return err
	}
	if meta.Etag == "" {
		return fmt.Errorf("hf/download: %s has no etag, so its blob has no name", t.file.Filename)
	}

	blobPath := t.layout.BlobPath(t.repo, meta.Etag)
	incomplete := t.layout.IncompletePath(t.repo, meta.Etag)

	if err := os.MkdirAll(filepath.Dir(blobPath), cache.DirMode); err != nil {
		return fmt.Errorf("hf/download: creating the blobs directory: %w", err)
	}

	// --- step 2: is it already here? Another tool may have put it there, and
	// this short-circuit is what makes SPEC section 3.2's shared cache actually
	// shared rather than merely co-located.
	if st, err := os.Stat(blobPath); err == nil && (meta.Size <= 0 || st.Size() == meta.Size) {
		t.progress.Store(st.Size())
		if err := s.recordMeta(ctx, t, meta, incomplete, st.Size()); err != nil {
			return err
		}
		return s.link(ctx, t, meta.Etag, blobPath)
	}

	// --- step 3: the D27 interop lock, held across the whole transfer.
	lockPath := t.layout.LockPath(t.repo, meta.Etag)
	lock, err := cache.AcquireWait(ctx, lockPath, cache.LockTimeout, func() {
		// Called once, on the first refusal. The UI says "another tool is
		// downloading this file", and the task stays `running` — this is a queue
		// behind `hf download`, not a failure.
		if err := s.recordTaskError(ctx, t.row.ID, ErrWaitingForLock); err != nil {
			s.log.Warn("hf/download: could not record the lock wait", "error", err)
		}
	})
	if err != nil {
		if errors.Is(err, cache.ErrLocked) {
			// Thirty minutes elapsed. NOTHING IS DISCARDED: the `.incomplete`
			// file stands and a retry resumes from it.
			return fmt.Errorf("%s: %w", ErrLockTimeout, err)
		}
		return err
	}
	defer func() { _ = lock.Release() }()

	// The lock may have been held by a tool that finished the file while we
	// waited, which is the ordinary outcome of a wait rather than a rare one.
	if st, err := os.Stat(blobPath); err == nil && (meta.Size <= 0 || st.Size() == meta.Size) {
		t.progress.Store(st.Size())
		if err := s.recordMeta(ctx, t, meta, incomplete, st.Size()); err != nil {
			return err
		}
		return s.link(ctx, t, meta.Etag, blobPath)
	}
	if err := s.clearTaskError(ctx, t.row.ID); err != nil {
		return err
	}

	// --- step 4: stream, resuming from whatever is on disk.
	//
	// `final` is what THIS response said, not what the plan assumed. Section
	// 7.4: "on success the validator recorded from this response replaces the
	// stored one" — so it, and never `meta`, is what the row is closed with
	// below. Writing `meta` back here would null the validator the transfer just
	// learned and make the NEXT resume send no `If-Range` at all.
	written, digest, final, err := s.stream(ctx, t, meta, incomplete)
	if err != nil {
		if ctx.Err() != nil && deleteOnCancel != nil && deleteOnCancel() {
			// A cancel that asked for the partial to go. The handle is closed
			// by now, so this is the one safe moment to remove it.
			_ = os.Remove(incomplete)
		}
		return err
	}

	// The blob path and the `.incomplete` path were both built from the name the
	// PLAN knew. If the response named a different blob, the bytes on disk belong
	// to a file this attempt did not fetch, and renaming them onto either name
	// would publish a blob whose content does not match it — the one outcome
	// content addressing exists to make impossible.
	//
	// Pinning the resolved commit (section 2.6) is what makes this
	// near-unreachable, since a file at a commit cannot change. It is checked
	// rather than assumed because the cost of being wrong is a corrupt entry in a
	// cache other tools also read.
	if final.Etag != "" && final.Etag != meta.Etag {
		_ = os.Remove(incomplete)
		return fmt.Errorf("%s: %s is now blob %s where %s was expected",
			ErrChecksumMismatch, t.file.Filename, final.Etag, meta.Etag)
	}

	// --- step 5: verify. For an LFS object the blob name IS the sha256, so the
	// check is free and total: a resumed transfer that spliced the wrong bytes
	// cannot survive it, which is why section 7.4 can call `If-Range` an
	// optimization rather than the correctness mechanism.
	if isSHA256(final.Etag) && !strings.EqualFold(digest, final.Etag) {
		_ = os.Remove(incomplete)
		if err := s.markFileCorrupt(ctx, t); err != nil {
			return err
		}
		return fmt.Errorf("%s: %s hashed to %s, expected %s",
			ErrChecksumMismatch, t.file.Filename, digest, final.Etag)
	}

	// --- step 6: rename. Same directory, so atomic and never a 40 GB copy.
	if err := os.Rename(incomplete, blobPath); err != nil {
		return fmt.Errorf("hf/download: publishing the blob: %w", err)
	}
	if err := os.Chmod(blobPath, cache.FileMode); err != nil {
		// A blob another tool created keeps its own mode, and a umask may have
		// narrowed ours. Neither is worth failing a completed transfer over;
		// the mode matters so other tools can READ the file, and a warning is
		// what tells the operator when that is at risk.
		s.log.Warn("hf/download: could not set the blob's mode", "path", blobPath, "error", err)
	}
	t.progress.Store(written)

	if err := s.recordMeta(ctx, t, final, incomplete, written); err != nil {
		return err
	}
	// --- step 7: link it into the snapshot.
	return s.link(ctx, t, final.Etag, blobPath)
}

// resolveMeta answers step 1. It prefers the etag the tree already gave — for an
// LFS object the tree's `oid` IS the blob name — and HEADs only when it has to,
// because a sharded download would otherwise spend one round trip per shard
// learning what it already knew.
func (s *Service) resolveMeta(ctx context.Context, t *task) (hf.FileMeta, error) {
	if t.row.Etag != nil && *t.row.Etag != "" && t.row.BytesTotal > 0 {
		return hf.FileMeta{
			URL:  t.row.URL,
			Etag: *t.row.Etag,
			Size: t.row.BytesTotal,
			// The validator columns are deliberately NOT filled from the row
			// here: they belong to the OpenParams, which reads them from the row
			// itself, and duplicating them into a FileMeta would invite a caller
			// to write one back as though this response had issued it.
		}, nil
	}
	meta, err := s.client.Head(ctx, t.repo, t.revision, t.file.Filename)
	if err != nil {
		return hf.FileMeta{}, hubError(err)
	}
	meta.URL = t.row.URL
	return meta, nil
}

// stream is step 4: open the transfer, resume or restart, hash inline, fsync.
//
// It returns the metadata THIS response carried, merged over the planning
// metadata. The caller closes the row with that and not with what it started
// from: section 7.4's "on success the validator recorded from this response
// replaces the stored one" is about the value the origin just issued, and
// writing the plan's copy back would erase it.
func (s *Service) stream(ctx context.Context, t *task, meta hf.FileMeta, incomplete string) (
	written int64, digest string, final hf.FileMeta, err error) {

	// Until a response arrives, what the plan knew is the best answer available;
	// every early return carries it so the caller never sees a zero FileMeta.
	final = meta

	offset := int64(0)
	if st, serr := os.Stat(incomplete); serr == nil {
		offset = st.Size()
	}
	if meta.Size > 0 && offset > meta.Size {
		// A partial longer than the object cannot be a prefix of it. Discard it
		// rather than send a Range the origin will refuse.
		offset = 0
		_ = os.Remove(incomplete)
	}

	params := hf.OpenParams{URL: t.row.URL, Repo: t.repo, Offset: offset}
	if offset > 0 {
		params.Validator = deref(t.row.Validator)
		params.ValidatorHost = deref(t.row.ValidatorHost)
		params.LastModified = deref(t.row.LastModified)
	}

	tr, err := s.client.Open(ctx, params)
	if errors.Is(err, hf.ErrNoRange) {
		// A 416: the partial is longer than the object, or the object changed.
		// Restart from zero.
		_ = os.Remove(incomplete)
		offset = 0
		params.Offset, params.Validator, params.ValidatorHost, params.LastModified = 0, "", "", ""
		tr, err = s.client.Open(ctx, params)
	}
	if err != nil {
		return 0, "", final, hubError(err)
	}
	defer func() { _ = tr.Body.Close() }()

	if offset > 0 && !tr.Resumed {
		// Section 7.4: "a `200` means the server ignored the range or the file
		// changed upstream, so the partial is discarded and the transfer
		// restarts (and the stale `validator` is cleared)".
		if err := s.clearValidator(ctx, t.row.ID); err != nil {
			return 0, "", final, err
		}
		if err := os.Remove(incomplete); err != nil && !os.IsNotExist(err) {
			return 0, "", final, fmt.Errorf("hf/download: discarding a stale partial: %w", err)
		}
		offset = 0
	}
	if tr.TotalSize > 0 && t.row.BytesTotal > 0 && tr.TotalSize != t.row.BytesTotal {
		// The `Content-Range` total must match the recorded size or the task
		// fails as `size_mismatch`. This is fatal rather than retryable: the
		// file upstream is not the file this download was sized against, and
		// asking again produces the same contradiction.
		return 0, "", final, &hf.SizeMismatchError{
			Expected: t.row.BytesTotal, Got: tr.TotalSize, URL: t.row.URL,
		}
	}

	// The hasher must cover the WHOLE file, so its state is rebuilt from the
	// bytes already on disk before a single new byte is appended.
	h := sha256.New()
	if offset > 0 {
		if err := rehash(h, incomplete, offset); err != nil {
			return 0, "", final, err
		}
	}
	t.startedAt.Store(offset)
	t.progress.Store(offset)

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(incomplete, flags, cache.FileMode)
	if err != nil {
		return 0, "", final, fmt.Errorf("hf/download: opening the partial file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// Record what THIS response said before a byte moves, so a transfer that
	// dies mid-stream leaves behind a validator the next attempt can use. That
	// is the difference between a resumable partial and forty gigabytes this
	// daemon will re-download because it forgot which ETag it was given.
	final = mergeValidator(meta, tr.Meta)
	if err := s.recordMeta(ctx, t, final, incomplete, offset); err != nil {
		return 0, "", final, err
	}

	counter := &progressWriter{task: t}
	dst := io.MultiWriter(f, h, counter)
	src := io.Reader(tr.Body)
	if s.limit != nil {
		src = s.limit.reader(ctx, src)
	}

	n, err := io.Copy(dst, src)
	if err != nil {
		return offset + n, "", final, fmt.Errorf("hf/download: streaming %s: %w", t.file.Filename, err)
	}
	total := offset + n
	if tr.TotalSize > 0 && total != tr.TotalSize {
		// A body that ended early. The partial is kept — that is the whole point
		// of `.incomplete` — and the next attempt resumes from where it stopped.
		return total, "", final, fmt.Errorf("hf/download: %s ended at %d of %d bytes",
			t.file.Filename, total, tr.TotalSize)
	}
	if err := f.Sync(); err != nil {
		return total, "", final, fmt.Errorf("hf/download: fsync: %w", err)
	}
	return total, hex.EncodeToString(h.Sum(nil)), final, nil
}

// mergeValidator keeps the blob name from the planning-time metadata and takes
// the HTTP validator from THIS response.
//
// The two halves come from different places on purpose. The blob name may have
// been known from the tree's `lfs.oid` and never re-fetched; the validator
// belongs to the response that just issued it, on the host that issued it, and
// section 7.4's "on success the validator recorded from this response replaces
// the stored one" is a statement about that half alone.
func mergeValidator(planned, actual hf.FileMeta) hf.FileMeta {
	out := planned
	if actual.Etag != "" {
		out.Etag = actual.Etag
	}
	out.Validator = actual.Validator
	out.ValidatorHost = actual.ValidatorHost
	out.LastModified = actual.LastModified
	if actual.Size > 0 {
		out.Size = actual.Size
	}
	if actual.Commit != "" {
		out.Commit = actual.Commit
	}
	return out
}

// rehash reads the first n bytes of path into h. It is section 7.4's integrity
// rule: SHA-256 must cover the whole file, and a resumed transfer that hashed
// only the new bytes would produce a digest matching nothing.
func rehash(h hash.Hash, path string, n int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("hf/download: rebuilding the digest: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, hashRebuildBuffer)
	copied, err := io.CopyBuffer(h, io.LimitReader(f, n), buf)
	if err != nil {
		return fmt.Errorf("hf/download: rebuilding the digest: %w", err)
	}
	if copied != n {
		return fmt.Errorf("hf/download: rebuilding the digest: read %d of %d bytes", copied, n)
	}
	return nil
}

// progressWriter counts bytes into the task's atomic counter. It is a Writer
// rather than a Reader wrapper so it sits in the same MultiWriter as the file
// and the hasher and therefore counts exactly what was written, never what was
// merely received.
type progressWriter struct{ task *task }

func (w *progressWriter) Write(p []byte) (int, error) {
	w.task.progress.Add(int64(len(p)))
	return len(p), nil
}

// link is step 7: the relative symlink into the snapshot, and the `refs/` entry.
//
// The symlink is RELATIVE (`../../blobs/<etag>`) and that is not a style choice:
// it is what keeps the cache movable and what `huggingface_hub` writes, so a
// tree this product creates survives being copied to another disk exactly as one
// the Python tool created does.
func (s *Service) link(ctx context.Context, t *task, etag, blobPath string) error {
	linkPath := t.layout.SnapshotFile(t.repo, t.revision, t.file.Filename)
	if err := os.MkdirAll(filepath.Dir(linkPath), cache.DirMode); err != nil {
		return fmt.Errorf("%s: %w", ErrLinkFailed, err)
	}

	target := cache.LinkTarget(t.file.Filename, etag)
	if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s: %w", ErrLinkFailed, err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		// F17: a filesystem that rejects symlinks falls back to copy mode with a
		// warning about the extra disk cost. The probe at root registration is
		// what usually catches this; the fallback is here because a root can be
		// remounted between registration and a download.
		s.log.Warn("hf/download: symlink rejected, copying instead",
			"path", linkPath, "error", err)
		if err := copyFile(blobPath, linkPath); err != nil {
			return fmt.Errorf("%s: %w", ErrLinkFailed, err)
		}
	}

	if err := s.writeRef(t); err != nil {
		// A missing `refs/` entry costs a display field and nothing else
		// (section 7.2 reads it only to fill `models.ref_name`), so it is never
		// a reason to fail a transfer that landed.
		s.log.Warn("hf/download: could not write the ref", "repo", t.repo, "error", err)
	}

	st, err := os.Stat(blobPath)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrLinkFailed, err)
	}
	return s.markFilePresent(ctx, t, etag, blobPath, linkPath, st)
}

// writeRef records `refs/<branch> = <commit>`, which is what lets a scan label a
// snapshot with the branch that points at it instead of a bare short sha.
func (s *Service) writeRef(t *task) error {
	// A revision that IS the commit names no branch, and writing `refs/<sha>`
	// would invent one.
	name := t.refName
	if name == "" || name == t.revision {
		return nil
	}
	p := t.layout.RefPath(t.repo, name)
	if err := os.MkdirAll(filepath.Dir(p), cache.DirMode); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(t.revision), cache.FileMode)
}

// copyFile is the F17 fallback. It is a plain copy rather than a reflink or a
// hard link on purpose: a hard link would make the snapshot entry and the blob
// one inode, so deleting either would corrupt the refcount D28 computes from the
// filesystem, and a reflink is not portable across the filesystems this runs on.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, cache.FileMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// -----------------------------------------------------------------------------
// The row writes a transfer makes
// -----------------------------------------------------------------------------

func (s *Service) recordMeta(ctx context.Context, t *task, meta hf.FileMeta,
	incomplete string, done int64) error {

	row := t.row
	row.BytesDone = done
	if meta.Size > 0 {
		row.BytesTotal = meta.Size
	}
	if meta.Etag != "" {
		e := meta.Etag
		row.Etag = &e
	}
	row.Validator = optional(meta.Validator)
	row.ValidatorHost = optional(meta.ValidatorHost)
	row.LastModified = optional(meta.LastModified)
	row.IncompletePath = &incomplete

	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.UpdateDownloadTaskTransfer(ctx, tx, row)
		return err
	}); err != nil {
		return err
	}
	t.row = row
	return nil
}

func (s *Service) markFilePresent(ctx context.Context, t *task, etag, blobPath, linkPath string,
	st os.FileInfo) error {

	now := s.now().UnixMilli()
	f := t.file
	e := etag
	f.Etag = &e
	f.BlobPath = &blobPath
	f.LinkPath = &linkPath
	f.SizeBytes = st.Size()
	f.BytesOnDisk = cache.AllocatedBytes(st)
	f.State = model.FilePresent
	f.ChecksumVerified = isSHA256(etag)
	f.UpdatedAt = now

	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.store.UpsertModelFile(ctx, tx, f); err != nil {
			return err
		}
		finished := now
		if _, err := s.store.SetDownloadTaskState(ctx, tx, t.row.ID, model.TaskSucceeded,
			nil, &finished, nil); err != nil {
			return err
		}
		if _, err := s.store.UpdateDownloadTaskProgress(ctx, tx, t.row.ID, st.Size()); err != nil {
			return err
		}
		// A task reached a terminal state, so `downloads.state` is re-folded in
		// the same transaction (section 2.7). The last shard to land is what
		// moves the download to `verifying`.
		_, err := s.writeState(ctx, tx, t.row.DownloadID, stateWrite{})
		return err
	})
}

func (s *Service) markFileCorrupt(ctx context.Context, t *task) error {
	now := s.now().UnixMilli()
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.SetModelFileState(ctx, tx, t.file.ID, model.FileCorrupt, now)
		return err
	})
}

// recordTaskError and clearTaskError below write `last_error` and re-assert the
// state the task ALREADY holds — both run inside a transfer, after startTask
// moved the row to `running`. They therefore change nothing the fold reads, and
// are the only task writes in this package with no writeState beside them.

func (s *Service) recordTaskError(ctx context.Context, taskID, msg string) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.BumpDownloadTaskAttempts(ctx, tx, taskID); err != nil {
			return err
		}
		_, err := s.store.SetDownloadTaskState(ctx, tx, taskID, model.TaskRunning, nil, nil, &msg)
		return err
	})
}

func (s *Service) clearTaskError(ctx context.Context, taskID string) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.SetDownloadTaskState(ctx, tx, taskID, model.TaskRunning, nil, nil, nil)
		return err
	})
}

func (s *Service) clearValidator(ctx context.Context, taskID string) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.ClearDownloadTaskValidator(ctx, tx, taskID)
		return err
	})
}

// -----------------------------------------------------------------------------
// The byte-rate limiter
// -----------------------------------------------------------------------------

// rateLimiter is section 7.4's optional token bucket, active only when
// `hf.rate_limit_bytes_sec > 0`. It is shared across every file in flight, which
// is the only reading of the setting that means anything: a per-file limit of
// 10 MB/s with three files running is a 30 MB/s limit.
type rateLimiter struct {
	mu       sync.Mutex
	rate     int64
	capacity int64
	tokens   float64
	last     time.Time
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

func newRateLimiter(bytesPerSec int64, now func() time.Time) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	// One second of burst. Smaller would stall on the first 1 MiB read of a
	// fast link; larger would let the average drift above the setting.
	return &rateLimiter{
		rate: bytesPerSec, capacity: bytesPerSec, tokens: float64(bytesPerSec),
		last: now(), now: now, sleep: sleepCtx,
	}
}

func (l *rateLimiter) reader(ctx context.Context, r io.Reader) io.Reader {
	return &limitedReader{ctx: ctx, r: r, l: l}
}

// take blocks until n bytes' worth of tokens are available.
func (l *rateLimiter) take(ctx context.Context, n int64) error {
	for {
		l.mu.Lock()
		now := l.now()
		elapsed := now.Sub(l.last).Seconds()
		if elapsed > 0 {
			l.tokens += elapsed * float64(l.rate)
			if l.tokens > float64(l.capacity) {
				l.tokens = float64(l.capacity)
			}
			l.last = now
		}
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return nil
		}
		deficit := float64(n) - l.tokens
		wait := time.Duration(deficit / float64(l.rate) * float64(time.Second))
		l.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	l   *rateLimiter
}

func (r *limitedReader) Read(p []byte) (int, error) {
	// Never ask for more than the bucket can hold, or take() would wait forever
	// for tokens the capacity cannot accumulate.
	if int64(len(p)) > r.l.capacity {
		p = p[:r.l.capacity]
	}
	n, err := r.r.Read(p)
	if n > 0 {
		if werr := r.l.take(r.ctx, int64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// isSHA256 reports whether a blob name is a 64-character hex digest, which for
// an LFS object it is. A plain git blob's etag is not, and section 7.2's step 5
// only claims the digest check for LFS objects — a repository serving a small
// non-LFS GGUF is verified by its size and its `Content-Range` alone.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// retryableTransfer reports whether a failure is worth another attempt. A size
// mismatch and a checksum mismatch are not: both mean the bytes upstream are not
// the bytes this download was planned against, and asking again produces the
// same answer.
func retryableTransfer(err error) bool {
	var sm *hf.SizeMismatchError
	if errors.As(err, &sm) {
		return false
	}
	if strings.Contains(err.Error(), ErrChecksumMismatch) {
		// One retry is what section 7.2 step 5 grants a checksum mismatch, and
		// the partial has already been deleted, so the next attempt is a fresh
		// download rather than a resume of the bytes that failed.
		return true
	}
	if strings.Contains(err.Error(), ErrLockTimeout) {
		return false
	}
	var me model.Error
	if errors.As(err, &me) {
		switch me.Code {
		case CodeHFGated, CodeHFPrivate, CodeFileNotInRepo:
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
