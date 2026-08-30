package selfupdate

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
)

// Extracting exactly one member from a release tarball.
//
// A release tarball is `binary + LICENSE + README` (DESIGN section 16.2 step 2)
// and this protocol wants precisely one of the three. Extracting one named
// member rather than a tree is not a shortcut, it is the safety property: there
// is no directory to walk, no path to join, no symlink to follow and therefore
// no traversal to defend against — the destination is a path the CALLER chose
// and the archive contributes nothing to it.
//
// The privileged actor runs this against `<prefix>`, so "the archive cannot name
// its own destination" is the difference between a swap and an arbitrary
// root-owned write.

// maxBinaryBytes bounds a decompressed member. The binary is ~30 MB with the UI
// embedded; 512 MB is far above anything this project produces and far below
// what a decompression bomb would need to matter.
const maxBinaryBytes = 512 << 20

// ErrMemberMissing is a tarball with no `llamaman` in it — a wrong asset, a
// truncated download that still verified (it cannot, but the error exists so the
// failure is named rather than mysterious), or an archive from somewhere else.
var ErrMemberMissing = errors.New("selfupdate: the release tarball contains no llamaman binary")

// ErrExpectedDigestRequired is a caller that asked to extract without saying
// which bytes it had verified. It is a programming error rather than a runtime
// condition, and it is an error rather than a default because the default that
// would suggest itself — "no digest means do not check" — is precisely the hole
// the parameter exists to close.
var ErrExpectedDigestRequired = errors.New(
	"selfupdate: extracting a release binary requires the tarball's verified sha256")

// ExtractBinary writes the tarball's `llamaman` member to dest with mode, fsyncs
// it, and returns the member's sha256.
//
// # The digest is checked against the bytes this call actually read
//
// expectSHA256 is the digest the SIGNED `checksums.txt` names for this tarball,
// as returned by VerifyStaged. The whole file is hashed from the very same read
// that feeds the tar reader — one os.Open, one pass, io.TeeReader — and the
// extracted member is not renamed into place unless that hash matches. Nothing
// is renamed onto a live path before the check, so a mismatch leaves `<prefix>`
// exactly as it was.
//
// That binding is the point, and it is a genuine privilege boundary rather than
// a belt-and-braces check. `<state_dir>/update` is owned by the unprivileged
// service identity while `<prefix>` is root's, so a caller that verified a path
// and then re-opened that path would be trusting the identity D89 says it must
// not: between the two opens that uid can rename a different tarball over the
// verified name, and the root actor would extract, chown 0755 root:root and
// rename onto `<prefix>/llamaman` a binary nothing ever signed. Hashing the
// bytes that are being extracted, rather than the bytes that were at that path a
// moment ago, is what makes D89's "a file the service identity could rewrite
// after verification is never the file that lands on `<prefix>`" true of the
// code and not only of the prose.
//
// dest is written through a temp file in the SAME directory and renamed, so a
// caller that points it at a live path never exposes a partial file — which is
// what section 12.2 step 2 needs when dest is `<prefix>/llamaman.new.tmp` and
// something may already be there from an actor that was killed (row 7: the file
// is reclaimed by the next run, "so it has a writer that reclaims it").
func ExtractBinary(tarball, dest string, mode fs.FileMode, expectSHA256 string) (string, error) {
	if expectSHA256 == "" {
		return "", ErrExpectedDigestRequired
	}

	f, err := os.Open(tarball)
	if err != nil {
		return "", fmt.Errorf("selfupdate: open %s: %w", tarball, err)
	}
	defer f.Close()

	// whole hashes every byte the gzip reader pulls, and the drain below covers
	// whatever it did not need. There is exactly one file handle in this
	// function and it is never re-opened.
	whole := newHasher()
	raw := io.TeeReader(f, whole)

	gz, err := gzip.NewReader(raw)
	if err != nil {
		return "", fmt.Errorf("selfupdate: %s is not gzip: %w", tarball, err)
	}
	defer gz.Close()

	var (
		tr      = tar.NewReader(gz)
		tmpName string
		member  string
	)
	for tmpName == "" {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: %s", ErrMemberMissing, tarball)
		}
		if err != nil {
			return "", fmt.Errorf("selfupdate: read %s: %w", tarball, err)
		}
		if h.Typeflag != tar.TypeReg || path.Base(path.Clean(h.Name)) != BinaryName {
			continue
		}
		if h.Size > maxBinaryBytes {
			return "", fmt.Errorf("selfupdate: %s in %s is %d bytes, over the %d-byte limit",
				BinaryName, tarball, h.Size, int64(maxBinaryBytes))
		}
		member, tmpName, err = writeMember(io.LimitReader(tr, maxBinaryBytes+1), dest, mode)
		if err != nil {
			return "", err
		}
	}
	// From here on the temp file exists, so every exit removes it unless the
	// rename below consumed it.
	defer func() { _ = os.Remove(tmpName) }()

	// The member ends before the archive does — trailing tar padding, and
	// whatever gzip had not needed to read yet — so drain the rest of the FILE
	// through the hasher. Only then does `whole` cover the whole tarball, which
	// is what checksums.txt named.
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return "", fmt.Errorf("selfupdate: read %s: %w", tarball, err)
	}
	if got := whole.hex(); got != expectSHA256 {
		return "", fmt.Errorf(
			"%w: %s hashed to %s while it was being extracted, but the signed checksums.txt says %s "+
				"— the file changed underneath this process and nothing was installed",
			ErrDigest, tarball, got, expectSHA256)
	}

	dir := filepath.Dir(dest)
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("selfupdate: rename %s to %s: %w", tmpName, dest, err)
	}
	if err := fsyncDir(dir); err != nil {
		return "", err
	}
	return member, nil
}

// writeMember is the temp-file half, kept separate so the reader loop above
// stays a loop. It deliberately stops SHORT of the rename: the caller does that,
// and only after the whole-file digest has been checked against the signature.
func writeMember(r io.Reader, dest string, mode fs.FileMode) (digest, tmpName string, err error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".*.part")
	if err != nil {
		return "", "", fmt.Errorf("selfupdate: create a temp file in %s: %w", dir, err)
	}
	tmpName = tmp.Name()
	// Every failure below removes the temp file; success hands it to the caller,
	// whose own defer takes over.
	fail := func(err error) (string, string, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}

	h := newHasher()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return fail(fmt.Errorf("selfupdate: write %s: %w", tmpName, err))
	}
	if n > maxBinaryBytes {
		return fail(fmt.Errorf("selfupdate: the extracted binary exceeds the %d-byte limit",
			int64(maxBinaryBytes)))
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(fmt.Errorf("selfupdate: chmod %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("selfupdate: fsync %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("selfupdate: close %s: %w", tmpName, err)
	}
	return h.hex(), tmpName, nil
}
