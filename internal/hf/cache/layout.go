package cache

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The hub cache layout (DESIGN section 7.2). It is `huggingface_hub`'s, which is
// the entire point — SPEC section 3.2 promises one shared cache, and a promise
// about a directory layout is only as true as the strings that build it:
//
//	<hub>/
//	├── .locks/<repo_folder>/<etag>.lock       flock — the SAME lock huggingface_hub takes (D27)
//	└── models--{org}--{name}/
//	    ├── refs/<branch>                      file containing the commit sha
//	    ├── blobs/<etag>                       content; for LFS objects <etag> == sha256 hex
//	    ├── blobs/<etag>.incomplete            in-progress download, resumable by either tool (D26)
//	    ├── snapshots/<commit>/<path>          relative symlink -> ../../blobs/<etag>
//	    └── .no_exist/<commit>/<path>          negative cache; we read it, never write it
//
// Every path in this product is built here and nowhere else, so there is exactly
// one place to be right and exactly one place a fixture test can pin.

// RepoIDSeparator is `huggingface_hub`'s REPO_ID_SEPARATOR: the token that joins
// the repo type and the parts of the repo id into one directory name. It is `--`
// and not `/` because the result has to be a single path segment.
const RepoIDSeparator = "--"

// RepoFolderPrefix is the `repo_type` half of a repo folder name, pluralized the
// way `huggingface_hub.repo_folder_name` pluralizes it. Only model repositories
// are in v1 scope, so the prefix is a constant rather than a parameter — a
// dataset or a space in this cache is not something Llama Man reads.
const RepoFolderPrefix = "models"

// Directory and file names inside a repo directory.
const (
	LocksDirName      = ".locks"
	BlobsDirName      = "blobs"
	SnapshotsDirName  = "snapshots"
	RefsDirName       = "refs"
	NoExistDirName    = ".no_exist"
	LockSuffix        = ".lock"
	IncompleteSuffix  = ".incomplete"
	GGUFExtension     = ".gguf"
	hubDirNameSuffix  = "hub"
	mmprojNamePrefix  = "mmproj"
	huggingfaceSubdir = "huggingface"
)

// Permissions. Section 7.2's write path is explicit about both: "Files 0644,
// directories 0755: other tools running as the same user must be able to read
// them", and the lock file is 0644 for the same reason one directory up.
const (
	FileMode os.FileMode = 0o644
	DirMode  os.FileMode = 0o755
)

// RepoFolderName renders a repo id as its cache directory name:
// `bartowski/Qwen3-8B-GGUF` becomes `models--bartowski--Qwen3-8B-GGUF`.
//
// This is `huggingface_hub.repo_folder_name` verbatim — every `/` in the repo id
// becomes `--`, so a repo id with no organization (`gpt2`) yields
// `models--gpt2`. Nothing else about the id is escaped, because nothing else
// needs to be: a repo id is already restricted to characters that are legal in a
// path segment.
func RepoFolderName(repoID string) string {
	parts := append([]string{RepoFolderPrefix}, strings.Split(strings.Trim(repoID, "/"), "/")...)
	return strings.Join(parts, RepoIDSeparator)
}

// RepoIDFromFolder is the inverse: `models--bartowski--Qwen3-8B-GGUF` becomes
// `bartowski/Qwen3-8B-GGUF`. It reports false for a directory that is not a
// model repo folder — a `datasets--…` left by another tool, or anything else
// that happens to sit in the hub directory.
//
// The mapping is not injective in general: a repo id containing a literal `--`
// would round-trip wrong. Hugging Face does not allow one in a namespace or a
// repo name, and `huggingface_hub` itself makes the same assumption when it
// reports a scanned cache, so this inverse is exactly as correct as the tool
// whose layout it reads.
func RepoIDFromFolder(folder string) (string, bool) {
	parts := strings.Split(folder, RepoIDSeparator)
	if len(parts) < 2 || parts[0] != RepoFolderPrefix {
		return "", false
	}
	for _, p := range parts[1:] {
		if p == "" {
			return "", false
		}
	}
	return strings.Join(parts[1:], "/"), true
}

// Layout binds a hub directory. Every method is a pure string operation: nothing
// here touches the filesystem, so the path table can be asserted against a
// fixture tree without one (DESIGN section 15).
//
// Hub is the hub directory ITSELF and need not end in `/hub` — rule 1 of section
// 7.2 produces one that does not, and no code in this package may assume the
// suffix.
type Layout struct {
	Hub string
}

