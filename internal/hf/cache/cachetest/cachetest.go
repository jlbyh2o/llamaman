// Package cachetest builds a Hugging Face hub cache on disk for tests.
//
// The checked-in tree under `internal/hf/cache/testdata/hub` pins the PATH
// layout and is deliberately inert — nothing in it parses. A scan test needs the
// other half: real GGUF bytes whose headers say something specific, and real
// relative symlinks whose targets resolve on the machine running the test.
// Neither survives being committed usefully — a symlink's target has to exist,
// and a GGUF fixture whose expected values live in another directory is a
// fixture nobody can read — so this builder writes the tree into a temp
// directory instead, from the same `ggufbuild` helper internal/gguf is tested
// with.
//
// It is an ordinary package rather than a `_test.go` file for the reason
// ggufbuild is: internal/models' reconciliation tests need the same tree, and
// building it where it is used beats reaching across the tree for a fixture
// whose expected values live somewhere else. Nothing in the product imports it.
package cachetest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
)

// Hub is a hub directory under construction.
type Hub struct {
	// Dir is the hub directory itself — what `hf_cache_roots.path` would hold.
	Dir string
	t   *testing.T
}

// New creates an empty hub directory in a fresh temp directory.
func New(t *testing.T) *Hub {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the hub directory: %v", err)
	}
	return &Hub{Dir: dir, t: t}
}

// At binds an existing directory as a hub, for a test that wants the hub
// somewhere specific — under a state directory, say, for the rule-4 detection
// case.
func At(t *testing.T, dir string) *Hub {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create the hub directory: %v", err)
	}
	return &Hub{Dir: dir, t: t}
}

// Layout is the path builder bound to this hub.
func (h *Hub) Layout() cache.Layout { return cache.NewLayout(h.Dir) }

// Blob writes content into `<repo>/blobs/<etag>` and returns the path. The etag
// is the caller's: for an LFS object it would be the sha256 hex, and a test that
// cares about the digest passes one it computed.
func (h *Hub) Blob(repoID, etag string, content []byte) string {
	h.t.Helper()
	l := h.Layout()
	if err := os.MkdirAll(l.BlobsDir(repoID), 0o755); err != nil {
		h.t.Fatalf("create the blobs directory: %v", err)
	}
	path := l.BlobPath(repoID, etag)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		h.t.Fatalf("write blob %s: %v", etag, err)
	}
	return path
}

// Link creates the snapshot entry `snapshots/<revision>/<name>` as the RELATIVE
// symlink `../../blobs/<etag>` — the same body `huggingface_hub` writes, which
// is what keeps the cache movable.
func (h *Hub) Link(repoID, revision, name, etag string) string {
	h.t.Helper()
	l := h.Layout()
	path := l.SnapshotFile(repoID, revision, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatalf("create the snapshot directory: %v", err)
	}
	if err := os.Symlink(cache.LinkTarget(name, etag), path); err != nil {
		h.t.Fatalf("link %s: %v", name, err)
	}
	return path
}

