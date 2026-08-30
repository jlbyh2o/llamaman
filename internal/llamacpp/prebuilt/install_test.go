package prebuilt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// fakeFetcher writes a canned archive instead of downloading one, so the whole
// pipeline is exercised without a network.
type fakeFetcher struct {
	payload []byte
	err     error
	calls   int
	lastURL string
}

func (f *fakeFetcher) Fetch(_ context.Context, url, dst, expect string, progress ProgressFunc) (string, error) {
	f.calls++
	f.lastURL = url
	if f.err != nil {
		return "", f.err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, f.payload, 0o640); err != nil {
		return "", err
	}
	sum := sha256.Sum256(f.payload)
	hexsum := hex.EncodeToString(sum[:])
	if expect != "" && expect != hexsum {
		return hexsum, ErrChecksumMismatch
	}
	if progress != nil {
		progress(int64(len(f.payload)), int64(len(f.payload)))
	}
	return hexsum, nil
}

// installFixture is a state directory laid out the way section 6.1 describes.
type installFixture struct {
	stateDir     string
	versionsRoot string
	tmpDir       string
	fetcher      *fakeFetcher
	runs         runs
	steps        []model.FailingStep
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	state := t.TempDir()
	f := &installFixture{
		stateDir:     state,
		versionsRoot: filepath.Join(state, "versions"),
		tmpDir:       filepath.Join(state, "tmp"),
		fetcher:      &fakeFetcher{payload: goodArchive(t)},
		runs:         healthyRuns(t),
	}
	if err := os.MkdirAll(f.versionsRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *installFixture) request(id string) InstallRequest {
	return InstallRequest{
		VersionID:       id,
		Tag:             "b10621",
		Channel:         model.ChannelNightly,
		Backend:         model.BackendCPU,
		AssetName:       "llama-b10621-bin-ubuntu-x64.tar.gz",
		AssetURL:        "https://github.invalid/download/b10621/llama-b10621-bin-ubuntu-x64.tar.gz",
		AssetReleaseTag: "b10621",
		VersionsRoot:    f.versionsRoot,
		TmpDir:          f.tmpDir,
		Fetcher:         f.fetcher,
		Run:             f.runs.runner(),
		HostLibc:        toolchain.Libc{Kind: toolchain.LibcGlibc, VersionString: "2.43"},
		BuiltBy:         "llamaman/0.1.0-test",
		Now:             func() time.Time { return time.Unix(1770000000, 0).UTC() },
		Progress: func(step model.FailingStep, _, _ int64, _ string) {
			if len(f.steps) == 0 || f.steps[len(f.steps)-1] != step {
				f.steps = append(f.steps, step)
			}
		},
	}
}

func TestInstallEndToEnd(t *testing.T) {
	f := newInstallFixture(t)
	req := f.request("b10621-cpu-bin")
	req.PublishedSHA256 = sha256Hex(f.fetcher.payload)

	res, err := Install(t.Context(), req)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The published tree is where section 6.1 says, and the staging directory
	// is gone because it was renamed into place.
	want := VersionDir(f.versionsRoot, "b10621-cpu-bin")
	if res.VersionDir != want {
		t.Errorf("version dir = %q, want %q", res.VersionDir, want)
	}
	if _, err := os.Stat(filepath.Join(want, "bin", BinServer)); err != nil {
		t.Errorf("bin/llama-server is not in the published tree: %v", err)
	}
	if _, err := os.Stat(StagingDir(f.versionsRoot, "b10621-cpu-bin")); err == nil {
		t.Error("the staging directory survived the publish")
	}
	// The archive is not kept: it is tens of megabytes and the tree is what
	// matters.
	if _, err := os.Stat(filepath.Join(f.tmpDir, req.AssetName)); err == nil {
		t.Error("the downloaded tarball was left in tmp/")
	}

	// The steps a job's progress_json records, in order.
	if !slices.Equal(f.steps, []model.FailingStep{model.StepFetch, model.StepVerify, model.StepInstall}) {
		t.Errorf("steps = %v", f.steps)
	}
	if res.FailingStep != "" {
		t.Errorf("failing step = %q on a successful install", res.FailingStep)
	}
	if res.SizeBytes <= 0 {
		t.Error("no size recorded")
	}
	if res.ArchiveSHA256 != sha256Hex(f.fetcher.payload) {
		t.Errorf("archive digest = %q", res.ArchiveSHA256)
	}

	// manifest.json, written into the staging tree BEFORE the rename, so a
	// version directory never exists without one.
	m, err := ReadManifest(want)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.ManifestVersion != ManifestVersion || m.VersionID != "b10621-cpu-bin" {
		t.Errorf("manifest identity = %+v", m)
	}
	if m.Acquisition != string(model.AcquisitionPrebuilt) || m.Backend != string(model.BackendCPU) {
		t.Errorf("manifest acquisition/backend = %q/%q", m.Acquisition, m.Backend)
	}
	if m.AssetSHA256 == "" || m.PublishedSHA256 != m.AssetSHA256 {
		t.Errorf("digests = computed %q, published %q", m.AssetSHA256, m.PublishedSHA256)
	}
	if m.HostGlibc != "2.43" {
		t.Errorf("host glibc = %q", m.HostGlibc)
	}
	if m.TopLevel != "build" {
		t.Errorf("stripped top level = %q", m.TopLevel)
	}
	// The capture section 2.5 requires verbatim, and the two columns derived
	// from it.
	if m.ServerHelp == "" {
		t.Error("the manifest carries no llama-server --help capture")
	}
	if !m.SupportsFit {
		t.Error("supports_fit was not derived from the capture")
	}
	if len(m.HelpFlags) == 0 || !slices.Contains(m.HelpFlags, "-ngl") {
		t.Errorf("help flags = %v", m.HelpFlags)
	}
	if m.VersionOutput == "" || m.DevicesOutput == "" {
		t.Error("the version and device captures are missing from the manifest")
	}
	if !slices.Contains(m.Binaries, BinServer) {
		t.Errorf("binaries = %v", m.Binaries)
	}
}

func TestInstallRejectionKeepsTheStagingTreeAndSignalsTheFallback(t *testing.T) {
	// D18 end to end: the download and the extraction are fine, the binary does
	// not run here, the row becomes `failed_verification`, and a SOURCE build of
	// the same tag is what the caller enqueues next.
	f := newInstallFixture(t)
	f.runs["llama-server --version"] = struct {
		out  string
		code int
		err  error
	}{out: "version `GLIBC_2.38' not found\n", code: 127}

	req := f.request("b10621-cpu-bin")
	req.HostLibc = toolchain.Libc{Kind: toolchain.LibcGlibc, VersionString: "2.36"}

	res, err := Install(t.Context(), req)
	if err == nil {
		t.Fatal("a binary that would not execute was installed")
	}
	if !errors.Is(err, ErrVerification) {
		t.Errorf("error %v is not an ErrVerification", err)
	}
	if res.FailingStep != model.StepVerify {
		t.Errorf("failing step = %q, want verify", res.FailingStep)
	}
	if !res.SourceFallback {
		t.Error("no source-build fallback signaled")
	}
	if res.Verify.Diagnosis == nil {
		t.Error("no diagnosis to carry into the replacement row's params_json")
	}
	// Nothing was published, and the staging tree is kept for the retry.
	if _, err := os.Stat(VersionDir(f.versionsRoot, "b10621-cpu-bin")); err == nil {
		t.Error("a rejected install published a version directory")
	}
	if _, err := os.Stat(StagingDir(f.versionsRoot, "b10621-cpu-bin")); err != nil {
		t.Errorf("the staging tree was discarded: %v", err)
	}
}

func TestInstallClearsAStagingTreeFromAPreviousAttempt(t *testing.T) {
	// The hardened reader refuses to overwrite an existing file, so a
	// half-extracted tree left by a failed attempt would fail every retry until
	// someone deleted it by hand.
	f := newInstallFixture(t)
	stale := StagingDir(f.versionsRoot, "b10621-cpu-bin")
	if err := os.MkdirAll(filepath.Join(stale, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "bin", BinServer), []byte("half"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(t.Context(), f.request("b10621-cpu-bin")); err != nil {
		t.Fatalf("Install over a stale staging tree: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(VersionDir(f.versionsRoot, "b10621-cpu-bin"), "bin", BinServer))
	if err != nil || string(b) == "half" {
		t.Errorf("the stale tree was merged into rather than replaced: %q, %v", b, err)
	}
}

func TestInstallHardenedExtractionRejectsAHostileArchive(t *testing.T) {
	f := newInstallFixture(t)
	f.fetcher.payload = tarGz(t, dir("build"), file("build/../../../etc/passwd", "root::0:0"))

	res, err := Install(t.Context(), f.request("b10621-cpu-bin"))
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
	if res.FailingStep != model.StepFetch {
		t.Errorf("failing step = %q, want fetch (extraction is part of it in section 2.5's table)", res.FailingStep)
	}
}

func TestInstallDownloadFailure(t *testing.T) {
	f := newInstallFixture(t)
	f.fetcher.err = errors.New("connection reset")

	res, err := Install(t.Context(), f.request("b10621-cpu-bin"))
	if err == nil {
		t.Fatal("a failed download reported success")
	}
	if res.FailingStep != model.StepFetch {
		t.Errorf("failing step = %q, want fetch", res.FailingStep)
	}
	if res.SourceFallback {
		t.Error("a network failure signaled a source-build fallback")
	}
}

func TestInstallForcedRebuildSwapsAndRespectsTheGuard(t *testing.T) {
	f := newInstallFixture(t)
	// A first install to have something to rebuild over.
	if _, err := Install(t.Context(), f.request("b10621-cpu-bin")); err != nil {
		t.Fatalf("first install: %v", err)
	}

	t.Run("refused while a process runs from it", func(t *testing.T) {
		req := f.request("b10621-cpu-bin")
		req.Guard = &guard{pid: 999, inUse: true}
		res, err := Install(t.Context(), req)
		if !errors.Is(err, ErrVersionInUse) {
			t.Fatalf("error = %v, want ErrVersionInUse", err)
		}
		if res.FailingStep != model.StepInstall {
			t.Errorf("failing step = %q, want install", res.FailingStep)
		}
	})

	t.Run("swapped when nothing is running", func(t *testing.T) {
		req := f.request("b10621-cpu-bin")
		g := &guard{}
		req.Guard = g
		if _, err := Install(t.Context(), req); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if g.asked == 0 {
			t.Error("the live-process guard was never asked before the swap")
		}
		if _, err := os.Stat(VersionDir(f.versionsRoot, "b10621-cpu-bin") + oldSuffix); err == nil {
			t.Error("the displaced tree was left behind")
		}
	})
}

func TestInstallRequiresItsInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*InstallRequest)
	}{
		{name: "no version id", mutate: func(r *InstallRequest) { r.VersionID = "" }},
		{name: "no versions root", mutate: func(r *InstallRequest) { r.VersionsRoot = "" }},
		{name: "no asset URL", mutate: func(r *InstallRequest) { r.AssetURL = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			req := f.request("b10621-cpu-bin")
			tc.mutate(&req)
			if _, err := Install(t.Context(), req); err == nil {
				t.Fatal("Install succeeded")
			}
			if f.fetcher.calls != 0 && tc.name != "no asset URL" {
				t.Error("a request that could not succeed still downloaded something")
			}
		})
	}
}

func TestInstallKeepsTheArchiveWhenAsked(t *testing.T) {
	f := newInstallFixture(t)
	req := f.request("b10621-cpu-bin")
	req.KeepArchive = true

	if _, err := Install(t.Context(), req); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.tmpDir, req.AssetName)); err != nil {
		t.Errorf("the archive was removed despite KeepArchive: %v", err)
	}
}

