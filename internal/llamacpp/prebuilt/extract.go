package prebuilt

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// The hardened extractor (DESIGN section 6.4 step 2): "into
// `versions/<id>.staging/` with a hardened tar reader that rejects absolute
// paths, `..` traversal, symlinks escaping the root, and device nodes, and
// strips the archive's top-level directory. `chmod +x bin/*`."
//
// Everything below exists because a tarball is ATTACKER-SHAPED DATA even when
// it comes from a host we trust: it is fetched over the network, and the
// process that unpacks it runs as the service identity with write access to the
// state directory and the model cache. The rules, and the concrete attack each
// one closes:
//
//	absolute path `/etc/cron.d/x`     → writes outside the destination entirely
//	`../../.ssh/authorized_keys`      → the same, by traversal
//	a symlink `lib -> /etc`, then a   → the classic two-entry escape: the second
//	  regular file `lib/passwd`          entry is written THROUGH the first
//	a symlink `x -> /etc/shadow`,     → the same escape in the other direction,
//	  then a hardlink to `x`             using a link rather than a write
//	a device node, fifo or socket     → mknod is not something an archive gets
//	setuid/setgid bits                → a setuid binary in a directory the
//	                                     service identity writes is a local root
//	a 4 GiB file, or 10^6 entries     → a decompression bomb fills the disk that
//	                                     holds the database
//
// The symlink-write-through case is closed twice over, deliberately: escaping
// links are rejected when they are created, AND every regular file is opened
// with O_CREATE|O_EXCL|O_NOFOLLOW, so even a link this reader wrongly allowed
// cannot be written through. Defense that depends on one check being right is
// not defense.

// Extraction limits. They are generous compared with a real llama.cpp tarball
// (~40 MB compressed, ~150 MB unpacked, a few hundred entries) and small
// compared with a disk.
const (
	DefaultMaxEntries    = 20_000
	DefaultMaxFileBytes  = int64(4) << 30
	DefaultMaxTotalBytes = int64(8) << 30
	// maxPathLen bounds one entry's path. Longer than this is not a real file
	// name; it is an attempt to find a buffer somewhere downstream.
	maxPathLen = 4096
)

// ErrUnsafeArchive is the class every hardening rejection belongs to, so a
// caller can tell "this tarball is hostile or corrupt" from "the disk is full"
// with errors.Is rather than by reading a message.
var ErrUnsafeArchive = errors.New("prebuilt: unsafe archive")

// ExtractOptions configures one extraction.
type ExtractOptions struct {
	// StripTopLevel removes the archive's single top-level directory, which is
	// what turns upstream's `build/bin/llama-server` into `bin/llama-server`.
	// An archive with more than one top-level entry is REJECTED rather than
	// flattened: guessing which directory was meant is how a `bin/` ends up
	// missing three files.
	StripTopLevel bool
	MaxEntries    int
	MaxFileBytes  int64
	MaxTotalBytes int64
	// AllowSymlinks permits symlinks whose target stays inside the destination.
	// Upstream tarballs contain `libggml.so -> libggml.so.0`, so this defaults
	// to true; an escaping link is refused either way.
	AllowSymlinks bool
}

// DefaultExtractOptions is what the prebuilt pipeline uses.
func DefaultExtractOptions() ExtractOptions {
	return ExtractOptions{
		StripTopLevel: true,
		MaxEntries:    DefaultMaxEntries,
		MaxFileBytes:  DefaultMaxFileBytes,
		MaxTotalBytes: DefaultMaxTotalBytes,
		AllowSymlinks: true,
	}
}

func (o ExtractOptions) withDefaults() ExtractOptions {
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	return o
}

// ExtractResult is what an extraction produced.
type ExtractResult struct {
	// TopLevel is the directory that was stripped, empty when none was.
	TopLevel string
	Files    int
	Dirs     int
	Symlinks int
	// Bytes is the total uncompressed size written.
	Bytes int64
	// Executables are the paths under bin/ that were made executable, relative
	// to the destination and sorted.
	Executables []string
}

