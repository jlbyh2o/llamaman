package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Refcounted deletion (D28, DESIGN section 7.2 "Delete").
//
// "Refcount blobs across every snapshot in the repo directory, then show a
// 'will free N GB, N files' preview before executing."
//
// The refcount is the whole decision, and its scope is the REPO DIRECTORY rather
// than the snapshot: `blobs/` is shared by every revision of one repository, so
// a tokenizer or a small shard that two snapshots both link to must survive the
// deletion of either. Removing a blob out from under a live snapshot is the one
// failure this design will not tolerate — it turns another model `corrupt` for a
// reason no log would explain.
//
// The plan is computed from the FILESYSTEM, not from `model_files`. The catalog
// can be stale (another tool ran, a disk was unplugged and returned), and a
// preview that says "will free 40 GB" has to be about the disk the user is
// looking at.

// BlobRef is one blob and what deleting it would cost or save.
type BlobRef struct {
	// Etag is the blob's file name.
	Etag string
	// Path is `<repo>/blobs/<etag>`.
	Path string
	// Bytes is what it occupies on disk (st_blocks × 512), which is what
	// removing it actually frees.
	Bytes int64
	// Refs is how many snapshot entries across the WHOLE repo directory point
	// at it, before this deletion.
	Refs int
	// Dropped is how many of those references this deletion removes.
	Dropped int
}

// Plan is D28's preview, and it is also the executable form: the API renders it
// as `{files, bytes, blobs_shared_kept, in_use_by}` and, when the user confirms,
// hands the SAME value to Execute. There is deliberately no second computation
// between the preview and the act.
type Plan struct {
	// Hub and RepoID are what Execute needs to take the D27 interop lock on each
	// blob before unlinking it — the lock path is `<hub>/.locks/<repo_folder>/…`
	// and only Layout.LockPath may build it (section 7.2a).
	Hub    string
	RepoID string
	// RepoDir is the `models--…` directory the plan is scoped to.
	RepoDir string
	// Links are the snapshot entries to unlink, absolute.
	Links []string
	// Blobs are the blobs whose refcount reaches zero — the ones that will
	// actually be removed.
	Blobs []BlobRef
	// SharedKept are the blobs this deletion drops a reference to that ANOTHER
	// snapshot still holds. They are kept, and they are reported: "will free
	// 12 GB, 4 files, keeping 2 shared blobs" is a sentence a user can act on,
	// where a bare byte count is not.
	SharedKept []BlobRef
	// SnapshotDirs are the `snapshots/<commit>` directories to remove once they
	// are empty.
	SnapshotDirs []string
	// RemoveRepoDir reports whether the whole `models--…` directory would be
	// left holding nothing and should go.
	RemoveRepoDir bool
}

// Files is how many snapshot entries the plan removes.
func (p Plan) Files() int { return len(p.Links) }

// Bytes is what the plan frees: the sum of the blobs whose refcount reaches
// zero. The snapshot links themselves are counted at zero, which is correct —
// a symlink occupies a directory entry, not the model's gigabytes, and adding
// its inode cost would make the preview's arithmetic unverifiable by the user.
func (p Plan) Bytes() int64 {
	var n int64
	for _, b := range p.Blobs {
		n += b.Bytes
	}
	return n
}

// SharedBytes is what the plan does NOT free because another snapshot still
// references it.
func (p Plan) SharedBytes() int64 {
	var n int64
	for _, b := range p.SharedKept {
		n += b.Bytes
	}
	return n
}

