package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
)

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	protected := filepath.Join(root, "usr")
	if err := os.MkdirAll(filepath.Join(protected, "share", "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		opts cache.ValidateOptions
		want error
	}{
		{
			// A hub directory that depends on a working directory is one this
			// daemon could resolve differently on the next boot.
			name: "a relative path", path: "relative/hub",
			want: cache.ErrRootNotAbsolute,
		},
		{
			// D57: `ProtectSystem=full` mounts these read-only, so the daemon
			// could not write there whatever the mode says. Registration is the
			// honest moment to refuse.
			name: "under a read-only system prefix",
			path: filepath.Join(protected, "share", "hub"),
			opts: cache.ValidateOptions{ProtectedPrefixes: []string{protected}},
			want: cache.ErrRootProtected,
		},
		{
			name: "an existing file", path: file,
			want: cache.ErrRootNotDirectory,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := cache.Validate(tc.path, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("Validate(%q) = %v, want %v", tc.path, err, tc.want)
			}
		})
	}
}

// TestProtectedPrefixMatchesBySegment: `/usrlocal` is not under `/usr`, and a
// string-prefix check that thinks it is would refuse a legitimate root.
func TestProtectedPrefixMatchesBySegment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	usr := filepath.Join(root, "usr")
	usrlocal := filepath.Join(root, "usrlocal")
	if err := os.MkdirAll(usrlocal, 0o755); err != nil {
		t.Fatal(err)
	}

	info, err := cache.Validate(usrlocal, cache.ValidateOptions{ProtectedPrefixes: []string{usr}})
	if err != nil {
		t.Fatalf("Validate(%q) = %v, want it accepted", usrlocal, err)
	}
	if !info.Exists {
		t.Fatal("the directory was not reported as existing")
	}
}

func TestValidateMeasuresTheFilesystem(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "hub")
	info, err := cache.Validate(dir, cache.ValidateOptions{Create: true, RequireWritable: true})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !info.Exists {
		t.Fatal("Create did not create the directory")
	}
	if !info.Writable {
		t.Fatal("a directory this process just created is not writable")
	}
	// F17: the probe writes a symlink and reads it back, because a filesystem
	// may accept the call and store a copy. Every filesystem a test runs on
	// keeps symlinks, so this is the positive half of the probe.
	if !info.SymlinksOK {
		t.Fatal("the symlink probe failed on a filesystem that supports them")
	}
	if info.TotalBytes <= 0 || info.FreeBytes <= 0 {
		t.Fatalf("statfs reported total=%d free=%d", info.TotalBytes, info.FreeBytes)
	}
	if info.FSType == "" {
		t.Fatal("no filesystem type was reported")
	}
}

// TestValidateAcceptsAnAbsentRootForInspection: the detection chain's winner on
// a fresh host does not exist yet, and the wizard shows it as "will be created".
func TestValidateAcceptsAnAbsentRootForInspection(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "not-yet")
	info, err := cache.Validate(dir, cache.ValidateOptions{})
	if err != nil {
		t.Fatalf("Validate of an absent directory = %v, want no error", err)
	}
	if info.Exists {
		t.Fatal("a directory that is not there was reported as existing")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a read-only inspection created the directory")
	}
}

func TestValidateRequiresWritableOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits, so this case cannot be produced")
	}

	// A read-only library is a perfectly good thing to serve models out of, so
	// registering it records `writable=0` rather than refusing.
	info, err := cache.Validate(dir, cache.ValidateOptions{})
	if err != nil {
		t.Fatalf("Validate = %v, want a writable=0 fact rather than an error", err)
	}
	if info.Writable {
		t.Fatal("a mode-0500 directory was reported writable")
	}

	// Promoting it is what is refused: only the primary root receives downloads.
	if _, err := cache.Validate(dir, cache.ValidateOptions{RequireWritable: true}); !errors.Is(err, cache.ErrRootNotWritable) {
		t.Fatalf("Validate with RequireWritable = %v, want ErrRootNotWritable", err)
	}
}
