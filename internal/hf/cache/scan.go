package cache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The cache scan (DESIGN section 7.2, "Scan").
//
// This half walks the filesystem and reports what is there. It does not touch
// the database, group shards or pair projectors — internal/models owns all
// three, because they are decisions about the CATALOG rather than facts about
// the disk, and keeping the walk free of them is what lets a scan be tested
// against a fixture tree with no store at all.
//
// Two rules from section 7.2 are structural here rather than incidental:
//
//   - THE REVISION COMES FROM THE SNAPSHOT DIRECTORY NAME, never from `refs/`.
//     A repo directory legitimately holds several snapshots at once, and files
//     fetched by explicit revision have no `refs/` entry at all. Taking the
//     revision from `refs/main` would stamp one sha onto every one of them and
//     collapse distinct snapshots onto a single identity.
//   - `refs/` is read for ONE purpose: to fill the display field `ref_name`.
//   - `.no_exist/` is skipped entirely. It is a negative cache, not snapshots.

// Progress is what a running scan reports, at most every ProgressEvery
// (section 7.2: "Progress counters update every 250 ms").
type Progress struct {
	DirsSeen   int
	FilesSeen  int
	ModelsSeen int
	BytesTotal int64
	// Current is the repo folder the walk is inside, for the UI's "scanning
	// models--bartowski--…" line.
	Current string
}

// ProgressEvery is section 7.2's counter interval.
const ProgressEvery = 250 * time.Millisecond

// FileEntry is one file inside a snapshot directory.
type FileEntry struct {
	// Name is the file's path INSIDE the snapshot, slash-separated. It is
	// `model_files.filename` and the `<path>` half of a resolve URL.
	Name string
	// Path is the absolute path of the snapshot entry — the symlink itself,
	// which is what llama.cpp is handed and what `models.primary_file` resolves
	// against.
	Path string
	// BlobPath is the resolved `blobs/<etag>` this entry points at, empty when
	// the entry is a regular file (the copy-mode fallback of F17) or when the
	// link does not resolve into this repo's blobs directory.
	BlobPath string
	// Etag is the blob's file NAME — for an LFS object the sha256 hex. It is
	// `model_files.etag`, and section 7.4 is emphatic that it is never sent in
	// a header.
	Etag string
	// SizeBytes is the file's logical length, followed through the link.
	SizeBytes int64
	// BytesOnDisk is what the blob actually occupies: st_blocks × 512, so a
	// sparse or partially-allocated file is not reported as its logical size.
	BytesOnDisk int64
	// Broken marks a symlink whose target is gone — a `stray_files` row with
	// reason `broken_symlink`, and a file that cannot be part of a ready model.
	Broken bool
	// IsGGUF is by extension only. A `.gguf` that does not parse is reported
	// with Shape nil and ParseErr set, and becomes a stray, rather than being
	// silently dropped.
	IsGGUF bool
	// Shape is the parsed header, nil for a non-GGUF or an unparsable one.
	Shape *gguf.Shape
	// KV is the full metadata table, kept only when ScanOptions.KeepMetadata is
	// set — `GET /models/{id}/metadata` wants it; a 300-model boot scan does
	// not.
	KV []gguf.KVPair
	// ParseErr is why a `.gguf` produced no Shape.
	ParseErr error
	// Shard is the file's place in a sharded set as its NAME declares it, and
	// ShardOK whether the name declared one at all. The header's `split.*` keys
	// are in Shape and internal/models reconciles the two.
	Shard   Shard
	ShardOK bool
}

// SnapshotEntry is one `snapshots/<commit>` directory.
type SnapshotEntry struct {
	// Revision is the directory name, which IS `models.revision`.
	Revision string
	// Dir is the absolute snapshot directory — `models.snapshot_dir`.
	Dir string
	// RefNames are the `refs/` entries whose contents equal Revision, sorted.
	// A snapshot no ref points at has none and is shown by its short sha.
	RefNames []string
	// Files are every file under Dir, recursively, in sorted order.
	Files []FileEntry
}

// RepoEntry is one `models--{org}--{name}` directory.
type RepoEntry struct {
	// RepoID is the folder name mapped back to `{org}/{name}`.
	RepoID string
	// Folder is the directory name itself.
	Folder string
	// Dir is the absolute repo directory.
	Dir string
	// Snapshots are its snapshot directories, sorted by revision. N snapshot
	// directories produce N distinct models rows (section 7.2).
	Snapshots []SnapshotEntry
}

// Stray is a file in the cache root that belongs to no model: a GGUF outside a
// snapshot directory, a blob nothing links to, or a broken link.
type Stray struct {
	Path      string
	SizeBytes int64
	Reason    model.StrayReason
}

