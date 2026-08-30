package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
)

// TestBuildRealCMakeProject runs the whole pipeline against REAL git and REAL
// cmake over the tiny project in testdata — everything the fake toolchain
// cannot vouch for.
//
// The fake tools prove the phase machine. This proves the parts that are only
// true if a real cmake agrees: that section 6.5's flag set configures, that the
// generator resolution works, that `cmake --install` puts the three binaries
// and the shared library where the layout expects them, and — the one that
// would otherwise be discovered in production — that D22's `$ORIGIN/../lib`
// RPATH makes the installed tree RELOCATABLE, which is what makes symlink
// activation and rollback safe.
//
// It is skipped, not failed, on a host with no toolchain: a developer without
// cmake still gets the rest of the suite.
func TestBuildRealCMakeProject(t *testing.T) {
	t.Parallel()

	requireTools(t, "git", "cmake")
	if _, err := exec.LookPath("cc"); err != nil {
		if _, err := exec.LookPath("gcc"); err != nil {
			t.Skip("no C compiler on PATH")
		}
	}
	if _, err := exec.LookPath("ninja"); err != nil {
		if _, err := exec.LookPath("make"); err != nil {
			t.Skip("neither ninja nor make on PATH")
		}
	}

	root := t.TempDir()
	upstream := gitFixtureRepo(t, root)
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o750); err != nil {
		t.Fatal(err)
	}

	obs := &recordObserver{}
	b := &Builder{
		Layout: Layout{StateDir: state},
		Logs:   NewLogRegistry(),
		Guard:  stubGuard{},
		OOM:    stubOOM{},
	}
	req := Request{
		VersionID: "tiny-cpu-src",
		Tag:       "tiny",
		GitURL:    upstream.dir,
		GitRef:    upstream.head,
		Backend:   model.BackendCPU,
		Observer:  obs,
	}

	res, err := b.Build(t.Context(), req)
	if err != nil {
		t.Fatalf("Build: %v\n--- log ---\n%s", err, readFile(t, b.Layout.LogPath(req.VersionID)))
	}

	t.Run("the real toolchain was detected in preflight", func(t *testing.T) {
		log := readFile(t, b.Layout.LogPath(req.VersionID))
		if !strings.Contains(log, "toolchain: git=") {
			t.Errorf("the preflight did not record the toolchain:\n%s", log)
		}
	})

	t.Run("the three binaries and the shared library are installed", func(t *testing.T) {
		for _, p := range []string{
			ServerPath(res.VersionDir), BenchPath(res.VersionDir), CLIPath(res.VersionDir),
		} {
			if !exists(t, p) {
				t.Errorf("%s is missing", p)
			}
		}
		libs, err := os.ReadDir(LibDir(res.VersionDir))
		if err != nil || len(libs) == 0 {
			t.Errorf("lib/ is empty (%v): -DBUILD_SHARED_LIBS=ON produced nothing to find", err)
		}
	})

	t.Run("D18: the installed llama-server runs on this host", func(t *testing.T) {
		if !strings.Contains(res.Verification.VersionOutput, "version: 6000") {
			t.Errorf("--version output = %q", res.Verification.VersionOutput)
		}
	})

	t.Run("the help capture became help_flags_json and supports_fit", func(t *testing.T) {
		if !res.Verification.SupportsFit {
			t.Error("SupportsFit = false")
		}
		for _, want := range []string{"--fit", "--ctx-size", "-c"} {
			if !res.Verification.HelpFlags.Has(want) {
				t.Errorf("HelpFlags is missing %s: %v", want, res.Verification.HelpFlags)
			}
		}
	})

	t.Run("D22: the installed tree is relocatable and needs no LD_LIBRARY_PATH", func(t *testing.T) {
		// Moving the directory is the whole claim: a version tree is renamed
		// into place at publish and reached through `versions/active`, so a
		// binary that only worked from its build location would break the first
		// time it was activated.
		moved := filepath.Join(root, "moved-elsewhere")
		if err := os.Rename(res.VersionDir, moved); err != nil {
			t.Fatalf("rename the version tree: %v", err)
		}
		t.Cleanup(func() { _ = os.Rename(moved, res.VersionDir) })

		out, result, err := procx.Capture(t.Context(), procx.Cmd{
			Path: ServerPath(moved),
			Args: []string{"--version"},
			// An empty LD_LIBRARY_PATH is the point: the loader must find
			// libggml-base.so through the install RPATH alone.
			ExtraEnv: []string{"LD_LIBRARY_PATH="},
		})
		if err != nil || !result.OK() {
			t.Fatalf("the relocated llama-server does not run (%v): %s", err, out)
		}
		if !strings.Contains(out, "version: 6000") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("a warm rebuild reuses the worktree and the cmake cache", func(t *testing.T) {
		if !b.CanResume(req.VersionID) {
			t.Fatal("CanResume = false, but the build directory was kept (D4)")
		}
		resumeReq := req
		resumeReq.Resume = true
		resumeReq.Observer = &recordObserver{}
		res2, err := b.Build(t.Context(), resumeReq)
		if err != nil {
			t.Fatalf("resumed build: %v\n--- log ---\n%s", err, readFile(t, b.Layout.LogPath(req.VersionID)))
		}
		if !res2.Resumed {
			t.Error("Result.Resumed = false")
		}
		if !exists(t, ServerPath(res2.VersionDir)) {
			t.Error("the resumed build did not publish")
		}
	})
}

type fixtureRepo struct {
	dir  string
	head string
}

// gitFixtureRepo turns testdata/tinyproject into a real git repository the
// pipeline can clone, so the `fetch` phase runs against real git rather than
// against a fake.
func gitFixtureRepo(t *testing.T, root string) fixtureRepo {
	t.Helper()

	dir := filepath.Join(root, "upstream")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join("testdata", "tinyproject"), dir)

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=llamaman-test", "GIT_AUTHOR_EMAIL=test@invalid",
			"GIT_COMMITTER_NAME=llamaman-test", "GIT_COMMITTER_EMAIL=test@invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "-b", "main")
	// A blobless partial clone of a local repository only filters when the
	// serving side allows it; without this git prints a warning and sends
	// everything, which would still pass but would not exercise the filter.
	git("config", "uploadpack.allowFilter", "true")
	git("add", ".")
	git("commit", "-m", "tiny llama.cpp stand-in")
	return fixtureRepo{dir: dir, head: git("rev-parse", "HEAD")}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o640)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s is not installed", n)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return string(b)
}

