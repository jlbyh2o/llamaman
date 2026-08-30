package prebuilt

import (
	"os"
	"path/filepath"
	"testing"
)

// The regression these tests exist for: `llama-bNNNN-bin-ubuntu-x64.tar.gz`
// carries ONE top-level directory with every binary and every `.so` flat inside
// it. Stripping the top level left the whole release in the staging root, Verify
// could not find `bin/llama-server`, and the CPU prebuilt channel — the wizard's
// one non-skippable step — failed on every host.

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestNormalizeLayoutBuildsBinAndLibForAFlatRelease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	writeExec(t, filepath.Join(root, BinServer))
	writeExec(t, filepath.Join(root, "llama-bench"))
	for _, lib := range []string{"libggml.so", "libggml-base.so.0", "libllama.so"} {
		if err := os.WriteFile(filepath.Join(root, lib), []byte("elf"), 0o640); err != nil {
			t.Fatalf("write %s: %v", lib, err)
		}
	}
	// A plain data file at the root belongs in neither directory.
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("mit"), 0o640); err != nil {
		t.Fatalf("write LICENSE: %v", err)
	}

	changed, err := NormalizeLayout(root)
	if err != nil {
		t.Fatalf("NormalizeLayout: %v", err)
	}
	if !changed {
		t.Fatal("NormalizeLayout reported no change on a flat release")
	}

	// What Verify stats and what Runtime.ServerPath renders into every argv.
	if _, err := os.Stat(filepath.Join(root, "bin", BinServer)); err != nil {
		t.Fatalf("bin/%s is not resolvable after normalization: %v", BinServer, err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "llama-bench")); err != nil {
		t.Fatalf("bin/llama-bench is not resolvable: %v", err)
	}
	for _, lib := range []string{"libggml.so", "libggml-base.so.0", "libllama.so"} {
		if _, err := os.Stat(filepath.Join(root, "lib", lib)); err != nil {
			t.Fatalf("lib/%s is not resolvable: %v", lib, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "bin", "LICENSE")); err == nil {
		t.Error("a non-executable data file was linked into bin/")
	}

	// The link must be RELATIVE, or it names the `.staging` directory forever
	// and dangles the moment the tree is published (D78).
	target, err := os.Readlink(filepath.Join(root, "bin", BinServer))
	if err != nil {
		t.Fatalf("bin/%s is not a symlink: %v", BinServer, err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("bin/%s links to the absolute path %q", BinServer, target)
	}

	// $ORIGIN is expanded from the RESOLVED path, so the real binary must still
	// live beside the libraries a flat release's RPATH looks for (D22).
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "bin", BinServer))
	if err != nil {
		t.Fatalf("resolve bin/%s: %v", BinServer, err)
	}
	if got, want := filepath.Dir(resolved), root; got != want {
		t.Errorf("the real binary resolved to %s, want it still in %s", got, want)
	}
}

func TestNormalizeLayoutLeavesANestedReleaseAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o750); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	writeExec(t, filepath.Join(root, "bin", BinServer))

	changed, err := NormalizeLayout(root)
	if err != nil {
		t.Fatalf("NormalizeLayout: %v", err)
	}
	if changed {
		t.Error("NormalizeLayout rewrote a tree that already had bin/llama-server")
	}
	fi, err := os.Lstat(filepath.Join(root, "bin", BinServer))
	if err != nil {
		t.Fatalf("lstat bin/%s: %v", BinServer, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a real binary was replaced by a symlink")
	}
}

// A tree with no server binary is not a llama.cpp release. Verify must be the
// one to say so, in its own words, rather than have this pass invent a failure
// or build an empty bin/ that turns "missing" into "will not execute".
func TestNormalizeLayoutDeclinesATreeWithNoServer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeExec(t, filepath.Join(root, "llama-bench"))

	changed, err := NormalizeLayout(root)
	if err != nil {
		t.Fatalf("NormalizeLayout: %v", err)
	}
	if changed {
		t.Error("NormalizeLayout normalized a tree with no llama-server")
	}
	if _, err := os.Stat(filepath.Join(root, "bin")); err == nil {
		t.Error("NormalizeLayout created bin/ for a tree it could not normalize")
	}
}