// ExtractTarGz unpacks a gzip-compressed tar stream into dest, which must
// already exist. It never writes outside dest, and it returns an error wrapping
// ErrUnsafeArchive for anything it refuses.
func ExtractTarGz(r io.Reader, dest string, opts ExtractOptions) (ExtractResult, error) {
	opts = opts.withDefaults()

	gz, err := gzip.NewReader(r)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("prebuilt: opening the gzip stream: %w", err)
	}
	defer gz.Close()

	root, err := filepath.Abs(dest)
	if err != nil {
		return ExtractResult{}, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ExtractResult{}, fmt.Errorf("prebuilt: destination %s is not a directory", dest)
	}

	var res ExtractResult
	tr := tar.NewReader(gz)
	entries := 0
	// Every symlink accepted so far, so the next one's target is resolved
	// through them rather than lexically (see linkTree).
	links := linkTree{}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("prebuilt: reading the archive: %w", err)
		}
		entries++
		if entries > opts.MaxEntries {
			return res, fmt.Errorf("%w: more than %d entries", ErrUnsafeArchive, opts.MaxEntries)
		}

		rel, skip, err := entryPath(hdr.Name, opts, &res)
		if err != nil {
			return res, err
		}
		if skip {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := mkdirNoFollow(root, rel); err != nil {
				return res, err
			}
			res.Dirs++

		case tar.TypeReg:
			if hdr.Size > opts.MaxFileBytes {
				return res, fmt.Errorf("%w: %s is %d bytes, over the %d limit",
					ErrUnsafeArchive, rel, hdr.Size, opts.MaxFileBytes)
			}
			if res.Bytes+hdr.Size > opts.MaxTotalBytes {
				return res, fmt.Errorf("%w: uncompressed size exceeds %d bytes",
					ErrUnsafeArchive, opts.MaxTotalBytes)
			}
			if err := mkdirNoFollow(root, path.Dir(rel)); err != nil {
				return res, err
			}
			n, err := writeFile(target, tr, fileMode(hdr.Mode), opts.MaxFileBytes)
			if err != nil {
				return res, err
			}
			res.Files++
			res.Bytes += n

		case tar.TypeSymlink:
			if !opts.AllowSymlinks {
				return res, fmt.Errorf("%w: %s is a symlink and symlinks are not permitted here",
					ErrUnsafeArchive, rel)
			}
			if err := checkLinkTarget(links, rel, hdr.Linkname); err != nil {
				return res, err
			}
			if err := mkdirNoFollow(root, path.Dir(rel)); err != nil {
				return res, err
			}
			if err := os.Symlink(filepath.FromSlash(hdr.Linkname), target); err != nil {
				return res, fmt.Errorf("prebuilt: creating symlink %s: %w", rel, err)
			}
			links[rel] = hdr.Linkname
			res.Symlinks++

		case tar.TypeLink:
			// A hardlink may only point at a regular file this same extraction
			// already wrote — never at a path outside, and never at a symlink,
			// which would inherit that link's escape.
			linkRel, _, err := entryPath(hdr.Linkname, opts, nil)
			if err != nil {
				return res, fmt.Errorf("%w: hardlink %s targets %q: %w",
					ErrUnsafeArchive, rel, hdr.Linkname, err)
			}
			source := filepath.Join(root, filepath.FromSlash(linkRel))
			fi, err := os.Lstat(source)
			if err != nil || !fi.Mode().IsRegular() {
				return res, fmt.Errorf("%w: hardlink %s targets %q, which is not a regular file already extracted here",
					ErrUnsafeArchive, rel, hdr.Linkname)
			}
			if err := mkdirNoFollow(root, path.Dir(rel)); err != nil {
				return res, err
			}
			if err := os.Link(source, target); err != nil {
				return res, fmt.Errorf("prebuilt: creating hardlink %s: %w", rel, err)
			}
			res.Files++

		default:
			// Character and block devices, fifos, sockets, and anything else a
			// tar can name. None of them belong in a compiler's output.
			return res, fmt.Errorf("%w: %s has entry type %q, which is not a file, directory or link",
				ErrUnsafeArchive, rel, string(hdr.Typeflag))
		}
	}

	execs, err := makeBinExecutable(root)
	if err != nil {
		return res, err
	}
	res.Executables = execs
	return res, nil
}