// ------------------------------------------------------------------- manifest

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		VersionID: "v0.3.0-cpu-bin", Tag: "v0.3.0", BuildTag: "b10621",
		Channel: string(model.ChannelStable), Acquisition: string(model.AcquisitionPrebuilt),
		Backend: string(model.BackendCPU), BuiltAt: time.Unix(1770000000, 0).UTC(),
		Binaries: []string{BinServer, BinBench}, SizeBytes: 1234,
		HelpFlags: []string{"-m", "-ngl"}, SupportsFit: true,
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ManifestVersion != ManifestVersion {
		t.Errorf("manifest_version = %d, want %d", got.ManifestVersion, ManifestVersion)
	}
	if got.VersionID != m.VersionID || got.BuildTag != m.BuildTag || !got.SupportsFit {
		t.Errorf("round trip = %+v", got)
	}

	// The keys are a contract shared with a source build's manifest, so one
	// reader decodes both.
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	for _, key := range []string{
		"manifest_version", "version_id", "acquisition", "backend", "built_at",
		"binaries", "size_bytes", "supports_fit",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("manifest.json is missing %q", key)
		}
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Error("manifest.json does not end with a newline")
	}
}

func TestManifestReadMissing(t *testing.T) {
	if _, err := ReadManifest(t.TempDir()); err == nil {
		t.Fatal("reading an absent manifest succeeded")
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "libggml.so.0"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libggml.so.0", filepath.Join(dir, "lib", "libggml.so")); err != nil {
		t.Fatal(err)
	}
	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	// The symlink is not counted a second time: a llama.cpp tree is full of
	// them and counting targets twice would double every reported size.
	if got != 4096 {
		t.Errorf("DirSize = %d, want 4096", got)
	}
}