// NewLayout binds hub, cleaning it so two spellings of the same directory build
// the same paths.
func NewLayout(hub string) Layout { return Layout{Hub: filepath.Clean(hub)} }

// LocksDir is `<hub>/.locks`, the sibling of every repo directory.
func (l Layout) LocksDir() string { return filepath.Join(l.Hub, LocksDirName) }

// RepoLocksDir is `<hub>/.locks/<repo_folder>`.
func (l Layout) RepoLocksDir(repoID string) string {
	return filepath.Join(l.LocksDir(), RepoFolderName(repoID))
}

// LockPath is D27's path, and it is the whole of SPEC section 3.2's
// one-shared-cache promise on the write side:
//
//	<hub>/.locks/<repo_folder>/<etag>.lock
//
// It is built HERE and nowhere else. Locking the `.incomplete` file, or a
// `.lock` inside the repo directory, interlocks with nothing — the two tools
// would each hold a lock the other never looks at, and the failure is silent
// corruption of a blob one of them is still writing.
//
// The path is only half the contract; the other half is the primitive, and it is
// `flock(2)`. See Acquire.
func (l Layout) LockPath(repoID, etag string) string {
	return filepath.Join(l.RepoLocksDir(repoID), etag+LockSuffix)
}

// LockPath is the package-level spelling of the same function, for a caller that
// has a hub directory rather than a Layout. D27 says the path is built by
// exactly one function; this one and the method are the same code, so there is
// still exactly one.
func LockPath(hub, repoID, etag string) string {
	return NewLayout(hub).LockPath(repoID, etag)
}

// RepoDir is `<hub>/models--{org}--{name}`.
func (l Layout) RepoDir(repoID string) string {
	return filepath.Join(l.Hub, RepoFolderName(repoID))
}

// BlobsDir is `<repo>/blobs`.
func (l Layout) BlobsDir(repoID string) string {
	return filepath.Join(l.RepoDir(repoID), BlobsDirName)
}

// BlobPath is `<repo>/blobs/<etag>`. For an LFS object `<etag>` is the sha256
// hex, which is why a blob that is already there at the right size can be linked
// without downloading anything (section 7.2 write path step 2).
func (l Layout) BlobPath(repoID, etag string) string {
	return filepath.Join(l.BlobsDir(repoID), etag)
}

// IncompletePath is `<repo>/blobs/<etag>.incomplete` — the partial file D26
// keeps byte-for-byte compatible with `huggingface_hub`'s own resume semantics.
func (l Layout) IncompletePath(repoID, etag string) string {
	return l.BlobPath(repoID, etag) + IncompleteSuffix
}

// SnapshotsDir is `<repo>/snapshots`.
func (l Layout) SnapshotsDir(repoID string) string {
	return filepath.Join(l.RepoDir(repoID), SnapshotsDirName)
}

// SnapshotDir is `<repo>/snapshots/<commit>`. `<commit>` is the resolved sha and
// never a branch name: section 7.2 takes `models.revision` from this directory
// name, because it is the only source that is right for every directory the walk
// visits.
func (l Layout) SnapshotDir(repoID, revision string) string {
	return filepath.Join(l.SnapshotsDir(repoID), revision)
}

// SnapshotFile is `<repo>/snapshots/<commit>/<path>`, where path is the file's
// path INSIDE the repository and may contain directories.
func (l Layout) SnapshotFile(repoID, revision, path string) string {
	return filepath.Join(l.SnapshotDir(repoID, revision), filepath.FromSlash(path))
}

// RefsDir is `<repo>/refs`.
func (l Layout) RefsDir(repoID string) string {
	return filepath.Join(l.RepoDir(repoID), RefsDirName)
}

// RefPath is `<repo>/refs/<branch>`, a file whose contents are the commit sha.
// A ref name may itself contain slashes (`refs/pr/3`), which is why this is a
// Join over the split rather than a single segment.
func (l Layout) RefPath(repoID, ref string) string {
	return filepath.Join(l.RefsDir(repoID), filepath.FromSlash(ref))
}

// NoExistDir is `<repo>/.no_exist/<commit>` — `huggingface_hub`'s negative
// cache. Section 7.2: "we read it, never write it", and the scan skips it
// entirely, because a marker for a file that does not exist is not a snapshot.
func (l Layout) NoExistDir(repoID, revision string) string {
	return filepath.Join(l.RepoDir(repoID), NoExistDirName, revision)
}

