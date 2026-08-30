package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The downloader's integration tests (DESIGN section 15).
//
// "HF client and downloader: an `httptest` server serving canned `/api/models`,
// `/tree` and a Range-capable `resolve`, plus adversarial modes — ignore the
// range, truncate mid-stream, wrong ETag… Resume correctness is asserted by
// hashing the final file."
//
// Everything below runs against a REAL SQLite database and a REAL job queue.
// That is not thoroughness for its own sake: section 2.3a's promise is that the
// job row and the domain row move in one transaction, and the only way to assert
// it is to let the queue write both and then read the columns back.

const testRepo = "bartowski/Test-Model-GGUF"
const testCommit = "4f0ac1c0a1f0ee0b1c2d3e4f5a6b7c8d9e0f1a2b"

// -----------------------------------------------------------------------------
// The origin
// -----------------------------------------------------------------------------

// originFile is one file the fake Hub serves.
type originFile struct {
	content []byte
	// sha is the sha256 hex, which for an LFS object IS the blob name — so the
	// digest check of section 7.2 step 5 is exercised for real.
	sha string
}

// origin is a Range-capable Hugging Face, with the adversarial modes section 15
// names. Every mode is a field rather than a subclass so one server can be
// configured per test and the failure it injects is visible at the call site.
type origin struct {
	t     *testing.T
	files map[string]*originFile

	mu sync.Mutex
	// truncateAfter cuts the first GET of a file short after this many bytes of
	// the body, simulating a dropped connection mid-stream.
	truncateAfter map[string]int
	// etag is the validator the origin currently issues. rotateEtag changes it
	// after the first response, which is what makes a second request's
	// `If-Range` fail to match.
	etag       string
	rotateEtag bool
	// gated makes every resolve answer 403 GatedRepo while the tree keeps
	// working — which is exactly how a gated repository behaves (section 7.1).
	gated bool
	// inflate overrides the `lfs.size` the tree reports, so the disk guard can
	// be exercised against a real statfs without writing a terabyte.
	inflate int64
	// commit overrides the `x-repo-commit` header, which is what the create path
	// turns into a snapshot DIRECTORY NAME. Empty means testCommit.
	commit string

	requests []recordedRequest
}

type recordedRequest struct {
	method  string
	path    string
	rng     string
	ifRange string
}

func newOrigin(t *testing.T) *origin {
	return &origin{
		t: t, files: map[string]*originFile{},
		truncateAfter: map[string]int{}, etag: `"v1"`,
	}
}

// add registers a file of n deterministic bytes.
func (o *origin) add(name string, n int) *originFile {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte('a' + (i % 26))
	}
	sum := sha256.Sum256(body)
	f := &originFile{content: body, sha: hex.EncodeToString(sum[:])}
	o.files[name] = f
	return f
}

func (o *origin) record(r *http.Request) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(o.requests)
	o.requests = append(o.requests, recordedRequest{
		method: r.Method, path: r.URL.Path,
		rng: r.Header.Get("Range"), ifRange: r.Header.Get("If-Range"),
	})
	return n
}

func (o *origin) gets(path string) []recordedRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []recordedRequest
	for _, r := range o.requests {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, path) {
			out = append(out, r)
		}
	}
	return out
}

func (o *origin) currentEtag(n int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rotateEtag && n > 0 {
		return `"v2"`
	}
	return o.etag
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := o.record(r)

	if strings.HasPrefix(r.URL.Path, "/api/models/") && strings.Contains(r.URL.Path, "/tree/") {
		o.serveTree(w)
		return
	}
	name, ok := o.resolveName(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f := o.files[name]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	if o.gated {
		w.Header().Set("X-Error-Code", "GatedRepo")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	etag := o.currentEtag(n)
	w.Header().Set("X-Linked-Etag", `"`+f.sha+`"`)
	w.Header().Set("X-Linked-Size", strconv.Itoa(len(f.content)))
	commit := o.commit
	if commit == "" {
		commit = testCommit
	}
	w.Header().Set("X-Repo-Commit", commit)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Etag", etag)

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(f.content)))
		w.WriteHeader(http.StatusOK)
		return
	}

	body := f.content
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" {
		start, err := rangeStart(rng)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if start >= int64(len(f.content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		// Real `If-Range` semantics: a validator that does not match the
		// object's current one means the client's partial is stale, and the
		// origin answers with the WHOLE file rather than a range.
		if ir := r.Header.Get("If-Range"); ir != "" && ir != etag {
			status = http.StatusOK
		} else {
			body = f.content[start:]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
				start, len(f.content)-1, len(f.content)))
			status = http.StatusPartialContent
		}
	}

	o.mu.Lock()
	cut, truncate := o.truncateAfter[name]
	if truncate {
		delete(o.truncateAfter, name)
	}
	o.mu.Unlock()

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if truncate && cut < len(body) {
		_, _ = w.Write(body[:cut])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return
	}
	_, _ = w.Write(body)
}