// entryPath validates one archive path and applies the top-level strip. `skip`
// is true for the stripped top-level directory entry itself.
//
// `res` may be nil, which is how a hardlink target is validated without being
// counted as an entry.
func entryPath(name string, opts ExtractOptions, res *ExtractResult) (string, bool, error) {
	if strings.ContainsRune(name, 0) {
		return "", false, fmt.Errorf("%w: an entry name contains a NUL byte", ErrUnsafeArchive)
	}
	if len(name) > maxPathLen {
		return "", false, fmt.Errorf("%w: an entry name is %d bytes long", ErrUnsafeArchive, len(name))
	}
	// Normalize separators the way tar specifies them — always `/` — and drop
	// the `./` prefix GNU tar writes.
	clean := strings.TrimPrefix(name, "./")
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" || clean == "." {
		return "", true, nil
	}
	if path.IsAbs(clean) || strings.HasPrefix(name, "/") {
		return "", false, fmt.Errorf("%w: %q is an absolute path", ErrUnsafeArchive, name)
	}
	// A Windows-style drive or backslash path is not something we unpack.
	if strings.Contains(clean, `\`) {
		return "", false, fmt.Errorf("%w: %q contains a backslash", ErrUnsafeArchive, name)
	}
	parts := strings.Split(clean, "/")
	for _, p := range parts {
		switch p {
		case "..":
			return "", false, fmt.Errorf("%w: %q traverses out of the destination", ErrUnsafeArchive, name)
		case "", ".":
			return "", false, fmt.Errorf("%w: %q has an empty path component", ErrUnsafeArchive, name)
		}
	}

	if !opts.StripTopLevel {
		return clean, false, nil
	}
	if res == nil {
		// Hardlink targets are validated against the same strip as the entries.
		if len(parts) == 1 {
			return "", false, fmt.Errorf("%w: %q is the top-level directory", ErrUnsafeArchive, name)
		}
		return strings.Join(parts[1:], "/"), false, nil
	}
	if res.TopLevel == "" {
		res.TopLevel = parts[0]
	}
	if parts[0] != res.TopLevel {
		return "", false, fmt.Errorf("%w: the archive has more than one top-level entry (%q and %q)",
			ErrUnsafeArchive, res.TopLevel, parts[0])
	}
	if len(parts) == 1 {
		return "", true, nil // the top-level directory entry itself
	}
	return strings.Join(parts[1:], "/"), false, nil
}

// linkTree records every symlink this extraction has created, keyed by its path
// relative to the destination root, so a later entry's target can be resolved
// THROUGH them instead of lexically.
//
// It exists because `path.Join` is not a path resolver. `path.Join("build",
// "here/../secret.key")` collapses `here/..` as though `here` were a real
// directory, and answers `build/secret.key`; the kernel resolves `here` FIRST,
// and if the archive planted `build/here -> .` a moment earlier the same target
// lands one level above `build`. That is the whole escape, and it needs three
// ordinary-looking entries:
//
//	dir      build/
//	symlink  build/here -> .
//	symlink  build/leak -> here/../secret.txt
//
// Lexically the second link stays inside; followed, it does not. Since staging
// is `<state_dir>/versions/<id>.staging`, `here/../../secret.key` from such a
// link names the AES-GCM key file of DESIGN section 2.2 — and the link survives
// the rename into the published `versions/<id>/` tree, which is what makes this
// worth resolving properly rather than tightening a string test.
//
// Writes through such a link were already impossible (mkdirNoFollow refuses to
// descend a symlink and writeFile opens O_EXCL|O_NOFOLLOW), so this is not the
// only thing standing between an archive and the key. It is what makes this
// file's header — "symlinks escaping the root" are rejected — true as written.
type linkTree map[string]string

// maxLinkSteps bounds the resolution below. A symlink cycle (`a -> b`, `b -> a`)
// is resolvable only by giving up, and an archive that needs more than this many
// steps to name one path is not a compiler's output.
const maxLinkSteps = 512

// linkWalker resolves a slash path component by component against a linkTree,
// keeping the components it has entered on one shared stack. `..` pops that
// stack, and popping an empty stack IS the escape — which is why this is a
// resolver rather than a lexical check.
type linkWalker struct {
	links linkTree
	stack []string
	steps int
}

var errLinkEscape = errors.New("resolves outside the destination")

func (w *linkWalker) walk(p string) error {
	for _, part := range strings.Split(p, "/") {
		w.steps++
		if w.steps > maxLinkSteps {
			return fmt.Errorf("follows more than %d symlink steps", maxLinkSteps)
		}
		switch part {
		case "", ".":
			continue
		case "..":
			if len(w.stack) == 0 {
				return errLinkEscape
			}
			w.stack = w.stack[:len(w.stack)-1]
			continue
		}
		w.stack = append(w.stack, part)
		target, ok := w.links[strings.Join(w.stack, "/")]
		if !ok {
			continue
		}
		if path.IsAbs(target) {
			return errLinkEscape
		}
		// Step back out of the link itself: a symlink's target is resolved
		// relative to the directory the link sits in, exactly as the kernel
		// resolves it.
		w.stack = w.stack[:len(w.stack)-1]
		if err := w.walk(target); err != nil {
			return err
		}
	}
	return nil
}

// checkLinkTarget refuses a symlink that would resolve outside the destination,
// following the links this extraction has already created (see linkTree).
func checkLinkTarget(links linkTree, linkRel, target string) error {
	if target == "" {
		return fmt.Errorf("%w: symlink %s has an empty target", ErrUnsafeArchive, linkRel)
	}
	if strings.ContainsRune(target, 0) {
		return fmt.Errorf("%w: symlink %s has a NUL in its target", ErrUnsafeArchive, linkRel)
	}
	if path.IsAbs(target) {
		return fmt.Errorf("%w: symlink %s points at the absolute path %q", ErrUnsafeArchive, linkRel, target)
	}
	w := &linkWalker{links: links}
	// The link's own directory first — its components may themselves be links
	// this archive planted — and only then the target.
	if err := w.walk(path.Dir(linkRel)); err != nil {
		return fmt.Errorf("%w: symlink %s sits under a path that %w", ErrUnsafeArchive, linkRel, err)
	}
	if err := w.walk(target); err != nil {
		return fmt.Errorf("%w: symlink %s points at %q, which %w",
			ErrUnsafeArchive, linkRel, target, err)
	}
	return nil
}

// mkdirNoFollow creates rel under root one component at a time, refusing to
// descend through a symlink.
//
// os.MkdirAll would happily walk through a `lib -> /etc` an earlier entry
// created, and create directories on the other side of it. This does not: every
// existing component must be a real directory.
func mkdirNoFollow(root, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil && fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symlink and the archive tries to write through it",
				ErrUnsafeArchive, cur)
		case err == nil:
			return fmt.Errorf("%w: %s exists and is not a directory", ErrUnsafeArchive, cur)
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("prebuilt: creating %s: %w", cur, err)
			}
		default:
			return err
		}
	}
	return nil
}

