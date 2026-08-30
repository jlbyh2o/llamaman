package prebuilt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stagedTree writes a staging directory with one identifying file in it.
func stagedTree(t *testing.T, root, id, marker string) string {
	t.Helper()
	dir := StagingDir(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", BinServer), []byte(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func serverContent(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "bin", BinServer))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return string(b)
}

type guard struct {
	pid   int
	inUse bool
	err   error
	asked int
}

func (g *guard) InUse(context.Context, string) (int, bool, error) {
	g.asked++
	return g.pid, g.inUse, g.err
}

func TestPublishFreshIsOneRename(t *testing.T) {
	root := t.TempDir()
	staging := stagedTree(t, root, "b10621-cpu-bin", "new")
	target := VersionDir(root, "b10621-cpu-bin")

	if err := Publish(t.Context(), PublishOptions{Staging: staging, Target: target}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := serverContent(t, target); got != "new" {
		t.Errorf("published content = %q", got)
	}
	if _, err := os.Stat(staging); err == nil {
		t.Error("the staging directory survived the publish")
	}
}

func TestPublishOverAnExistingDirectorySwaps(t *testing.T) {
	// D78's forced rebuild: `versions/active` names <id> and is never touched,
	// so the two renames leave it correct before and after.
	root := t.TempDir()
	id := "b10621-cuda-src"
	target := VersionDir(root, id)
	if err := os.MkdirAll(filepath.Join(target, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "bin", BinServer), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The activation symlink points at the id, by name.
	active := filepath.Join(root, "active")
	if err := os.Symlink(id, active); err != nil {
		t.Fatal(err)
	}
	staging := stagedTree(t, root, id, "rebuilt")

	g := &guard{}
	if err := Publish(t.Context(), PublishOptions{Staging: staging, Target: target, Guard: g}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if g.asked != 1 {
		t.Errorf("the live-process guard was asked %d times, want exactly 1 (immediately before the swap)", g.asked)
	}
	if got := serverContent(t, target); got != "rebuilt" {
		t.Errorf("content after the swap = %q, want rebuilt", got)
	}
	// The symlink was never touched and still resolves — through the new tree.
	resolved, err := filepath.EvalSymlinks(active)
	if err != nil {
		t.Fatalf("versions/active does not resolve after the swap: %v", err)
	}
	if got := serverContent(t, resolved); got != "rebuilt" {
		t.Errorf("versions/active resolves to content %q", got)
	}
	if _, err := os.Stat(target + oldSuffix); err == nil {
		t.Error("the displaced directory was left behind")
	}
}

func TestPublishRefusesWhileAProcessIsRunningFromTheTarget(t *testing.T) {
	// D25: renaming a directory out from under a running llama-server is the
	// one case that must be a refusal rather than a rebuild.
	root := t.TempDir()
	id := "b10621-cpu-bin"
	target := VersionDir(root, id)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := stagedTree(t, root, id, "rebuilt")

	g := &guard{pid: 4242, inUse: true}
	err := Publish(t.Context(), PublishOptions{Staging: staging, Target: target, Guard: g})
	if !errors.Is(err, ErrVersionInUse) {
		t.Fatalf("error = %v, want ErrVersionInUse (409 version_in_use)", err)
	}
	if _, statErr := os.Stat(staging); statErr != nil {
		t.Error("the staging tree was discarded by a refused publish")
	}
}

func TestPublishGuardFailureIsNotASilentSwap(t *testing.T) {
	root := t.TempDir()
	id := "b10621-cpu-bin"
	target := VersionDir(root, id)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := stagedTree(t, root, id, "rebuilt")

	g := &guard{err: errors.New("/proc unreadable")}
	if err := Publish(t.Context(), PublishOptions{Staging: staging, Target: target, Guard: g}); err == nil {
		t.Fatal("a guard that could not answer was treated as a no")
	}
}

func TestPublishClearsALeftoverOldDirectory(t *testing.T) {
	// A swap that died between its two renames leaves `<id>.old`, which would
	// make the next attempt's first rename fail.
	root := t.TempDir()
	id := "b10621-cpu-bin"
	target := VersionDir(root, id)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target+oldSuffix, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := stagedTree(t, root, id, "rebuilt")

	if err := Publish(t.Context(), PublishOptions{Staging: staging, Target: target}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := serverContent(t, target); got != "rebuilt" {
		t.Errorf("content = %q", got)
	}
}

func TestPublishNeedsAStagingTree(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		staging string
		target  string
	}{
		{name: "no staging path", target: VersionDir(root, "x")},
		{name: "no target path", staging: StagingDir(root, "x")},
		{name: "staging does not exist", staging: StagingDir(root, "absent"), target: VersionDir(root, "absent")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Publish(t.Context(), PublishOptions{Staging: tc.staging, Target: tc.target}); err == nil {
				t.Fatal("Publish succeeded")
			}
		})
	}
}

func TestCleanStagingRefusesAPublishedDirectory(t *testing.T) {
	// The guard that stops a mistyped argument from deleting a working version.
	root := t.TempDir()
	published := VersionDir(root, "b10621-cpu-bin")
	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanStaging(published); err == nil {
		t.Fatal("CleanStaging removed a published version directory")
	}
	if _, err := os.Stat(published); err != nil {
		t.Error("the published directory was removed anyway")
	}

	staging := stagedTree(t, root, "b10621-cpu-bin", "x")
	if err := CleanStaging(staging); err != nil {
		t.Fatalf("CleanStaging: %v", err)
	}
	if _, err := os.Stat(staging); err == nil {
		t.Error("the staging directory survived")
	}
	// A second removal of a directory that is already gone is success.
	if err := CleanStaging(staging); err != nil {
		t.Errorf("removing an absent staging directory failed: %v", err)
	}
	if err := CleanStaging(""); err != nil {
		t.Errorf("CleanStaging(\"\") = %v, want nil", err)
	}
}

func TestStagingAndVersionPaths(t *testing.T) {
	root := "/var/lib/llamaman/versions"
	if got := StagingDir(root, "b10621-cpu-bin"); got != root+"/b10621-cpu-bin.staging" {
		t.Errorf("StagingDir = %q", got)
	}
	if got := VersionDir(root, "b10621-cpu-bin"); got != root+"/b10621-cpu-bin" {
		t.Errorf("VersionDir = %q", got)
	}
}