// resolveName reads `/{org}/{name}/resolve/{rev}/{file}`.
func (o *origin) resolveName(path string) (string, bool) {
	_, after, ok := strings.Cut(path, "/resolve/")
	if !ok {
		return "", false
	}
	_, file, ok := strings.Cut(after, "/")
	return file, ok
}

func (o *origin) serveTree(w http.ResponseWriter) {
	type lfs struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	}
	type entry struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *lfs   `json:"lfs"`
	}
	var out []entry
	for name, f := range o.files {
		size := int64(len(f.content))
		if o.inflate > 0 {
			size = o.inflate
		}
		out = append(out, entry{
			Type: "file", Path: name,
			// The POINTER size on the top-level field, the true size inside
			// `lfs`. A tree that reported the true size in both places would
			// let a reader that ignores `lfs` pass this suite while failing on
			// the real Hub.
			Size: 135, OID: f.sha,
			LFS: &lfs{OID: f.sha, Size: size},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func rangeStart(v string) (int64, error) {
	_, after, ok := strings.Cut(v, "=")
	if !ok {
		return 0, errors.New("no =")
	}
	first, _, _ := strings.Cut(after, "-")
	return strconv.ParseInt(first, 10, 64)
}

// -----------------------------------------------------------------------------
// The harness
// -----------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	db     *store.Store
	queue  *jobs.Queue
	svc    *Service
	origin *origin
	server *httptest.Server
	hub    string
	rootID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	hub := filepath.Join(dir, "hf-cache", "hub")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatalf("create the hub directory: %v", err)
	}

	rootID := store.NewID(time.Now())
	if err := db.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return db.InsertCacheRoot(ctx, tx, model.CacheRoot{
			ID: rootID, Path: hub, IsPrimary: true, Writable: true, SymlinksOK: true,
			CreatedAt: time.Now().UnixMilli(),
		})
	}); err != nil {
		t.Fatalf("seed the cache root: %v", err)
	}

	o := newOrigin(t)
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)

	client := hf.New(hf.Options{
		Endpoint: srv.URL, UserAgent: "llamaman/test", CacheTTL: -1,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})

	q, err := jobs.New(db, jobs.Options{
		BootID: "01BOOT000000000000000000TH",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}

	svc, err := New(Config{
		Store: db, Client: client, Queue: q,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("download.New: %v", err)
	}
	if err := Register(q, svc); err != nil {
		t.Fatalf("Register: %v", err)
	}

	return &harness{t: t, db: db, queue: q, svc: svc, origin: o, server: srv, hub: hub, rootID: rootID}
}

// run drains the queue, so a job that retried is followed to its conclusion.
func (h *harness) run() {
	h.t.Helper()
	ctx := context.Background()
	for range 20 {
		ran, err := h.queue.RunOnce(ctx)
		if err != nil {
			h.t.Fatalf("RunOnce: %v", err)
		}
		if !ran {
			return
		}
	}
	h.t.Fatal("the queue did not settle in 20 runs")
}

func (h *harness) create(files []string) CreateResult {
	h.t.Helper()
	res, err := h.svc.Create(context.Background(), CreateRequest{
		RepoID: testRepo, Files: files, IncludeMmproj: false,
	})
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return res
}

func (h *harness) view(id string) View {
	h.t.Helper()
	v, err := h.svc.Get(context.Background(), id)
	if err != nil {
		h.t.Fatalf("Get: %v", err)
	}
	return v
}

func (h *harness) localModel(id string) model.LocalModel {
	h.t.Helper()
	var m model.LocalModel
	if err := h.db.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		m, err = h.db.LocalModel(ctx, tx, id)
		return err
	}); err != nil {
		h.t.Fatalf("read the model: %v", err)
	}
	return m
}

func (h *harness) job(subjectID string) model.Job {
	h.t.Helper()
	var j model.Job
	if err := h.db.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		rows, err := h.db.Jobs(ctx, tx, store.JobFilter{
			SubjectType: model.SubjectDownload, SubjectID: subjectID,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no job for download %s", subjectID)
		}
		j = rows[0]
		return nil
	}); err != nil {
		h.t.Fatalf("read the job: %v", err)
	}
	return j
}

