package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Root registration and validation (DESIGN sections 3.7, 7.2a, D57, F17).
//
// A root is a hub DIRECTORY, and registering one answers four questions that the
// filesystem alone can answer: is the path one this daemon is allowed to use, is
// it there, can the service identity write to it, and does it support symlinks.
// The last one is F17 and is not a formality — the whole snapshot layout is
// relative symlinks into `blobs/`, and a filesystem that refuses them needs the
// copy-mode fallback and the warning that goes with it.

// The refusals. Each is a distinct value because each maps to a different
// documented response in section 3.7: `422 root_path_protected`,
// `422 root_not_writable`, and the ordinary 422 for a path that is not a
// directory or not absolute.
var (
	// ErrRootNotAbsolute rejects a relative path. A hub directory that depends
	// on a working directory is a root this daemon would resolve differently on
	// the next boot.
	ErrRootNotAbsolute = errors.New("hf/cache: a cache root must be an absolute path")
	// ErrRootProtected rejects a path under one of the prefixes the unit's
	// `ProtectSystem=full` mounts read-only (D57). The daemon would be unable to
	// write there no matter what the file mode says, and the honest moment to
	// say so is registration rather than the first download.
	ErrRootProtected = errors.New("hf/cache: a cache root may not live under a read-only system prefix")
	// ErrRootNotDirectory rejects a path that exists and is not a directory.
	ErrRootNotDirectory = errors.New("hf/cache: the cache root exists and is not a directory")
	// ErrRootNotWritable rejects a directory the service identity cannot write.
	// It is only fatal for a root that is to become PRIMARY — section 7.2a
	// excludes a `writable=0` root from promotion, and reads it happily.
	ErrRootNotWritable = errors.New("hf/cache: the cache root is not writable by this service identity")
)

// ProtectedPrefixes are the paths `ProtectSystem=full` mounts read-only for
// `llamaman.service` (section 5.4). They are listed here rather than derived
// from the unit, because the check has to work in `llamaman doctor` and in the
// wizard on a host where the unit has not been installed yet — and because the
// directive's meaning is fixed by systemd, not by our rendering of it.
//
// `/var` is deliberately ABSENT: `ProtectSystem=full` covers /usr, /boot, /efi
// and /etc only, and the `--dedicated-user` topology's cache lives under
// /var/lib by design (section 7.2 rule 4).
var ProtectedPrefixes = []string{"/usr", "/boot", "/efi", "/etc"}

// RootInfo is everything a `hf_cache_roots` row records about a hub directory
// that the filesystem is the authority for.
type RootInfo struct {
	// Path is the cleaned hub directory.
	Path string
	// Exists reports whether it was already there. A root that did not exist
	// and that Validate created reports true; one it only inspected reports
	// true as well — the field describes the directory now, not its history.
	Exists bool
	// Writable is `hf_cache_roots.writable`, established by creating and
	// removing a file rather than by reading the mode: the mode does not know
	// about a read-only mount, an ACL or a full filesystem, and all three are
	// ways a directory that looks writable is not.
	Writable bool
	// SymlinksOK is `hf_cache_roots.symlinks_ok`, the F17 probe.
	SymlinksOK bool
	// FSType, TotalBytes and FreeBytes come from statfs and are what
	// `GET /system/disk` reports per root. FreeBytes counts the space available
	// to an UNPRIVILEGED writer, which is what the download guard needs — the
	// root-reserved blocks are not ours to spend.
	FSType     string
	TotalBytes int64
	FreeBytes  int64
}

// ValidateOptions tunes what Validate is allowed to do.
type ValidateOptions struct {
	// Create makes Validate create the directory (and its parents) when it is
	// absent, 0755. Section 7.2a's SetPrimaryRoot passes true — "creatable" is
	// one of the things it validates — while a read-only inspection passes
	// false.
	Create bool
	// RequireWritable turns a non-writable directory into ErrRootNotWritable
	// rather than a `writable=0` fact. Only the primary root requires it.
	RequireWritable bool
	// ProtectedPrefixes overrides the package list, for a test that needs a
	// prefix inside its own temp directory.
	ProtectedPrefixes []string
}