// TestSmokeBuildLlamaCPP builds llama.cpp itself, from the real upstream
// repository, on the CPU backend.
//
// It is guarded by an environment variable and skipped otherwise, for the two
// reasons the project's testing rules name: it reaches the network, and it
// takes minutes rather than seconds. Everything it covers is covered
// deterministically by the fake-toolchain suite and by the tiny-project test
// above; what only THIS test can catch is upstream changing a cmake option or a
// binary name out from under section 6.5's flag set.
//
//	LLAMAMAN_SMOKE_LLAMACPP_BUILD=1 go test ./internal/llamacpp/source -run Smoke -timeout 60m
//	LLAMAMAN_SMOKE_LLAMACPP_REF=b6100  (optional; defaults to master)
func TestSmokeBuildLlamaCPP(t *testing.T) {
	if os.Getenv("LLAMAMAN_SMOKE_LLAMACPP_BUILD") == "" {
		t.Skip("set LLAMAMAN_SMOKE_LLAMACPP_BUILD=1 to build llama.cpp from source over the network")
	}
	requireTools(t, "git", "cmake")

	ref := os.Getenv("LLAMAMAN_SMOKE_LLAMACPP_REF")
	if ref == "" {
		ref = "master"
	}
	state := t.TempDir()
	b := &Builder{
		Layout: Layout{StateDir: state},
		Logs:   NewLogRegistry(),
	}
	req := Request{
		VersionID: "smoke-cpu-src",
		Tag:       "smoke",
		GitRef:    ref,
		Backend:   model.BackendCPU,
		Observer: ObserverFunc(func(_ context.Context, p Progress) error {
			t.Logf("phase=%s pct=%v jobs=%d", p.Phase, p.Pct, p.Jobs)
			return nil
		}),
	}

	res, err := b.Build(t.Context(), req)
	if err != nil {
		t.Fatalf("Build: %v\n--- tail of the log ---\n%s", err,
			lastLines(readFile(t, b.Layout.LogPath(req.VersionID)), 80))
	}
	if !res.Verification.HelpFlags.Available() {
		t.Error("the real llama-server produced no parseable help capture")
	}
	if !exists(t, BenchPath(res.VersionDir)) {
		t.Error("llama-bench is missing: -DLLAMA_BUILD_TOOLS=ON (D23) did not do its job")
	}
	t.Logf("built %s at %s in %s (%d binaries, %.1f MiB)",
		req.VersionID, res.ResolvedCommit, res.Duration().Round(1e9), len(res.Binaries),
		float64(res.SizeBytes)/(1<<20))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