// assertLanded is the assertion every success case ends with: the blob is at
// `blobs/<sha256>`, the snapshot entry is a RELATIVE symlink pointing at it, and
// the bytes hash to the name the blob wears.
func (h *harness) assertLanded(name string, f *originFile) {
	h.t.Helper()
	layout := cache.NewLayout(h.hub)

	blob := layout.BlobPath(testRepo, f.sha)
	got, err := os.ReadFile(blob)
	if err != nil {
		h.t.Fatalf("read the blob for %s: %v", name, err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != f.sha {
		h.t.Fatalf("%s hashed to %x, want %s — the resumed transfer spliced the wrong bytes",
			name, sum, f.sha)
	}
	if len(got) != len(f.content) {
		h.t.Fatalf("%s is %d bytes, want %d", name, len(got), len(f.content))
	}

	link := layout.SnapshotFile(testRepo, testCommit, name)
	target, err := os.Readlink(link)
	if err != nil {
		h.t.Fatalf("the snapshot entry for %s is not a symlink: %v", name, err)
	}
	// Relative, and pointing into this repo's blobs — what keeps the cache
	// movable and what `huggingface_hub` writes.
	if filepath.IsAbs(target) {
		h.t.Errorf("%s links to %q, want a relative target", name, target)
	}
	if want := cache.LinkTarget(name, f.sha); target != want {
		h.t.Errorf("%s links to %q, want %q", name, target, want)
	}

	// The `.incomplete` file must be gone: the rename is what publishes the
	// blob, and a partial left behind would be swept as an orphan next boot.
	if _, err := os.Stat(layout.IncompletePath(testRepo, f.sha)); !os.IsNotExist(err) {
		h.t.Errorf("%s left its .incomplete file behind", name)
	}
}

// -----------------------------------------------------------------------------
// The tests
// -----------------------------------------------------------------------------

// TestDownloadCompletesAndLinks is the baseline: one file, one job, one blob,
// one relative symlink, and every row folded to `succeeded`.
func TestDownloadCompletesAndLinks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 8192)

	res := h.create([]string{"Model-Q4_K_M.gguf"})
	if res.Job.JobID == "" {
		t.Fatal("Create returned no job; the receipt IS the response")
	}
	// The revision stored is the RESOLVED commit, never the branch: `main` names
	// a different tree next week (section 2.6).
	if got := h.localModel(res.ModelID).Revision; got != testCommit {
		t.Errorf("revision = %q, want the resolved commit", got)
	}
	// The size came from `lfs.size`, not from the 135-byte pointer the tree also
	// carried.
	if res.Download.BytesTotal != 8192 {
		t.Errorf("bytes_total = %d, want the LFS size", res.Download.BytesTotal)
	}

	h.run()
	h.assertLanded("Model-Q4_K_M.gguf", f)

	v := h.view(res.Download.ID)
	if v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded (%v)", v.State, v.ErrorMessage)
	}
	if v.BytesDone != 8192 {
		t.Errorf("bytes_done = %d, want 8192", v.BytesDone)
	}
	if len(v.Tasks) != 1 || v.Tasks[0].State != model.TaskSucceeded {
		t.Errorf("tasks = %+v", v.Tasks)
	}
	// The blob name is the sha256, and it is what the task row records — never a
	// validator.
	if v.Tasks[0].Etag == nil || *v.Tasks[0].Etag != f.sha {
		t.Errorf("task etag = %v, want the blob name %s", v.Tasks[0].Etag, f.sha)
	}

	m := h.localModel(res.ModelID)
	if m.State != model.ModelReady {
		t.Errorf("model state = %s, want ready", m.State)
	}
	if m.TotalBytes != 8192 {
		t.Errorf("model total_bytes = %d", m.TotalBytes)
	}

	// Section 2.3a: the job row and the domain row moved together.
	if j := h.job(res.Download.ID); j.State != model.JobSucceeded {
		t.Errorf("job state = %s, want succeeded", j.State)
	}
}