// Validate inspects — and, with Create, materializes — a hub directory, and
// returns what the `hf_cache_roots` row should record.
//
// It is the one place the four questions are asked, so `POST /cache/roots`, the
// promote endpoint, `PATCH /settings {"hf.hub_dir"}` and the wizard's cache step
// cannot disagree about what a usable root is.
func Validate(path string, opts ValidateOptions) (RootInfo, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return RootInfo{}, fmt.Errorf("%w: %s", ErrRootNotAbsolute, path)
	}
	prefixes := opts.ProtectedPrefixes
	if prefixes == nil {
		prefixes = ProtectedPrefixes
	}
	if p, bad := underProtectedPrefix(clean, prefixes); bad {
		return RootInfo{}, fmt.Errorf("%w: %s is under %s", ErrRootProtected, clean, p)
	}

	info := RootInfo{Path: clean}
	st, err := os.Stat(clean)
	switch {
	case err == nil && !st.IsDir():
		return info, fmt.Errorf("%w: %s", ErrRootNotDirectory, clean)
	case err == nil:
		info.Exists = true
	case os.IsNotExist(err) && opts.Create:
		if err := os.MkdirAll(clean, DirMode); err != nil {
			return info, fmt.Errorf("hf/cache: create %s: %w", clean, err)
		}
		info.Exists = true
	case os.IsNotExist(err):
		// A root that is not there yet is not an error for a read-only
		// inspection: the detection chain's winner on a fresh host is exactly
		// this, and the wizard shows it as "will be created".
		return info, nil
	default:
		return info, fmt.Errorf("hf/cache: stat %s: %w", clean, err)
	}

	info.Writable, info.SymlinksOK = probe(clean)
	if opts.RequireWritable && !info.Writable {
		return info, fmt.Errorf("%w: %s", ErrRootNotWritable, clean)
	}
	info.FSType, info.TotalBytes, info.FreeBytes = statfs(clean)
	return info, nil
}

// underProtectedPrefix reports whether p is at or under one of the prefixes, by
// path SEGMENT rather than by string prefix — `/usrlocal` is not under `/usr`,
// and a check that thinks it is would refuse a legitimate root.
func underProtectedPrefix(p string, prefixes []string) (string, bool) {
	for _, prefix := range prefixes {
		clean := filepath.Clean(prefix)
		if p == clean || strings.HasPrefix(p, clean+string(filepath.Separator)) {
			return clean, true
		}
	}
	return "", false
}

// probe answers the two questions a stat cannot: can this identity write here,
// and does this filesystem keep a symlink.
//
// Both are answered by doing the thing, in a uniquely named temporary pair that
// is removed afterwards, because every cheaper answer is wrong somewhere: the
// mode bits do not know about a read-only mount or an ACL, and "is it ext4" does
// not know about a `noexec`-style mount option or an overlay whose upper layer
// refuses symlinks. F17's fallback costs real disk (copy mode instead of links),
// so the flag it turns on had better be measured.
func probe(dir string) (writable, symlinksOK bool) {
	f, err := os.CreateTemp(dir, ".llamaman-probe-*")
	if err != nil {
		return false, false
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	writable = true

	link := name + ".link"
	if err := os.Symlink(filepath.Base(name), link); err != nil {
		return writable, false
	}
	defer os.Remove(link)
	// Creating it is not enough: a filesystem may accept the call and store a
	// copy. Reading the link back is what distinguishes the two.
	if target, err := os.Readlink(link); err != nil || target != filepath.Base(name) {
		return writable, false
	}
	return writable, true
}

// statfs reports the filesystem type and the space on it. A failure yields zeros
// and an empty type rather than an error: free space is a display fact and a
// download guard input, and a root that cannot be measured is still a root that
// can be read.
func statfs(dir string) (fsType string, total, free int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", 0, 0
	}
	bs := int64(st.Bsize)
	// Bavail, not Bfree: the difference is the root-reserved blocks, which an
	// unprivileged service identity may not spend, and counting them would make
	// the section 7.4 disk guard pass on a filesystem that is already full for
	// us.
	return fsTypeName(int64(st.Type)), int64(st.Blocks) * bs, int64(st.Bavail) * bs
}

// fsTypeName maps the statfs magic numbers worth naming to their filesystem
// names. An unknown value is rendered as its hex magic rather than "unknown", so
// a bug report from an exotic filesystem carries the number somebody can look
// up.
func fsTypeName(magic int64) string {
	names := map[int64]string{
		0xEF53:     "ext4",
		0x58465342: "xfs",
		0x9123683E: "btrfs",
		0x01021994: "tmpfs",
		0x6969:     "nfs",
		0xFF534D42: "cifs",
		0x2FC12FC1: "zfs",
		0x65735546: "fuse",
		0x794C7630: "overlayfs",
		0x4D44:     "vfat",
		0x5346544E: "ntfs",
		0x0000EE01: "bcachefs",
		0x73717368: "squashfs",
		0x0000F15F: "ecryptfs",
	}
	if n, ok := names[magic]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", magic)
}