// writeFile creates one regular file. O_EXCL is what makes a duplicate entry an
// error rather than an overwrite, and O_NOFOLLOW is the second line of defense
// against a symlink an earlier entry planted.
func writeFile(target string, r io.Reader, mode fs.FileMode, limit int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscallNoFollow, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return 0, fmt.Errorf("%w: the archive contains %s twice", ErrUnsafeArchive, filepath.Base(target))
		}
		return 0, fmt.Errorf("prebuilt: creating %s: %w", target, err)
	}
	defer f.Close()

	// LimitReader is belt to the header's braces: a tar whose header understates
	// the real payload length cannot write more than the limit anyway.
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		return n, fmt.Errorf("prebuilt: writing %s: %w", target, err)
	}
	if n > limit {
		return n, fmt.Errorf("%w: %s exceeds the %d byte limit", ErrUnsafeArchive, target, limit)
	}
	if err := f.Close(); err != nil {
		return n, fmt.Errorf("prebuilt: closing %s: %w", target, err)
	}
	return n, nil
}

// fileMode reduces an archive's mode to one of two values, dropping setuid,
// setgid, sticky and any group- or world-writable bit. A tarball does not get
// to decide the permissions of files in the state directory.
func fileMode(mode int64) fs.FileMode {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// makeBinExecutable is section 6.4's `chmod +x bin/*`. Upstream's tarballs do
// carry the bit, but an archive produced by a tool that dropped it would
// otherwise install a version whose `llama-server` cannot be executed — a
// failure that surfaces as an exec error at instance start rather than at
// install time.
func makeBinExecutable(root string) ([]string, error) {
	binDir := filepath.Join(root, "bin")
	entries, err := os.ReadDir(binDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prebuilt: reading %s: %w", binDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if fi.Mode()&0o111 == 0 {
			if err := os.Chmod(filepath.Join(binDir, e.Name()), 0o755); err != nil {
				return nil, fmt.Errorf("prebuilt: chmod +x %s: %w", e.Name(), err)
			}
		}
		out = append(out, path.Join("bin", e.Name()))
	}
	sort.Strings(out)
	return out, nil
}