// Result is one walk of one hub directory.
type Result struct {
	Hub    string
	Repos  []RepoEntry
	Strays []Stray

	DirsSeen   int
	FilesSeen  int
	BytesTotal int64

	// Errors are the paths the walk could not read — a permission-denied repo
	// directory on a shared cache, most often. A scan reports them and keeps
	// going: refusing to scan a 300-model cache because one directory is
	// unreadable would be a worse answer than scanning 299.
	Errors []error
}

// ScanOptions tunes a walk.
type ScanOptions struct {
	// KeepMetadata retains the full GGUF key/value table on every parsed file.
	// Off by default: a boot scan of a large cache would otherwise hold every
	// model's tokenizer metadata in memory at once, and section 3.7's metadata
	// endpoint re-reads the one file it is asked about.
	KeepMetadata bool
	// Progress is called at most every ProgressEvery. Nil disables reporting.
	Progress func(Progress)
	// Now supplies the clock the progress throttle uses. Nil uses time.Now.
	Now func() time.Time
	// ParseOptions are passed to internal/gguf. The zero value is the parser's
	// own defaults, which bound the header and elide the tokenizer arrays.
	ParseOptions []gguf.Option
}

// Scan walks a hub directory and reports every repository, snapshot, file and
// stray in it. It makes NO network calls: card metadata is fetched lazily when a
// model is opened, so a 300-model scan works offline.
//
// ctx cancellation stops the walk and returns what was found so far together
// with ctx.Err(), because a canceled scan's partial result is still what the job
// row's counters should record.
func Scan(ctx context.Context, hub string, opts ScanOptions) (Result, error) {
	l := NewLayout(hub)
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	res := Result{Hub: l.Hub}
	entries, err := os.ReadDir(l.Hub)
	if err != nil {
		if os.IsNotExist(err) {
			// A registered root whose directory is gone is not a scan failure:
			// it is a disk that is not mounted, and the reconciliation that
			// follows marks its models `missing` rather than deleting them.
			return res, nil
		}
		return res, err
	}

	var (
		lastReport time.Time
		report     = func(current string, force bool) {
			if opts.Progress == nil {
				return
			}
			if t := now(); force || t.Sub(lastReport) >= ProgressEvery {
				lastReport = t
				opts.Progress(Progress{
					DirsSeen: res.DirsSeen, FilesSeen: res.FilesSeen,
					ModelsSeen: countSnapshots(res.Repos), BytesTotal: res.BytesTotal,
					Current: current,
				})
			}
		}
	)

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		name := e.Name()
		if name == LocksDirName {
			continue
		}
		if !e.IsDir() {
			// A loose file directly in the hub directory. Only a GGUF is worth
			// reporting: everything else there is another tool's bookkeeping.
			if IsGGUF(name) {
				res.Strays = append(res.Strays, stray(filepath.Join(l.Hub, name), model.StrayOutsideSnapshot))
			}
			continue
		}
		repoID, ok := RepoIDFromFolder(name)
		if !ok {
			continue
		}
		res.DirsSeen++
		report(name, false)

		repo, err := scanRepo(ctx, l, repoID, name, opts, &res)
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Repos = append(res.Repos, repo)
	}

	sort.Slice(res.Repos, func(i, j int) bool { return res.Repos[i].RepoID < res.Repos[j].RepoID })
	sort.Slice(res.Strays, func(i, j int) bool { return res.Strays[i].Path < res.Strays[j].Path })
	report("", true)
	return res, nil
}

// scanRepo walks one `models--…` directory: its snapshots, its refs, and the
// blobs nothing links to.
func scanRepo(ctx context.Context, l Layout, repoID, folder string,
	opts ScanOptions, res *Result) (RepoEntry, error) {

	repo := RepoEntry{RepoID: repoID, Folder: folder, Dir: l.RepoDir(repoID)}
	refs := readRefs(l, repoID)

	// linked accumulates every blob any snapshot in this repo points at, which
	// is what makes an orphan blob detectable — the same refcount D28's delete
	// preview computes, gathered here for free because the walk is already
	// resolving every link.
	linked := map[string]struct{}{}

	snapDirs, err := os.ReadDir(l.SnapshotsDir(repoID))
	if err != nil && !os.IsNotExist(err) {
		return repo, err
	}
	for _, sd := range snapDirs {
		if err := ctx.Err(); err != nil {
			return repo, err
		}
		if !sd.IsDir() {
			continue
		}
		res.DirsSeen++
		snap := SnapshotEntry{
			Revision: sd.Name(),
			Dir:      l.SnapshotDir(repoID, sd.Name()),
			RefNames: refs[sd.Name()],
		}
		files, err := scanSnapshot(ctx, snap.Dir, l.BlobsDir(repoID), opts, res, linked)
		if err != nil {
			if ctx.Err() != nil {
				return repo, err
			}
			res.Errors = append(res.Errors, err)
			continue
		}
		snap.Files = files
		repo.Snapshots = append(repo.Snapshots, snap)
	}
	sort.Slice(repo.Snapshots, func(i, j int) bool {
		return repo.Snapshots[i].Revision < repo.Snapshots[j].Revision
	})

	// Orphan blobs: content no snapshot references. An `.incomplete` file is
	// NOT one — it is a transfer in progress, possibly another tool's (D26), and
	// section 7.4's startup sweep is the only thing entitled to remove one.
	blobs, err := os.ReadDir(l.BlobsDir(repoID))
	if err != nil && !os.IsNotExist(err) {
		res.Errors = append(res.Errors, err)
	}
	for _, b := range blobs {
		if b.IsDir() || strings.HasSuffix(b.Name(), IncompleteSuffix) {
			continue
		}
		if _, ok := linked[b.Name()]; ok {
			continue
		}
		res.Strays = append(res.Strays,
			stray(filepath.Join(l.BlobsDir(repoID), b.Name()), model.StrayOrphanBlob))
	}
	return repo, nil
}