// TestResumeFromTruncation is section 7.4's central promise. The origin drops
// the connection mid-stream; the transfer keeps its `.incomplete` file, asks for
// the rest with a `Range`, and the FINAL FILE HASHES CORRECTLY — which is the
// only assertion that can distinguish a real resume from a silent restart.
func TestResumeFromTruncation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	const size = 64 * 1024
	f := h.origin.add("Model-Q4_K_M.gguf", size)
	h.origin.truncateAfter["Model-Q4_K_M.gguf"] = 20_000

	res := h.create([]string{"Model-Q4_K_M.gguf"})
	h.run()

	h.assertLanded("Model-Q4_K_M.gguf", f)

	v := h.view(res.Download.ID)
	if v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded (%v)", v.State, v.ErrorMessage)
	}

	// Two GETs: the truncated one and the resumed one. The second must carry a
	// Range starting exactly where the first stopped — a resume that restarted
	// from zero would also produce a correct file, and only this assertion tells
	// the two apart.
	gets := h.origin.gets("Model-Q4_K_M.gguf")
	if len(gets) != 2 {
		t.Fatalf("got %d GETs, want 2 (one truncated, one resumed): %+v", len(gets), gets)
	}
	if gets[0].rng != "" {
		t.Errorf("the first GET carried Range %q, want none", gets[0].rng)
	}
	if gets[1].rng != "bytes=20000-" {
		t.Errorf("the resume asked for %q, want bytes=20000-", gets[1].rng)
	}
	// The validator this origin issued is a real quoted entity-tag from the same
	// host, so section 7.4's first row applies and it is sent byte-exact.
	if gets[1].ifRange != `"v1"` {
		t.Errorf("If-Range = %q, want the byte-exact validator", gets[1].ifRange)
	}
	// The blob name must never appear in a header, in any test.
	if strings.Contains(gets[1].ifRange, f.sha) {
		t.Error("the blob name was sent as If-Range")
	}
}

// TestIfRangeMismatchRestarts is the other half of section 7.4: the recorded
// validator no longer matches, the origin answers `200` with the whole object,
// and the partial is DISCARDED rather than spliced onto the front of a full
// body. The digest is what proves it.
func TestIfRangeMismatchRestarts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	const size = 64 * 1024
	f := h.origin.add("Model-Q4_K_M.gguf", size)
	h.origin.truncateAfter["Model-Q4_K_M.gguf"] = 20_000
	// The object's validator changes after the first response, so the resume's
	// `If-Range` cannot match and the origin serves the whole file.
	h.origin.rotateEtag = true

	res := h.create([]string{"Model-Q4_K_M.gguf"})
	h.run()

	// The file is correct. Had the 20 000 stale bytes been kept and the full
	// body appended to them, this hash would fail — which is precisely why the
	// design can call `If-Range` an optimization and the SHA-256 the gate.
	h.assertLanded("Model-Q4_K_M.gguf", f)

	v := h.view(res.Download.ID)
	if v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded (%v)", v.State, v.ErrorMessage)
	}
	gets := h.origin.gets("Model-Q4_K_M.gguf")
	if len(gets) < 2 {
		t.Fatalf("got %d GETs, want at least 2", len(gets))
	}
	if gets[1].ifRange == "" {
		t.Error("the resume sent no If-Range; this case is meant to exercise a mismatch")
	}
	// The stale validator is cleared when the origin refuses it, so a later
	// attempt does not spend another round trip proving the same thing.
	if v.Tasks[0].Validator == nil || *v.Tasks[0].Validator != `"v2"` {
		t.Errorf("validator = %v, want the one THIS transfer's response issued", v.Tasks[0].Validator)
	}
}

// TestShardedDownloadTracksEachFile is section 7.3: one logical job, one
// progress bar per file, and a model whose `primary_file` is shard 1.
func TestShardedDownloadTracksEachFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	s1 := h.origin.add("Model-Q8_0-00001-of-00002.gguf", 12_000)
	s2 := h.origin.add("Model-Q8_0-00002-of-00002.gguf", 9_000)

	// Naming ONE shard must expand to the set: a partial set is not a model.
	res := h.create([]string{"Model-Q8_0-00002-of-00002.gguf"})
	if res.Download.BytesTotal != 21_000 {
		t.Fatalf("bytes_total = %d, want both shards", res.Download.BytesTotal)
	}

	h.run()
	h.assertLanded("Model-Q8_0-00001-of-00002.gguf", s1)
	h.assertLanded("Model-Q8_0-00002-of-00002.gguf", s2)

	v := h.view(res.Download.ID)
	if v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded (%v)", v.State, v.ErrorMessage)
	}
	if len(v.Tasks) != 2 {
		t.Fatalf("got %d tasks, want one per shard", len(v.Tasks))
	}
	// Per-file progress, in shard order: "47%" of a five-shard model tells a
	// user nothing about which shard is stuck.
	for i, want := range []struct {
		name  string
		index int
		bytes int64
	}{
		{"Model-Q8_0-00001-of-00002.gguf", 1, 12_000},
		{"Model-Q8_0-00002-of-00002.gguf", 2, 9_000},
	} {
		got := v.Tasks[i]
		if got.Filename != want.name || got.ShardIndex != want.index || got.ShardTotal != 2 {
			t.Errorf("task %d = {%s %d/%d}, want {%s %d/2}",
				i, got.Filename, got.ShardIndex, got.ShardTotal, want.name, want.index)
		}
		if got.BytesDone != want.bytes || got.BytesTotal != want.bytes {
			t.Errorf("task %d bytes = %d/%d, want %d", i, got.BytesDone, got.BytesTotal, want.bytes)
		}
		if got.State != model.TaskSucceeded {
			t.Errorf("task %d state = %s", i, got.State)
		}
	}
	if v.BytesDone != 21_000 {
		t.Errorf("bytes_done = %d, want the sum of both shards", v.BytesDone)
	}

	m := h.localModel(res.ModelID)
	if m.PrimaryFile != "Model-Q8_0-00001-of-00002.gguf" {
		t.Errorf("primary_file = %q, want shard 1", m.PrimaryFile)
	}
	if m.ShardCount != 2 {
		t.Errorf("shard_count = %d", m.ShardCount)
	}
	if m.State != model.ModelReady {
		t.Errorf("model state = %s", m.State)
	}
	// The stored fold agrees with the fold function (section 2.7's property).
	if got := FoldTaskViews(v.Tasks); got != model.DownloadVerifying {
		t.Errorf("the task rows fold to %s; the worker moves that to succeeded", got)
	}
}