// File writes a snapshot entry as a PLAIN FILE rather than a symlink — the
// copy-mode fallback of F17, on a filesystem that refuses links.
func (h *Hub) File(repoID, revision, name string, content []byte) string {
	h.t.Helper()
	path := h.Layout().SnapshotFile(repoID, revision, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatalf("create the snapshot directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// Ref writes `refs/<name>` containing a commit sha. It is read for exactly one
// purpose (section 7.2): filling the DISPLAY field `models.ref_name`.
func (h *Hub) Ref(repoID, name, sha string) {
	h.t.Helper()
	path := h.Layout().RefPath(repoID, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatalf("create the refs directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(sha+"\n"), 0o644); err != nil {
		h.t.Fatalf("write ref %s: %v", name, err)
	}
}

// NoExist writes a `.no_exist/<revision>/<name>` marker — the negative cache the
// scan must skip entirely.
func (h *Hub) NoExist(repoID, revision, name string) {
	h.t.Helper()
	dir := h.Layout().NoExistDir(repoID, revision)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("create the .no_exist directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		h.t.Fatalf("write the .no_exist marker: %v", err)
	}
}

// Incomplete writes `blobs/<etag>.incomplete`, a transfer in progress. It is
// NOT a stray: it may belong to a concurrent `hf download` (D26), and only
// section 7.4's startup sweep is entitled to remove one.
func (h *Hub) Incomplete(repoID, etag string, content []byte) string {
	h.t.Helper()
	l := h.Layout()
	if err := os.MkdirAll(l.BlobsDir(repoID), 0o755); err != nil {
		h.t.Fatalf("create the blobs directory: %v", err)
	}
	path := l.IncompletePath(repoID, etag)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		h.t.Fatalf("write the incomplete blob: %v", err)
	}
	return path
}

// BreakLink removes the blob a snapshot entry points at, leaving a dangling
// symlink — the `broken_symlink` stray, and the state a cache in which somebody
// ran `rm blobs/*` is in.
func (h *Hub) BreakLink(repoID, etag string) {
	h.t.Helper()
	if err := os.Remove(h.Layout().BlobPath(repoID, etag)); err != nil {
		h.t.Fatalf("remove blob %s: %v", etag, err)
	}
}

// GGUF describes a synthetic model file to write into the cache.
type GGUF struct {
	// Arch is `general.architecture`.
	Arch string
	// Layers is the transformer block count; four tensors are written per
	// block, so the per-layer bucketing has real work to do.
	Layers int
	// Embd and FF size those tensors.
	Embd, FF uint64
	// Quant is written as `general.file_type`'s name via a dominant tensor
	// type. Empty leaves the file to be labeled by its tensors.
	FileType uint32
	// Vision marks the file a multimodal projector by METADATA rather than by
	// name (`clip.has_vision_encoder`), which is the stronger of section 7.2's
	// two classification signals.
	Vision bool
	// Pooling writes `{arch}.pooling_type`, which classifies an embedding model
	// whatever its architecture string says.
	Pooling *uint32
	// SplitNo and SplitCount write the `split.*` shard metadata. SplitCount 0
	// writes neither.
	SplitNo, SplitCount uint16
	// Complete writes the tensor DATA as well as the header, so File.Complete
	// reports true and the file's size is what its index describes.
	//
	// It is off by default and that is deliberate: every fixture here is a
	// truncated header of a few kilobytes, because the scan reads nothing but
	// the header and a q4_K layer's real data is a hundred kilobytes per
	// tensor. A test about SIZES turns it on; a test about layout, grouping or
	// classification does not need to write half a megabyte to say what it
	// means.
	Complete bool
	// Extra are additional metadata pairs, for a test that needs one key this
	// struct does not name.
	Extra map[string]ggufbuild.Val
}

// Model writes one synthetic GGUF into the cache as a blob plus a relative
// snapshot symlink, and returns the blob's etag.
//
// The etag is derived from the file name rather than hashed, because these
// fixtures are about the LAYOUT and the grouping: a test that needs a real
// sha256 (the verify path) computes and passes one through Blob and Link.
func (h *Hub) Model(repoID, revision, name string, spec GGUF) string {
	h.t.Helper()
	etag := FakeEtag(repoID, revision, name)
	h.Blob(repoID, etag, BuildGGUF(spec))
	h.Link(repoID, revision, name, etag)
	return etag
}

// BuildGGUF renders a spec into GGUF bytes.
//
// The default is a truncated header — valid, complete as a header, and a few
// kilobytes — which is what a scan reads and all any layout test needs. Set
// GGUF.Complete for the tensor data too.
func BuildGGUF(spec GGUF) []byte {
	arch := spec.Arch
	if arch == "" {
		arch = "llama"
	}
	b := ggufbuild.New(arch)

	layers := spec.Layers
	if layers == 0 {
		layers = 2
	}
	// The defaults are multiples of 256 because the layer tensors below are
	// q4_K, whose block size is 256: a dimension that is not a multiple of it
	// is a header the parser correctly refuses, and a fixture that cannot be
	// parsed would prove nothing about the scan.
	embd := spec.Embd
	if embd == 0 {
		embd = 256
	}
	ff := spec.FF
	if ff == 0 {
		ff = 256
	}

	b.Set(arch+".block_count", ggufbuild.U32(uint32(layers)))
	b.Set(arch+".context_length", ggufbuild.U32(4096))
	b.Set(arch+".embedding_length", ggufbuild.U32(uint32(embd)))
	b.Set(arch+".feed_forward_length", ggufbuild.U32(uint32(ff)))
	b.Set(arch+".attention.head_count", ggufbuild.U32(8))
	b.Set(arch+".attention.head_count_kv", ggufbuild.U32(2))
	b.Set("tokenizer.ggml.model", ggufbuild.Str("gpt2"))
	b.Set("tokenizer.ggml.tokens", ggufbuild.Strs("a", "b", "c", "d"))

	if spec.FileType != 0 {
		b.Set("general.file_type", ggufbuild.U32(spec.FileType))
	}
	if spec.Vision {
		b.Set("clip.has_vision_encoder", ggufbuild.Bool(true))
	}
	if spec.Pooling != nil {
		b.Set(arch+".pooling_type", ggufbuild.U32(*spec.Pooling))
	}
	if spec.SplitCount > 0 {
		b.Set("split.no", ggufbuild.U16(spec.SplitNo))
		b.Set("split.count", ggufbuild.U16(spec.SplitCount))
	}
	for k, v := range spec.Extra {
		b.Set(k, v)
	}

	b.Layers(layers, embd, ff, gguf.TypeQ4_K)
	if spec.Complete {
		return b.Full()
	}
	return b.Header()
}

// FakeEtag is a deterministic, path-shaped blob name. It is not a digest and is
// not meant to look like one: a test asserting a digest passes its own.
func FakeEtag(repoID, revision, name string) string {
	s := repoID + "@" + revision + "/" + name
	r := strings.NewReplacer("/", "-", "@", "-", ".", "-", "_", "-")
	return "etag-" + r.Replace(s)
}
