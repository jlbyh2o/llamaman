package prebuilt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// entry is one archive member, in the form the evil-archive table writes them.
type entry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	link     string
	size     int64 // overrides len(body) in the header, for the lying-header case
}

func file(name, body string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o644, body: body}
}

func exe(name, body string) entry {
	return entry{name: name, typeflag: tar.TypeReg, mode: 0o755, body: body}
}

func dir(name string) entry {
	return entry{name: name, typeflag: tar.TypeDir, mode: 0o755}
}

func symlink(name, target string) entry {
	return entry{name: name, typeflag: tar.TypeSymlink, mode: 0o777, link: target}
}

func hardlink(name, target string) entry {
	return entry{name: name, typeflag: tar.TypeLink, mode: 0o644, link: target}
}

// tarGz builds a gzip-compressed tar in memory.
func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.link,
			Size:     size,
		}
		if e.typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// goodArchive is the shape upstream actually ships: one top-level directory
// holding bin/ and lib/, with a versioned shared library and a symlink to it.
func goodArchive(t *testing.T) []byte {
	t.Helper()
	return tarGz(t,
		dir("build"),
		dir("build/bin"),
		exe("build/bin/llama-server", "ELF-ish server"),
		exe("build/bin/llama-bench", "ELF-ish bench"),
		// Deliberately NOT executable in the archive: section 6.4's `chmod +x
		// bin/*` is what makes it runnable.
		file("build/bin/llama-cli", "ELF-ish cli"),
		dir("build/lib"),
		file("build/lib/libggml.so.0", "so bytes"),
		symlink("build/lib/libggml.so", "libggml.so.0"),
	)
}