// PlanDelete builds the plan for removing `files` from one snapshot of one
// repository. files are names relative to the snapshot directory
// (`model_files.filename`); an empty list means the whole snapshot.
//
// It refcounts every blob across every snapshot in the repo directory first, and
// only then decides. A file the caller names that is not there is skipped rather
// than refused: a delete whose preview is one file short of the catalog is still
// the right thing to do, and the alternative is a model row nothing can remove.
func PlanDelete(hub, repoID, revision string, files []string) (Plan, error) {
	l := NewLayout(hub)
	plan := Plan{Hub: hub, RepoID: repoID, RepoDir: l.RepoDir(repoID)}

	counts, sizes, err := refcount(l, repoID)
	if err != nil {
		return plan, err
	}

	snapDir := l.SnapshotDir(repoID, revision)
	targets, err := resolveTargets(snapDir, files)
	if err != nil {
		return plan, err
	}

	// dropped counts how many references THIS deletion removes per blob, which
	// is what turns a repo-wide refcount into a per-deletion decision.
	dropped := map[string]int{}
	for _, t := range targets {
		plan.Links = append(plan.Links, t.path)
		if t.etag != "" {
			dropped[t.etag]++
		}
	}
	sort.Strings(plan.Links)

	etags := make([]string, 0, len(dropped))
	for e := range dropped {
		etags = append(etags, e)
	}
	sort.Strings(etags)

	for _, e := range etags {
		ref := BlobRef{
			Etag:    e,
			Path:    l.BlobPath(repoID, e),
			Bytes:   sizes[e],
			Refs:    counts[e],
			Dropped: dropped[e],
		}
		if counts[e] > dropped[e] {
			plan.SharedKept = append(plan.SharedKept, ref)
			continue
		}
		plan.Blobs = append(plan.Blobs, ref)
	}

	// The snapshot directory goes when nothing is left in it, and the repo
	// directory goes when no snapshot is left at all. Both are computed here so
	// the preview can say "and the repository directory" rather than leaving an
	// empty tree behind that the next scan reports as nothing.
	if remaining, err := remainingAfter(snapDir, targets); err == nil && remaining == 0 {
		plan.SnapshotDirs = append(plan.SnapshotDirs, snapDir)
		plan.RemoveRepoDir = onlySnapshot(l, repoID, revision) && noBlobsSurvive(counts, dropped)
	}
	return plan, nil
}

// Execute performs the plan: unlink the snapshot entries, remove the blobs whose
// refcount is STILL zero once they are gone, then the emptied directories.
//
// The ORDER is the first safety property. Links first means a blob is never
// removed while a snapshot entry still points at it, so an interruption at any
// point leaves a cache that is consistent — some links gone and their blobs
// still present, which the next scan reports as orphan blobs and the user can
// clean up, rather than links pointing at nothing, which reads as corruption.
//
// # The re-check, and why the plan's own count is not enough
//
// A plan is computed, shown to a user, and executed when they confirm; a
// download of a second revision that shares one of its blobs can land in
// between. The downloader's write path (section 7.2) finishes a file in two
// steps — `rename .incomplete → blobs/<etag>`, then `symlink
// snapshots/<other>/<file> → ../../blobs/<etag>` — and holds the D27 lock across
// BOTH. A delete that trusted its stale count would unlink the blob between
// those two steps and leave a dangling snapshot symlink under a `models` row
// still marked `ready`, which section 7.2 calls "the one failure this design
// will not tolerate".
//
// So each blob is unlinked only while this process holds that same lock, and
// only after its references are counted again with the lock held. A blob whose
// lock another tool holds is LEFT ALONE rather than waited for: a delete must
// not block for the thirty minutes section 7.2a gives a transfer, and a blob
// that survives a delete is an orphan the next scan reports — the benign side of
// the trade, and the same outcome an interruption already produces.
func (p Plan) Execute() error {
	var errs []error
	for _, link := range p.Links {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := p.removeBlobs(); err != nil {
		errs = append(errs, err)
	}
	for _, dir := range p.SnapshotDirs {
		pruneEmptyDirs(dir)
	}
	if p.RemoveRepoDir {
		// RemoveAll would be wrong here: it would take a snapshot another
		// process created between the plan and the execution. Removing only
		// empty directories, bottom up, refuses instead of guessing.
		pruneEmptyDirs(p.RepoDir)
	}
	return errors.Join(errs...)
}

// removeBlobs is the interlocked half of Execute, described above: take the D27
// lock on every blob the plan would remove, recount with those locks held, and
// unlink only what nothing points at any more.
//
// The locks are taken for ALL of them before the recount, so the count is one
// walk of the repository rather than one per blob, and so no blob can gain a
// reference between being counted and being unlinked. Every acquisition is
// non-blocking (section 7.2a pins that too): a lock this process cannot take
// belongs to a transfer that is mid-publish, and its blob is skipped.
func (p Plan) removeBlobs() error {
	if len(p.Blobs) == 0 {
		return nil
	}
	// A plan built without the hub and repo id cannot name the lock path, and an
	// uninterlocked delete is exactly what this function exists to prevent.
	if p.Hub == "" || p.RepoID == "" {
		return errors.New("hf/cache: this delete plan carries no hub or repository id, " +
			"so the interop lock cannot be taken and no blob will be removed")
	}
	l := NewLayout(p.Hub)

	held := make(map[string]*Lock, len(p.Blobs))
	defer func() {
		for _, lock := range held {
			_ = lock.Release()
		}
	}()
	var errs []error
	for _, b := range p.Blobs {
		lock, err := Acquire(l.LockPath(p.RepoID, b.Etag))
		if err != nil {
			if !errors.Is(err, ErrLocked) {
				errs = append(errs, err)
			}
			continue
		}
		held[b.Etag] = lock
	}

	counts, _, err := refcount(l, p.RepoID)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}

	for _, b := range p.Blobs {
		if _, ok := held[b.Etag]; !ok {
			continue
		}
		if counts[b.Etag] > 0 {
			// Something linked it while the plan was being confirmed. Keeping it
			// is the whole point of the recount.
			continue
		}
		if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		// The `.incomplete` sibling of a blob we are removing belongs to the
		// same content and is ours to drop with it — but only when the blob
		// itself was ours to remove, which is the refcount decision above.
		if err := os.Remove(b.Path + IncompleteSuffix); err != nil && !os.IsNotExist(err) {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type target struct {
	path string
	etag string
}

// resolveTargets lists the snapshot entries a delete names, resolving each one's
// blob. An empty `files` means every file in the snapshot.
func resolveTargets(snapDir string, files []string) ([]target, error) {
	blobsDir := filepath.Clean(filepath.Join(snapDir, "..", "..", BlobsDirName))

	var out []target
	appendTarget := func(path string) {
		t := target{path: path}
		if etag, ok := blobOf(path, blobsDir); ok {
			t.etag = etag
		}
		out = append(out, t)
	}

	if len(files) > 0 {
		for _, name := range files {
			p := filepath.Join(snapDir, filepath.FromSlash(name))
			if _, err := os.Lstat(p); err != nil {
				continue
			}
			appendTarget(p)
		}
		return out, nil
	}

	err := filepath.WalkDir(snapDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is not a file we can delete
		}
		if d.IsDir() {
			return nil
		}
		appendTarget(path)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	return out, nil
}

// refcount counts, for every blob in the repository, how many snapshot entries
// across EVERY snapshot point at it, and how large it is. This is D28's whole
// mechanism.
func refcount(l Layout, repoID string) (counts map[string]int, sizes map[string]int64, err error) {
	counts = map[string]int{}
	sizes = map[string]int64{}

	blobsDir := l.BlobsDir(repoID)
	snapshots, err := os.ReadDir(l.SnapshotsDir(repoID))
	if err != nil && !os.IsNotExist(err) {
		return counts, sizes, err
	}
	for _, sd := range snapshots {
		if !sd.IsDir() {
			continue
		}
		dir := l.SnapshotDir(repoID, sd.Name())
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil //nolint:nilerr // an unreadable entry cannot hold a reference we can count
			}
			if etag, ok := blobOf(path, blobsDir); ok {
				counts[etag]++
			}
			return nil
		})
	}

	entries, err := os.ReadDir(blobsDir)
	if err != nil && !os.IsNotExist(err) {
		return counts, sizes, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), IncompleteSuffix) {
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			sizes[e.Name()] = allocatedBytes(info, info.Size())
		}
	}
	return counts, sizes, nil
}