// TestFlockContention is D27's other half. A competing holder of the interop
// lock — here another open file descriptor on the same path, which is exactly
// what `hf download` in another process is to the kernel — must make the task
// WAIT and say so, not fail.
//
// flock(2) locks are held per open file description, so a second Acquire in this
// process contends with the worker's exactly as another process would. That is
// the property the test is asserting, and it is why the lock primitive is pinned
// in the design rather than left to the implementation.
func TestFlockContention(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 4096)

	// Take the lock the worker will want, before it starts.
	lockPath := cache.LockPath(h.hub, testRepo, f.sha)
	if err := os.MkdirAll(filepath.Dir(lockPath), cache.DirMode); err != nil {
		t.Fatalf("create the locks directory: %v", err)
	}
	competitor, err := cache.Acquire(lockPath)
	if err != nil {
		t.Fatalf("the competing holder could not take the lock: %v", err)
	}

	res := h.create([]string{"Model-Q4_K_M.gguf"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run()
	}()

	// The task must report that it is waiting — the UI's "another tool is
	// downloading this file" — while staying `running` and healthy.
	deadline := time.Now().Add(15 * time.Second)
	saw := false
	for time.Now().Before(deadline) && !saw {
		v, err := h.svc.Get(context.Background(), res.Download.ID)
		if err == nil && len(v.Tasks) == 1 && v.Tasks[0].LastError != nil &&
			*v.Tasks[0].LastError == ErrWaitingForLock {
			saw = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !saw {
		_ = competitor.Release()
		<-done
		t.Fatal("the task never reported waiting_for_lock while another holder had the lock")
	}

	// The blob must not exist yet: the lock is what is stopping it.
	if _, err := os.Stat(cache.NewLayout(h.hub).BlobPath(testRepo, f.sha)); err == nil {
		t.Error("the blob was written while another process held the lock")
	}

	if err := competitor.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	<-done

	h.assertLanded("Model-Q4_K_M.gguf", f)
	v := h.view(res.Download.ID)
	if v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded once the lock was released (%v)",
			v.State, v.ErrorMessage)
	}
	// The wait is cleared once the lock is taken: it was a status, not a
	// failure, and leaving it behind would make a healthy download look broken.
	if v.Tasks[0].LastError != nil {
		t.Errorf("last_error = %q, want it cleared once the lock was taken", *v.Tasks[0].LastError)
	}
}

// TestBlobAlreadyPresentSkipsTheTransfer is section 7.2's write-path step 2 —
// the short-circuit that makes SPEC section 3.2's shared cache actually shared.
// Another tool put the blob there; this download links it and moves no bytes.
func TestBlobAlreadyPresentSkipsTheTransfer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 4096)

	layout := cache.NewLayout(h.hub)
	if err := os.MkdirAll(layout.BlobsDir(testRepo), cache.DirMode); err != nil {
		t.Fatalf("create the blobs directory: %v", err)
	}
	if err := os.WriteFile(layout.BlobPath(testRepo, f.sha), f.content, cache.FileMode); err != nil {
		t.Fatalf("plant the blob: %v", err)
	}

	res := h.create([]string{"Model-Q4_K_M.gguf"})
	h.run()

	h.assertLanded("Model-Q4_K_M.gguf", f)
	if v := h.view(res.Download.ID); v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded", v.State)
	}
	// A HEAD to learn the blob name is expected; a GET is not.
	if gets := h.origin.gets("Model-Q4_K_M.gguf"); len(gets) != 0 {
		t.Errorf("the transfer made %d GETs for a blob already on disk", len(gets))
	}
}