func TestExtractHappyPath(t *testing.T) {
	dest := t.TempDir()
	res, err := ExtractTarGz(bytes.NewReader(goodArchive(t)), dest, DefaultExtractOptions())
	if err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}

	if res.TopLevel != "build" {
		t.Errorf("top level = %q, want build", res.TopLevel)
	}
	// The strip is what turns `build/bin/llama-server` into `bin/llama-server`,
	// which is the path every other part of this design names.
	for _, want := range []string{"bin/llama-server", "bin/llama-bench", "bin/llama-cli", "lib/libggml.so.0"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("%s missing after extraction: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "build")); err == nil {
		t.Error("the top-level directory was not stripped")
	}

	// chmod +x bin/*, including the entry whose archive mode lacked the bit.
	for _, name := range []string{"llama-server", "llama-bench", "llama-cli"} {
		fi, err := os.Stat(filepath.Join(dest, "bin", name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("bin/%s is not executable (mode %v)", name, fi.Mode())
		}
	}
	if !slices.Equal(res.Executables, []string{"bin/llama-bench", "bin/llama-cli", "bin/llama-server"}) {
		t.Errorf("executables = %v", res.Executables)
	}

	// The library symlink survives, because upstream trees depend on it.
	link, err := os.Readlink(filepath.Join(dest, "lib", "libggml.so"))
	if err != nil || link != "libggml.so.0" {
		t.Errorf("lib/libggml.so = %q, %v; want a relative link to libggml.so.0", link, err)
	}
	if res.Files != 4 || res.Symlinks != 1 {
		t.Errorf("counted %d files and %d symlinks", res.Files, res.Symlinks)
	}
	if res.Bytes == 0 {
		t.Error("no byte total recorded")
	}
}

// TestExtractRejectsEvilArchives is the hardening table. Every case is an
// archive that would write outside the destination, create something a compiler
// never emits, or exhaust the disk — and every one must be refused with an
// error that identifies itself as ErrUnsafeArchive rather than as an I/O error.
func TestExtractRejectsEvilArchives(t *testing.T) {
	big := strings.Repeat("A", 4096)

	tests := []struct {
		name    string
		entries []entry
		opts    func(ExtractOptions) ExtractOptions
		wantIn  string
	}{
		{
			name:    "absolute path",
			entries: []entry{dir("build"), file("/etc/cron.d/pwned", "* * * * * root sh")},
			wantIn:  "absolute path",
		},
		{
			name:    "parent traversal",
			entries: []entry{dir("build"), file("build/../../../etc/passwd", "root::0:0")},
			wantIn:  "traverses out of the destination",
		},
		{
			name:    "traversal hidden in the middle",
			entries: []entry{dir("build"), file("build/bin/../../../../tmp/x", "x")},
			wantIn:  "traverses out of the destination",
		},
		{
			name:    "symlink escaping to an absolute path",
			entries: []entry{dir("build"), symlink("build/lib", "/etc")},
			wantIn:  "absolute path",
		},
		{
			name:    "symlink escaping by traversal",
			entries: []entry{dir("build"), dir("build/bin"), symlink("build/bin/out", "../../../../etc")},
			wantIn:  "resolves outside the destination",
		},
		{
			name: "symlink followed by a write through it",
			// The classic two-entry escape. The link is refused on sight; if it
			// somehow were not, O_NOFOLLOW and mkdirNoFollow refuse the write.
			entries: []entry{dir("build"), symlink("build/lib", "../../../../etc"), file("build/lib/passwd", "x")},
			wantIn:  "resolves outside the destination",
		},
		{
			name:    "hardlink to a path outside the archive",
			entries: []entry{dir("build"), hardlink("build/shadow", "/etc/shadow")},
			wantIn:  "hardlink",
		},
		{
			name:    "hardlink to a file that was never extracted",
			entries: []entry{dir("build"), hardlink("build/x", "build/never-written")},
			wantIn:  "hardlink",
		},
		{
			name:    "character device",
			entries: []entry{dir("build"), {name: "build/mem", typeflag: tar.TypeChar, mode: 0o666}},
			wantIn:  "entry type",
		},
		{
			name:    "block device",
			entries: []entry{dir("build"), {name: "build/sda", typeflag: tar.TypeBlock, mode: 0o660}},
			wantIn:  "entry type",
		},
		{
			name:    "fifo",
			entries: []entry{dir("build"), {name: "build/pipe", typeflag: tar.TypeFifo, mode: 0o644}},
			wantIn:  "entry type",
		},
		{
			name:    "two top-level directories",
			entries: []entry{dir("build"), file("build/bin/x", "x"), file("other/y", "y")},
			wantIn:  "more than one top-level entry",
		},
		{
			name:    "the same file twice",
			entries: []entry{dir("build"), file("build/bin/x", "first"), file("build/bin/x", "second")},
			wantIn:  "twice",
		},
		{
			name:    "backslash in the name",
			entries: []entry{dir("build"), file(`build\..\..\evil`, "x")},
			wantIn:  "backslash",
		},
		{
			name:    "one file over the per-file limit",
			entries: []entry{dir("build"), file("build/huge", big)},
			opts:    func(o ExtractOptions) ExtractOptions { o.MaxFileBytes = 1024; return o },
			wantIn:  "over the",
		},
		{
			name:    "many files over the total limit",
			entries: []entry{dir("build"), file("build/a", big), file("build/b", big), file("build/c", big)},
			opts:    func(o ExtractOptions) ExtractOptions { o.MaxTotalBytes = 5000; return o },
			wantIn:  "uncompressed size exceeds",
		},
		{
			name:    "too many entries",
			entries: []entry{dir("build"), file("build/a", "a"), file("build/b", "b"), file("build/c", "c")},
			opts:    func(o ExtractOptions) ExtractOptions { o.MaxEntries = 2; return o },
			wantIn:  "more than 2 entries",
		},
		{
			name:    "symlinks forbidden outright",
			entries: []entry{dir("build"), dir("build/lib"), symlink("build/lib/x", "y")},
			opts:    func(o ExtractOptions) ExtractOptions { o.AllowSymlinks = false; return o },
			wantIn:  "symlinks are not permitted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			// A canary outside the destination: nothing this test does may
			// touch it, whatever the archive claims.
			outside := filepath.Join(t.TempDir(), "canary")
			if err := os.WriteFile(outside, []byte("untouched"), 0o644); err != nil {
				t.Fatal(err)
			}

			opts := DefaultExtractOptions()
			if tc.opts != nil {
				opts = tc.opts(opts)
			}
			_, err := ExtractTarGz(bytes.NewReader(tarGz(t, tc.entries...)), dest, opts)
			if err == nil {
				t.Fatal("the archive was accepted")
			}
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Errorf("error %v does not identify itself as ErrUnsafeArchive", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not contain %q", err, tc.wantIn)
			}

			b, readErr := os.ReadFile(outside)
			if readErr != nil || string(b) != "untouched" {
				t.Errorf("the canary outside the destination was modified: %q, %v", b, readErr)
			}
		})
	}
}