// scanSnapshot walks one snapshot directory recursively.
func scanSnapshot(ctx context.Context, dir, blobsDir string, opts ScanOptions,
	res *Result, linked map[string]struct{}) ([]FileEntry, error) {

	var out []FileEntry
	// counted keeps BytesTotal honest across snapshots that share a blob: the
	// same content linked from three revisions occupies the disk once, and a
	// disk-usage number that triples it is worse than no number.
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			res.Errors = append(res.Errors, err)
			return nil
		}
		if d.IsDir() {
			if path != dir {
				res.DirsSeen++
			}
			return nil
		}

		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		res.FilesSeen++

		fe := FileEntry{Name: filepath.ToSlash(rel), Path: path, IsGGUF: IsGGUF(rel)}
		fe.Shard, fe.ShardOK = ParseShardName(filepath.Base(rel))

		// Resolve the link before stat: a broken link is a fact worth reporting
		// and a Stat through it would only say ENOENT.
		if d.Type()&fs.ModeSymlink != 0 {
			target, lerr := os.Readlink(path)
			if lerr == nil {
				abs := target
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(filepath.Dir(path), target)
				}
				abs = filepath.Clean(abs)
				if filepath.Dir(abs) == filepath.Clean(blobsDir) {
					fe.BlobPath = abs
					fe.Etag = filepath.Base(abs)
				}
			}
		}

		st, serr := os.Stat(path)
		if serr != nil {
			fe.Broken = true
			res.Strays = append(res.Strays, Stray{Path: path, Reason: model.StrayBrokenSymlink})
			out = append(out, fe)
			return nil
		}
		fe.SizeBytes = st.Size()
		fe.BytesOnDisk = allocatedBytes(st, fe.SizeBytes)

		if fe.Etag != "" {
			if _, dup := linked[fe.Etag]; !dup {
				linked[fe.Etag] = struct{}{}
				res.BytesTotal += fe.BytesOnDisk
			}
		} else {
			// Copy mode (F17) or a plain file another tool wrote: it is its own
			// content, so it always counts.
			res.BytesTotal += fe.BytesOnDisk
		}

		if fe.IsGGUF {
			f, perr := gguf.ParseFile(path, opts.ParseOptions...)
			if perr != nil {
				fe.ParseErr = perr
				res.Strays = append(res.Strays,
					Stray{Path: path, SizeBytes: fe.SizeBytes, Reason: model.StrayUnparsable})
			} else {
				sh := f.Shape()
				fe.Shape = &sh
				if opts.KeepMetadata {
					fe.KV = f.KV.All()
				}
			}
		}
		out = append(out, fe)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readRefs maps a commit sha to the ref names that point at it.
//
// It is the ONLY thing `refs/` is read for (section 7.2): `models.ref_name` is a
// DISPLAY field. A snapshot no ref points at keeps a NULL ref_name and is shown
// by its short sha, which is the correct rendering of a revision the user pinned
// by hand.
func readRefs(l Layout, repoID string) map[string][]string {
	out := map[string][]string{}
	root := l.RefsDir(repoID)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable refs/ costs a display field, nothing more
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		sha := strings.TrimSpace(string(b))
		if sha == "" {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out[sha] = append(out[sha], filepath.ToSlash(rel))
		return nil
	})
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func stray(path string, reason model.StrayReason) Stray {
	s := Stray{Path: path, Reason: reason}
	if st, err := os.Stat(path); err == nil {
		s.SizeBytes = st.Size()
	}
	return s
}

func countSnapshots(repos []RepoEntry) int {
	n := 0
	for _, r := range repos {
		n += len(r.Snapshots)
	}
	return n
}