// LinkTarget is the RELATIVE symlink body that a snapshot entry points at:
// `../../blobs/<etag>` for a file at the snapshot root, one `../` deeper per
// directory in the file's path. Relative is not a style choice — it is what
// keeps the cache movable, and it is what `huggingface_hub` writes, so a tree
// this product creates survives being copied to another disk exactly as one the
// Python tool created does.
func LinkTarget(path, etag string) string {
	depth := 2 + strings.Count(strings.Trim(filepath.ToSlash(path), "/"), "/")
	parts := make([]string, 0, depth+2)
	for range depth {
		parts = append(parts, "..")
	}
	parts = append(parts, BlobsDirName, etag)
	return filepath.Join(parts...)
}

// HFHome projects a hub directory back to an `HF_HOME`: `<X>/hub` becomes `<X>`,
// and anything else has none.
//
// It reports false rather than inventing a path, and section 7.2a is explicit
// that this is a COURTESY projection: `settings['hf.hub_dir']` is the
// authoritative value, `settings['hf.home']` is empty whenever the suffix is
// absent, and rule 1 of the detection chain routinely produces a hub directory
// that has no `HF_HOME` at all.
func HFHome(hub string) (string, bool) {
	clean := filepath.Clean(hub)
	dir, base := filepath.Split(clean)
	if base != hubDirNameSuffix || dir == "" {
		return "", false
	}
	parent := filepath.Clean(dir)
	if parent == clean {
		return "", false
	}
	return parent, true
}

// shardPattern matches `huggingface_hub`'s and llama.cpp's shard convention:
// `<base>-00001-of-00003.gguf`. The widths are fixed at five digits by the
// producer (`llama-gguf-split`), but this accepts any run of digits so a file
// written by a tool that chose a different width is still grouped rather than
// silently treated as three unrelated models.
var shardPattern = regexp.MustCompile(`^(.*)-(\d+)-of-(\d+)\.gguf$`)

// Shard is one file's place in a sharded GGUF set (section 7.3).
type Shard struct {
	// Base is the file name with the shard suffix removed — the identity every
	// shard of one set shares.
	Base string
	// Index is 1-based, matching the file name and `split.no + 1`.
	Index int
	// Total is the set size, matching `split.count`.
	Total int
}

// ParseShardName splits `Model-Q4_K_M-00002-of-00005.gguf` into its base and its
// place in the set. It reports false for an unsharded name, whose caller then
// treats the file as a set of one.
//
// This is the FILE NAME half of section 7.2's grouping rule. The other half is
// the `split.count`/`split.no` metadata inside the header, which internal/models
// cross-checks: a producer that wrote the metadata but not the suffix, or the
// reverse, still groups correctly.
func ParseShardName(filename string) (Shard, bool) {
	m := shardPattern.FindStringSubmatch(filename)
	if m == nil {
		return Shard{}, false
	}
	idx, err := strconv.Atoi(m[2])
	if err != nil || idx < 1 {
		return Shard{}, false
	}
	total, err := strconv.Atoi(m[3])
	if err != nil || total < 1 || idx > total {
		return Shard{}, false
	}
	return Shard{Base: m[1], Index: idx, Total: total}, true
}

// ShardName renders the inverse, so a caller expanding a quant into its file set
// (section 7.3) builds names that this package's own parser accepts.
func ShardName(base string, index, total int) string {
	return base + "-" + pad5(index) + "-of-" + pad5(total) + GGUFExtension
}

func pad5(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 5 {
		s = "0" + s
	}
	return s
}

// IsGGUF reports whether a file name is a GGUF by extension. The scan uses it to
// decide what to parse; a `.gguf` that does not parse becomes a stray with
// reason `unparsable` rather than being ignored, so this test is deliberately
// only about the name.
func IsGGUF(name string) bool {
	return strings.EqualFold(filepath.Ext(name), GGUFExtension)
}

// LooksLikeMmproj reports whether a file name follows the community convention
// for a multimodal projector (`mmproj-model-f16.gguf`, `mmproj-Qwen2-VL-f32.gguf`).
//
// It is one of two signals, and the weaker one: section 7.2 classifies on the
// name OR on `clip.has_vision_encoder` in the header, because the convention is
// a convention and the metadata is a fact.
func LooksLikeMmproj(name string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(name)), mmprojNamePrefix)
}
