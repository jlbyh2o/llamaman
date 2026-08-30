package prebuilt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Normalizing an extracted tree to DESIGN section 6.1's `{bin,lib}` layout.
//
// StripTopLevel's own doc comment describes upstream's archives as
// `build/bin/llama-server`, so that stripping one directory leaves
// `bin/llama-server`. That was true of the tarballs the pipeline was written
// against and is NOT true of the current ones: `llama-bNNNN-bin-ubuntu-x64.tar.gz`
// carries exactly one top-level directory, `llama-bNNNN/`, and every binary and
// every `.so` sits FLAT inside it — there is no `bin/` and no `lib/` at all.
// Stripping the top level therefore flattened the whole release into the staging
// root, and `Verify` then could not find `bin/llama-server` and failed the
// install with `build_failed`. Because D18's source-build fallback does not fire
// for that failure class, the CPU prebuilt channel — the wizard's one
// non-skippable step — was a dead end on every host.
//
// The fix is a normalization pass rather than a change to the extractor,
// because BOTH layouts are real and a downloaded archive is not ours to choose:
// an archive that already has `bin/` is left exactly as it was, and a flat one
// grows a `bin/` and a `lib/` view of itself.
//
// # Why the view is symlinks and not a move
//
// A version tree is self-contained through its RPATH (D22), and the two layouts
// carry DIFFERENT RPATHs because each is the only one that works for its own
// shape: a nested build links `$ORIGIN/../lib`, a flat one links `$ORIGIN`. The
// dynamic loader expands `$ORIGIN` from the RESOLVED path of the executable, so
// a symlink at `bin/llama-server` pointing to `../llama-server` leaves `$ORIGIN`
// as the directory the real file is in — where the flat archive's libraries
// actually are. Moving the files instead would put `$ORIGIN` at `bin/` and break
// the very archives this pass exists to support, and hardlinking has the same
// defect for the same reason.
//
// So the flat files stay where the archive put them, and `bin/` and `lib/`
// become the names the rest of this project uses to find them: `Verify` stats
// `bin/llama-server`, `Runtime.ServerPath()` renders `<dir>/bin/llama-server`
// into every instance's argv, and the manifest lists `bin/`'s entries.

// SharedLibSuffix identifies a shared object by name. `.so` alone is not enough:
// a release ships `libggml.so.0` and `libllama.so.0.0.0` beside the bare names.
const SharedLibSuffix = ".so"

// NormalizeLayout gives an extracted tree the `{bin,lib}` shape section 6.1
// specifies, and reports whether it had to do anything.
//
// It is a no-op — reporting false — for a tree that already has a `bin/`
// directory, which is every archive built the way StripTopLevel was written for.
// It is also a no-op for a tree with no executable at its root, because that is
// not a layout this pass can rescue and `Verify` should say so in its own words
// rather than have this function invent a failure.
func NormalizeLayout(root string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, "bin", BinServer)); err == nil {
		return false, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return false, fmt.Errorf("prebuilt: reading %s: %w", root, err)
	}

	var bins, libs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		switch {
		case isSharedLib(name):
			libs = append(libs, name)
		case isExecutable(root, e):
			bins = append(bins, name)
		}
	}
	sort.Strings(bins)
	sort.Strings(libs)

	// The server binary is the one file that makes a tree a llama.cpp release.
	// Without it there is nothing to normalize INTO a valid layout, and building
	// an empty `bin/` would turn Verify's precise "bin/llama-server is missing
	// from the archive" into a confusing "it exists but will not execute".
	if !contains(bins, BinServer) {
		return false, nil
	}

	if err := linkInto(root, "bin", bins); err != nil {
		return false, err
	}
	if len(libs) > 0 {
		if err := linkInto(root, "lib", libs); err != nil {
			return false, err
		}
	}
	return true, nil
}

// linkInto creates dir under root and fills it with relative symlinks back to
// the named files at the root.
//
// An existing entry is left alone rather than replaced: a tree that is partly
// normalized is one a previous attempt was interrupted in, and the files it
// already linked are correct.
func linkInto(root, dir string, names []string) error {
	target := filepath.Join(root, dir)
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("prebuilt: creating %s: %w", target, err)
	}
	for _, name := range names {
		link := filepath.Join(target, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("prebuilt: checking %s: %w", link, err)
		}
		// A RELATIVE target, so the tree survives the `.staging` → final
		// rename that publishes it (D78). An absolute link would name the
		// staging directory forever and dangle the moment it is published.
		if err := os.Symlink(filepath.Join("..", name), link); err != nil {
			return fmt.Errorf("prebuilt: linking %s: %w", link, err)
		}
	}
	return nil
}

// isSharedLib reports whether a file name is a shared object, including the
// versioned forms (`libggml.so.0`, `libllama.so.0.0.0`).
func isSharedLib(name string) bool {
	return strings.HasSuffix(name, SharedLibSuffix) ||
		strings.Contains(name, SharedLibSuffix+".")
}

// isExecutable reports whether a directory entry is a regular file with an
// execute bit.
//
// The mode is what decides, not the name: llama.cpp's release binaries have no
// extension and no common prefix beyond `llama-`, and a name-based rule would
// have to be updated every time upstream adds a tool.
func isExecutable(root string, e fs.DirEntry) bool {
	info, err := e.Info()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func contains(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}