// blobOf resolves a snapshot entry to the blob name it links to, and reports
// false for a regular file (copy mode, F17) or a link that leaves this
// repository's blobs directory — neither of which this refcount governs.
func blobOf(path, blobsDir string) (string, bool) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(path), target)
	}
	abs = filepath.Clean(abs)
	if filepath.Dir(abs) != filepath.Clean(blobsDir) {
		return "", false
	}
	return filepath.Base(abs), true
}

// remainingAfter counts the files that would still be in the snapshot directory
// once the targets are gone.
func remainingAfter(snapDir string, targets []target) (int, error) {
	gone := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		gone[t.path] = struct{}{}
	}
	remaining := 0
	err := filepath.WalkDir(snapDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is still an entry, and is counted below
		}
		if _, ok := gone[path]; !ok {
			remaining++
		}
		return nil
	})
	return remaining, err
}

// onlySnapshot reports whether revision is the only snapshot directory in the
// repository.
func onlySnapshot(l Layout, repoID, revision string) bool {
	entries, err := os.ReadDir(l.SnapshotsDir(repoID))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != revision {
			return false
		}
	}
	return true
}

// noBlobsSurvive reports whether every counted blob is being dropped to zero —
// the condition under which removing the repository directory takes nothing with
// it that anything still needs.
func noBlobsSurvive(counts map[string]int, dropped map[string]int) bool {
	for etag, n := range counts {
		if n > dropped[etag] {
			return false
		}
	}
	return true
}

// pruneEmptyDirs removes dir and every empty directory beneath it, bottom up.
// A directory that still holds anything is left alone, which is what makes this
// safe to call on a repo directory a concurrent download is writing into.
func pruneEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a directory we cannot read is one we must not remove
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails with ENOTEMPTY when something remains, which is the point
	}
}