// TestGatedRepositoryIsRefusedAtCreation is section 3.6's UX signal. The tree
// succeeds and the resolve is refused, which is exactly how a gate behaves; the
// refusal must reach the user AT THE CLICK, carrying the page they can accept
// the terms on, rather than an hour later as a failed job.
func TestGatedRepositoryIsRefusedAtCreation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 4096)
	h.origin.gated = true

	_, err := h.svc.Create(context.Background(), CreateRequest{
		RepoID: testRepo, Files: []string{"Model-Q4_K_M.gguf"},
	})
	if err == nil {
		t.Fatal("Create succeeded against a gated repository")
	}
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want a model.Error", err)
	}
	if me.Code != CodeHFGated {
		t.Fatalf("code = %s, want %s", me.Code, CodeHFGated)
	}
	if me.Details["repo"] != testRepo {
		t.Errorf("details.repo = %v, want %s", me.Details["repo"], testRepo)
	}
	url, _ := me.Details["request_url"].(string)
	if !strings.HasSuffix(url, "/"+testRepo) {
		t.Errorf("details.request_url = %q, want the repository page to link out to", url)
	}

	// Nothing was written: a refusal leaves no half-created download behind.
	var count int
	if err := h.db.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		rows, err := h.db.Downloads(ctx, tx, store.DownloadFilter{})
		count = len(rows)
		return err
	}); err != nil {
		t.Fatalf("list downloads: %v", err)
	}
	if count != 0 {
		t.Errorf("%d download rows exist after a refusal", count)
	}
}

// TestDuplicateDownloadIsRefused is the `409 download_exists` guard, and it
// NAMES the existing download rather than merely refusing: a user who
// double-clicked wants to be taken to the one that is running.
func TestDuplicateDownloadIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 4096)

	first := h.create([]string{"Model-Q4_K_M.gguf"})
	_, err := h.svc.Create(context.Background(), CreateRequest{
		RepoID: testRepo, Files: []string{"Model-Q4_K_M.gguf"},
	})
	var me model.Error
	if !errors.As(err, &me) || me.Code != model.CodeDownloadExists {
		t.Fatalf("err = %v, want download_exists", err)
	}
	if me.Details["download_id"] != first.Download.ID {
		t.Errorf("details.download_id = %v, want %s", me.Details["download_id"], first.Download.ID)
	}
}

// TestIdempotencyReplayReturnsTheSameJob is D65's window: a repeat POST inside
// it returns the ORIGINAL job rather than creating a second one, which is what
// makes a double-clicked Download a replay instead of a 409.
func TestIdempotencyReplayReturnsTheSameJob(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 4096)

	req := CreateRequest{
		RepoID: testRepo, Files: []string{"Model-Q4_K_M.gguf"},
		Idempotency: &jobs.Idempotency{
			Key: "key-1", Route: "POST /api/v1/downloads", RequestFingerprint: "fp-1",
		},
	}
	first, err := h.svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.Job.Replayed {
		t.Error("the first request was reported as a replay")
	}

	second, err := h.svc.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Job.Replayed {
		t.Error("the replay was not recognized; a second job was queued")
	}
	if second.Job.JobID != first.Job.JobID {
		t.Errorf("replay job = %s, want the original %s", second.Job.JobID, first.Job.JobID)
	}
}

// TestSweepRemovesOrphansAndKeepsOurs is section 7.4's startup sweep. Both
// halves are asserted, because getting either wrong is a data-loss bug: a
// partial no task row names is removed, and one a PAUSED download owns is not.
func TestSweepRemovesOrphansAndKeepsOurs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 4096)
	res := h.create([]string{"Model-Q4_K_M.gguf"})

	layout := cache.NewLayout(h.hub)
	if err := os.MkdirAll(layout.BlobsDir(testRepo), cache.DirMode); err != nil {
		t.Fatalf("create the blobs directory: %v", err)
	}
	// One partial this daemon owns, recorded on the task row…
	ours := layout.IncompletePath(testRepo, f.sha)
	if err := os.WriteFile(ours, []byte("partial"), cache.FileMode); err != nil {
		t.Fatalf("write our partial: %v", err)
	}
	if err := h.db.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		tasks, err := h.db.DownloadTasks(ctx, tx, res.Download.ID)
		if err != nil {
			return err
		}
		row := tasks[0]
		row.IncompletePath = &ours
		_, err = h.db.UpdateDownloadTaskTransfer(ctx, tx, row)
		return err
	}); err != nil {
		t.Fatalf("record the partial: %v", err)
	}
	// …and one nobody claims.
	orphan := layout.IncompletePath(testRepo, strings.Repeat("f", 64))
	if err := os.WriteFile(orphan, []byte("somebody else's"), cache.FileMode); err != nil {
		t.Fatalf("write the orphan: %v", err)
	}

	got, err := h.svc.SweepPrimary(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got.Removed != 1 {
		t.Errorf("removed %d, want exactly the orphan", got.Removed)
	}
	if _, err := os.Stat(ours); err != nil {
		t.Error("the sweep removed a partial a download row names; a pause would become a restart")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan survived the sweep")
	}
}