func TestExtractStripsSetuidBits(t *testing.T) {
	// A setuid file in a directory the service identity can write is a local
	// privilege-escalation primitive. An archive does not get to create one.
	dest := t.TempDir()
	arch := tarGz(t,
		dir("build"),
		dir("build/bin"),
		entry{name: "build/bin/llama-server", typeflag: tar.TypeReg, mode: 0o4755, body: "x"},
		entry{name: "build/bin/llama-bench", typeflag: tar.TypeReg, mode: 0o2775, body: "x"},
		entry{name: "build/data", typeflag: tar.TypeReg, mode: 0o666, body: "x"},
	)
	if _, err := ExtractTarGz(bytes.NewReader(arch), dest, DefaultExtractOptions()); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	tests := []struct {
		path string
		want fs.FileMode
	}{
		{path: "bin/llama-server", want: 0o755},
		{path: "bin/llama-bench", want: 0o755},
		{path: "data", want: 0o644},
	}
	for _, tc := range tests {
		fi, err := os.Stat(filepath.Join(dest, tc.path))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s mode = %v, want %v", tc.path, got, tc.want)
		}
		if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Errorf("%s kept a setuid/setgid/sticky bit: %v", tc.path, fi.Mode())
		}
	}
}

func TestExtractAllowsSymlinksThatStayInside(t *testing.T) {
	dest := t.TempDir()
	arch := tarGz(t,
		dir("build"),
		dir("build/lib"),
		file("build/lib/libggml.so.0", "so"),
		symlink("build/lib/libggml.so", "libggml.so.0"),
		// A link into a sibling directory is still inside the destination.
		dir("build/bin"),
		symlink("build/bin/lib", "../lib"),
	)
	res, err := ExtractTarGz(bytes.NewReader(arch), dest, DefaultExtractOptions())
	if err != nil {
		t.Fatalf("a tree of internal symlinks was refused: %v", err)
	}
	if res.Symlinks != 2 {
		t.Errorf("symlinks = %d, want 2", res.Symlinks)
	}
}