// TestCancelClosesTheRowsAndKeepsThePartial is section 3.8's default:
// `keep_partial=true`, so a retry resumes rather than starting over.
func TestCancelClosesTheRowsAndKeepsThePartial(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 4096)
	res := h.create([]string{"Model-Q4_K_M.gguf"})

	// No worker holds the job, so the queue closes it and the domain rows in one
	// transaction through the worker's DomainWriter — section 2.3a's other path.
	if err := h.svc.Cancel(context.Background(), res.Download.ID, true); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	v := h.view(res.Download.ID)
	if v.State != model.DownloadCanceled {
		t.Errorf("state = %s, want canceled", v.State)
	}
	if v.Tasks[0].State != model.TaskCanceled {
		t.Errorf("task state = %s, want canceled", v.Tasks[0].State)
	}
	if j := h.job(res.Download.ID); j.State != model.JobCanceled {
		t.Errorf("job state = %s, want canceled", j.State)
	}

	// A second cancel is refused rather than silently accepted: the download has
	// already finished, and pretending otherwise would hide a client bug.
	err := h.svc.Cancel(context.Background(), res.Download.ID, true)
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeDownloadNotPausable {
		t.Fatalf("second cancel = %v, want download_not_pausable", err)
	}
}

// TestPauseAndResumeMoveBothRows is section 2.3a's "pause/resume moves both",
// and the reason `paused` is a JOB state rather than a downloads-only flag:
// without it a paused download would either hold a lease forever or free its
// subject for a duplicate job.
func TestPauseAndResumeMoveBothRows(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 4096)
	res := h.create([]string{"Model-Q4_K_M.gguf"})

	if err := h.svc.Pause(context.Background(), res.Download.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	v := h.view(res.Download.ID)
	if v.State != model.DownloadPaused {
		t.Errorf("downloads.state = %s, want paused", v.State)
	}
	if v.Tasks[0].State != model.TaskPaused {
		t.Errorf("task state = %s, want paused", v.Tasks[0].State)
	}
	if j := h.job(res.Download.ID); j.State != model.JobPaused {
		t.Errorf("jobs.state = %s, want paused", j.State)
	}

	// A paused job is not claimable, so the queue finds nothing to run — which
	// is the whole point: the pause survives, and it holds its subject against
	// `idx_jobs_one_live_per_subject` while it stands.
	ran, err := h.queue.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ran {
		t.Error("the queue ran a paused download")
	}

	// Pausing twice is refused rather than silently accepted.
	err = h.svc.Pause(context.Background(), res.Download.ID)
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeDownloadNotPausable {
		t.Fatalf("second pause = %v, want download_not_pausable", err)
	}

	if err := h.svc.Resume(context.Background(), res.Download.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.view(res.Download.ID).State; got != model.DownloadQueued {
		t.Errorf("downloads.state = %s, want queued", got)
	}
	if j := h.job(res.Download.ID); j.State != model.JobQueued {
		t.Errorf("jobs.state = %s, want queued", j.State)
	}

	h.run()
	h.assertLanded("Model-Q4_K_M.gguf", f)
	if got := h.view(res.Download.ID).State; got != model.DownloadSucceeded {
		t.Errorf("state = %s, want succeeded after the resume", got)
	}
}

// TestRetryResumesACanceledDownload is the control verb that has to reach a
// FINISHED job: `failed` and `canceled` are terminal, so the live-job query the
// other verbs use filters out exactly the rows a retry is for.
//
// It also asserts the point of `keep_partial`: the retry resumes from the bytes
// the cancel left behind rather than starting over.
func TestRetryResumesACanceledDownload(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	f := h.origin.add("Model-Q4_K_M.gguf", 32_000)
	res := h.create([]string{"Model-Q4_K_M.gguf"})

	if err := h.svc.Cancel(context.Background(), res.Download.ID, true); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if v := h.view(res.Download.ID); v.State != model.DownloadCanceled {
		t.Fatalf("state = %s, want canceled", v.State)
	}

	// Leave a partial behind, as an interrupted transfer would have.
	layout := cache.NewLayout(h.hub)
	if err := os.MkdirAll(layout.BlobsDir(testRepo), cache.DirMode); err != nil {
		t.Fatalf("create the blobs directory: %v", err)
	}
	partial := layout.IncompletePath(testRepo, f.sha)
	if err := os.WriteFile(partial, f.content[:10_000], cache.FileMode); err != nil {
		t.Fatalf("write the partial: %v", err)
	}

	if err := h.svc.Retry(context.Background(), res.Download.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	h.run()

	h.assertLanded("Model-Q4_K_M.gguf", f)
	if v := h.view(res.Download.ID); v.State != model.DownloadSucceeded {
		t.Fatalf("state = %s, want succeeded (%v)", v.State, v.ErrorMessage)
	}
	// The retry resumed: it asked for the bytes after the partial, not for the
	// whole file. The digest above proves the splice was correct.
	gets := h.origin.gets("Model-Q4_K_M.gguf")
	if len(gets) != 1 {
		t.Fatalf("got %d GETs, want 1", len(gets))
	}
	if gets[0].rng != "bytes=10000-" {
		t.Errorf("the retry asked for %q, want it to resume at bytes=10000-", gets[0].rng)
	}
}

// TestPriorityMovesBothRows is the queue reorder. The pool leases on
// `jobs.priority`, so a reorder that moved only `downloads.priority` would
// change the list a user reads without changing the order the worker works
// through — the most confusing possible outcome for a control whose entire
// purpose is order.
func TestPriorityMovesBothRows(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Q4_K_M.gguf", 4096)
	res := h.create([]string{"Model-Q4_K_M.gguf"})

	if err := h.svc.SetPriority(context.Background(), res.Download.ID, 10); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	if got := h.view(res.Download.ID).Priority; got != 10 {
		t.Errorf("downloads.priority = %d, want 10", got)
	}
	if got := h.job(res.Download.ID).Priority; got != 10 {
		t.Errorf("jobs.priority = %d, want 10 — the pool leases on this column", got)
	}
}

// TestInsufficientDiskCarriesTheNumbers: a refusal that will not say how much is
// free and how much is needed is a refusal a user cannot act on.
func TestInsufficientDiskCarriesTheNumbers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.origin.add("Model-Huge.gguf", 16)
	// The tree reports the true size from `lfs.size`. Inflating it past any
	// plausible filesystem exercises the guard against a REAL statfs, which is
	// the only way to know the arithmetic is being done against the right
	// number.
	h.origin.inflate = 1 << 60

	_, err := h.svc.Create(context.Background(), CreateRequest{
		RepoID: testRepo, Files: []string{"Model-Huge.gguf"},
	})
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeInsufficientDisk {
		t.Fatalf("err = %v, want insufficient_disk", err)
	}
	for _, key := range []string{"free_bytes", "needed_bytes", "total_bytes", "path"} {
		if _, ok := me.Details[key]; !ok {
			t.Errorf("details is missing %s: %+v", key, me.Details)
		}
	}
}

// TestServerSuppliedCommitIsRefusedAsADirectoryName closes a path-traversal into
// the hub root.
//
// `x-repo-commit` is read off the FINAL response after redirects — the CDN's
// headers, or whatever host `settings['hf.endpoint']` names — and it becomes a
// snapshot DIRECTORY NAME through `layout.SnapshotDir`, which is a plain
// filepath.Join. A header of `../../../..` would have the transfer create
// directories and symlinks outside the hub root as the service identity, and
// store the escaped path in `models.snapshot_dir`. Section 2.6 says that column
// holds a resolved commit sha and nothing else, so that is what is enforced.
func TestServerSuppliedCommitIsRefusedAsADirectoryName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		commit string
	}{
		{name: "parent traversal", commit: "../../../../.."},
		{name: "traversal hidden in a plausible sha", commit: "../" + testCommit},
		{name: "an absolute path", commit: "/etc"},
		{name: "a branch name", commit: "main"},
		{name: "a short sha", commit: "4f0ac1c"},
		{name: "not hex", commit: strings.Repeat("z", 40)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.origin.add("Model-Q4_K_M.gguf", 4096)
			h.origin.commit = tc.commit

			_, err := h.svc.Create(context.Background(), CreateRequest{
				RepoID: testRepo, Files: []string{"Model-Q4_K_M.gguf"},
			})
			if err == nil {
				t.Fatalf("Create accepted %q as the snapshot directory name", tc.commit)
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != CodeHFUnreachable {
				t.Fatalf("err = %v, want a %s model.Error", err, CodeHFUnreachable)
			}
			// Nothing outside the hub root was created, and no row records the
			// path that would have been.
			entries, rerr := os.ReadDir(filepath.Dir(h.hub))
			if rerr != nil {
				t.Fatalf("read the cache directory: %v", rerr)
			}
			for _, e := range entries {
				if e.Name() != "hub" {
					t.Errorf("%q was created beside the hub root", e.Name())
				}
			}
		})
	}
}