// TestExtractRefusesChainedSymlinkEscapes is the case a lexical check passes and
// the kernel does not. Each archive plants a link that resolves to somewhere
// inside, then a second link whose target walks back OUT through it — which
// `path.Join` folds away as though the first component were a real directory.
//
// The published tree matters as much as the staging one: staging is
// `<state_dir>/versions/<id>.staging`, so `here/../../secret.key` from such a
// link names the AES-GCM key file of DESIGN section 2.2, and the link survives
// the rename into `versions/<id>/`.
func TestExtractRefusesChainedSymlinkEscapes(t *testing.T) {
	// Every target below is LEXICALLY clean: `path.Join(path.Dir(link), target)`
	// stays inside the destination for all three, which is exactly why a lexical
	// check accepts them.
	cases := []struct {
		name    string
		entries []entry
	}{
		{
			name: "a link to the current directory, walked back out of",
			entries: []entry{
				dir("build"),
				symlink("build/here", "."),
				symlink("build/leak", "here/../secret.key"),
			},
		},
		{
			name: "a chain of two inside links that together ascend",
			entries: []entry{
				dir("build"), dir("build/bin"),
				symlink("build/bin/a", "."),
				symlink("build/bin/b", "a/.."),
				symlink("build/bin/leak", "b/../secret.key"),
			},
		},
		{
			name: "the link's own directory is a link that ascends",
			entries: []entry{
				dir("build"), dir("build/bin"),
				symlink("build/up", "bin/.."),
				symlink("build/up/x", "../secret.key"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The file the escape is aimed at exists, one level above the
			// destination, so filepath.EvalSymlinks can resolve a successful
			// escape rather than reporting a dangling link.
			base := t.TempDir()
			if err := os.WriteFile(filepath.Join(base, "secret.key"), []byte("k"), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(base, "staging")
			if err := os.MkdirAll(dest, 0o750); err != nil {
				t.Fatal(err)
			}
			arch := tarGz(t, tc.entries...)
			_, err := ExtractTarGz(bytes.NewReader(arch), dest, DefaultExtractOptions())
			if err == nil {
				t.Fatalf("the archive was accepted; %s", describeEscape(t, dest))
			}
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Errorf("error = %v, want one wrapping ErrUnsafeArchive", err)
			}
			// Whatever was written before the refusal, nothing under the
			// destination may resolve outside it.
			if msg := describeEscape(t, dest); msg != "" {
				t.Errorf("the extraction was refused but %s", msg)
			}
		})
	}
}

// describeEscape reports the first entry under dest that resolves outside it,
// which is the property the lexical check silently failed to guarantee.
func describeEscape(t *testing.T, dest string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return ""
	}
	var found string
	_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || p == dest {
			return nil //nolint:nilerr // an unreadable entry cannot be the escape we are describing
		}
		resolved, rerr := filepath.EvalSymlinks(p)
		if rerr != nil {
			// A dangling link is still an escape when its lexical parent is
			// outside; a link into a directory that does not exist is not
			// something this test can resolve, so it is left alone.
			return nil
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			found = p + " resolves to " + resolved + ", outside " + root
		}
		return nil
	})
	return found
}

func TestExtractWithoutStripping(t *testing.T) {
	dest := t.TempDir()
	opts := DefaultExtractOptions()
	opts.StripTopLevel = false
	arch := tarGz(t, dir("a"), file("a/x", "1"), dir("b"), file("b/y", "2"))

	res, err := ExtractTarGz(bytes.NewReader(arch), dest, opts)
	if err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	if res.TopLevel != "" {
		t.Errorf("top level = %q, want empty when nothing is stripped", res.TopLevel)
	}
	for _, want := range []string{"a/x", "b/y"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("%s missing: %v", want, err)
		}
	}
}

func TestExtractRejectsANonGzipStream(t *testing.T) {
	dest := t.TempDir()
	_, err := ExtractTarGz(strings.NewReader("this is not gzip"), dest, DefaultExtractOptions())
	if err == nil {
		t.Fatal("a non-gzip stream was accepted")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("error %q does not say what was wrong", err)
	}
}

func TestExtractRequiresAnExistingDestination(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	_, err := ExtractTarGz(bytes.NewReader(goodArchive(t)), missing, DefaultExtractOptions())
	if err == nil {
		t.Fatal("extraction into a missing directory succeeded")
	}
}

func TestExtractHardlinkToAnExtractedFile(t *testing.T) {
	// A hardlink to a regular file this same extraction already wrote is the
	// one legitimate use, and it must keep working.
	dest := t.TempDir()
	arch := tarGz(t,
		dir("build"),
		dir("build/bin"),
		exe("build/bin/llama-server", "server"),
		hardlink("build/bin/llama-server-alias", "build/bin/llama-server"),
	)
	if _, err := ExtractTarGz(bytes.NewReader(arch), dest, DefaultExtractOptions()); err != nil {
		t.Fatalf("a legitimate hardlink was refused: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "bin", "llama-server-alias"))
	if err != nil || string(b) != "server" {
		t.Errorf("hardlink content = %q, %v", b, err)
	}
}
