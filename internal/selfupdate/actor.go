package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/jlbyh2o/llamaman/internal/model"
	"golang.org/x/sys/unix"
)

// The privileged half: the swap (DESIGN section 12.2).
//
// This is `llamaman selfupdate-apply --scope system` running as root out of
// `llamaman-selfupdate.service`, and it is the SAME sequence the daemon performs
// in process in the D2 user-scope topology, where there is no privilege boundary
// to cross (section 5.2a item 2). One implementation, two callers.
//
// Every step is either a check with no side effect or one atomic rename. Three
// properties are load-bearing and all three are structural rather than
// argued:
//
//   - **Nothing here stops a unit.** Not at step 0, not at any later step. The
//     daemon has already stopped serving itself (section 12.1 step 7) and is
//     waiting to be restarted, so there is nothing to stop and no
//     verify-before-stop ordering to get wrong. Section 19's first preservation
//     property is this sentence.
//   - **Every change to the installed binary is one rename() between two names
//     in one directory.** `<prefix>` holds both the installed binary and the
//     retained one, so the rename can never return EXDEV and there is no
//     intermediate on-disk state for a crash to land in (D89).
//   - **This process never opens llamaman.db**, not even read-only: a
//     root-created `-wal`/`-shm` beside a 0600 database is a database the
//     service identity can never write again (section 11.3). It therefore raises
//     no notification and writes no row; it states facts in the journal and the
//     daemon's gate is what tells the human (section 12.2).
//
// A failure at any step logs one structured journald line and exits non-zero,
// undoing nothing — because there is nothing to undo. Each step either completed
// its rename or did not, and section 12.3's branch 3 closes the update out
// either way.

// Restarter is the one systemd verb this actor issues, declared here because the
// consumer owns the interface (DESIGN section 1) and because naming it this
// narrowly is what makes "no step in the protocol stops a unit" checkable: there
// is no Stop on this interface to call.
type Restarter interface {
	// RestartNoBlock is `systemctl restart --no-block llamaman.service`. It must
	// not wait: this call ends by restarting the very process that would be
	// waiting on the job.
	RestartNoBlock(ctx context.Context, unit string) error
}

// ApplyOptions is everything the swap needs.
type ApplyOptions struct {
	// Scope is the `--scope` argument install-units rendered into the unit. It is
	// parsed and validated by the caller; a missing or unparsable value is a
	// REFUSAL, not a guess (section 12.2), and this actor additionally refuses to
	// run at all in user scope, where the daemon performs the same sequence in
	// process.
	Scope model.SystemdScope

	// StateDir is `%S/llamaman`, from $STATE_DIRECTORY. Prefix is derived from
	// this process's own os.Executable() and cross-checked against the marker's
	// `binary_path`.
	Layout Layout

	// Keys are the compiled-in release keys. The actor re-verifies the tarball
	// itself rather than trusting the unprivileged staging step, because this is
	// a genuine privilege boundary.
	//
	// **Nil means the compiled-in set**, loaded through EmbeddedKeys() — the same
	// default New() applies to the daemon's half (service.go). The default lives
	// here rather than at the call site because there is no correct alternative
	// for a caller to supply: section 12.2 step 0's re-verification is against
	// "the compiled-in ed25519 key" and nothing else, and a production caller
	// that simply forgot the field would otherwise hand VerifyChecksums an empty
	// KeySet, which fails ErrSignature for every genuine release — refusing every
	// update at preflight with a message that reads like a corrupt download.
	Keys KeySet

	// Restart issues the final `systemctl restart --no-block llamaman.service`.
	// Nil performs the swap and skips the restart, which is what a test wants and
	// what nothing in production passes.
	Restart Restarter

	// Log receives one structured line per outcome. Nil uses slog.Default.
	Log *slog.Logger

	// GOARCH names the tarball. It exists as a field only so a test can drive the
	// path on a machine whose architecture is not the fixture's.
	GOARCH string
}

// ApplyResult reports what the swap did, for the journal line and for a test.
type ApplyResult struct {
	// FromVersion and TargetVersion are read out of the marker.
	FromVersion, TargetVersion string
	// RetainedSHA256 is the digest of the binary now at `<prefix>/llamaman.prev`,
	// verified against the bytes that were read to produce it.
	RetainedSHA256 string
	// InstalledSHA256 is the digest of the binary now at `<prefix>/llamaman`.
	InstalledSHA256 string
	// Restarted reports whether `systemctl restart --no-block` was issued.
	Restarted bool
}

// ErrUserScope is section 12.2's refusal to run the ROOT ONESHOT in user scope:
// there is no oneshot in that topology, because the daemon performs the swap in
// process, and a root actor summoned there would be a privilege boundary nobody
// asked for.
//
// It belongs to the `selfupdate-apply` ENTRY POINT rather than to Apply itself,
// and the distinction is section 5.2a item 2's: "the forward swap is performed
// by the daemon itself, in process, and it is section 12.2's sequence with the
// privilege boundary removed". Apply IS that sequence — one implementation, two
// callers — so it must run in user scope when the daemon calls it, while
// `llamaman selfupdate-apply --scope user` must refuse. internal/cli enforces
// the refusal; the sequence below does the work in whichever scope it is given.
var ErrUserScope = errors.New("selfupdate: selfupdate-apply refuses to run in user scope; the daemon performs the swap in process there (DESIGN section 5.2a)")

// ErrPrefixMismatch is the marker's `binary_path` disagreeing with this
// process's own resolved executable — stop-point row 5's third refusal. It
// exists so the actor refuses rather than guesses which `<prefix>` was meant.
var ErrPrefixMismatch = errors.New("selfupdate: the marker's binary_path is not this process's own binary")

// Apply performs section 12.2's steps 0 through 4.
func Apply(ctx context.Context, opts ApplyOptions) (ApplyResult, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	arch := opts.GOARCH
	if arch == "" {
		arch = runtime.GOARCH
	}
	keys := opts.Keys
	if keys == nil {
		// The compiled-in set, exactly as section 12.2 step 0 says: "re-verify
		// `update/checksums.txt` against the compiled-in ed25519 key". Failing to
		// load it is a REFUSAL — nothing is touched — rather than an empty set
		// that would refuse every release one step later with a message about
		// signatures instead of about this binary.
		loaded, err := EmbeddedKeys()
		if err != nil {
			return ApplyResult{}, err
		}
		keys = loaded
	}

	// --- step 0: preflight. Every refusal is decided here, and nothing is
	// touched. Note what is NOT in this list and never was: nothing here stops
	// llamaman.service, at this step or any later one.
	//
	// The scope is validated but NOT restricted to system here: this function is
	// the swap sequence, and section 5.2a item 2 has the user-scope daemon run
	// exactly it, in process. `selfupdate-apply --scope user` is what refuses,
	// in internal/cli, because the refusal is about who was summoned rather than
	// about what the sequence does.
	if !opts.Scope.Valid() {
		return ApplyResult{}, fmt.Errorf("selfupdate: --scope must be %q or %q, got %q",
			model.ScopeSystem, model.ScopeUser, opts.Scope)
	}

	l := opts.Layout
	marker, err := ReadMarker(l.PendingPath())
	if err != nil {
		return ApplyResult{}, err
	}

	// The prefix comes from this process's own binary, never from the marker.
	// The marker's copy is a CROSS-CHECK: a disagreement means the daemon and
	// this actor are looking at two different installations, and the right answer
	// then is to refuse rather than to pick one.
	//
	// A caller that has ALREADY resolved it may pass it in, and exactly two do:
	// the D2 user-scope daemon, which performs this sequence in process and is
	// itself the binary at `<prefix>/llamaman`, and a test, which needs a
	// `<prefix>` it owns rather than the directory its own test binary was built
	// into. The root oneshot passes nothing and derives it here, which is the
	// path D15 is about.
	if l.Prefix == "" {
		prefix, err := ResolvePrefix()
		if err != nil {
			return ApplyResult{}, err
		}
		l.Prefix = prefix
	}
	prefix := l.Prefix
	if marker.BinaryPath != "" && marker.BinaryPath != l.InstalledPath() {
		return ApplyResult{}, fmt.Errorf("%w: marker says %s, this process is %s",
			ErrPrefixMismatch, marker.BinaryPath, l.InstalledPath())
	}

	// The signed digest is carried forward to step 2 rather than being recomputed
	// there from the same path: `update/` belongs to the unprivileged service
	// identity, and re-opening a verified path is how a root actor comes to
	// install bytes nobody signed (D89).
	tarball := TarballName(marker.TargetVersion, arch)
	signedDigest, err := VerifyStaged(l.UpdateDir(), tarball, keys)
	if err != nil {
		return ApplyResult{}, err
	}

	installed := l.InstalledPath()
	size, err := fileSize(installed)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := checkRoom(prefix, 2*size); err != nil {
		return ApplyResult{}, err
	}

	res := ApplyResult{FromVersion: marker.FromVersion, TargetVersion: marker.TargetVersion}

	// --- step 1: retain the current binary. Copy, fsync, verify the copy's
	// sha256 against the digest of the bytes just read, then rename it over
	// `<prefix>/llamaman.prev`. Same directory, so the rename is atomic and
	// cannot return EXDEV; any previously retained binary is replaced in one
	// step, which is why llamaman.prev never accumulates and never goes stale.
	res.RetainedSHA256, err = retain(installed, l.retainTmpPath(), l.RetainedPath())
	if err != nil {
		return res, err
	}

	// --- step 2: extract the new binary into `<prefix>`, from the tarball step 0
	// re-verified. The staged `update/llamaman.new` is never used and no longer
	// exists — the daemon unlinked it after its version probe (D89 (c)).
	//
	// The digest step 0 read out of the SIGNED checksums file is passed in, and
	// ExtractBinary hashes the bytes it is extracting against it in the same
	// pass. A tarball swapped between step 0 and here therefore fails, having
	// written only `<prefix>/llamaman.new.tmp` — stop-point row 7, which the
	// protocol already exits from — rather than installing unsigned bytes.
	mode, owner, err := statFile(installed)
	if err != nil {
		return res, err
	}
	res.InstalledSHA256, err = ExtractBinary(filepath.Join(l.UpdateDir(), tarball),
		l.installTmpPath(), mode, signedDigest)
	if err != nil {
		return res, err
	}
	if err := chownLike(l.installTmpPath(), owner); err != nil {
		return res, err
	}

	// --- step 3: the swap. One atomic rename between two names in one
	// root-owned directory. Before this instant the installed binary is wholly
	// the old one; after it, wholly the new one; a power loss during it leaves one
	// or the other and never a fragment, because both files were fsynced first.
	if err := os.Rename(l.installTmpPath(), installed); err != nil {
		return res, fmt.Errorf("selfupdate: install %s: %w", installed, err)
	}
	if err := fsyncDir(prefix); err != nil {
		return res, err
	}
	log.Info("self-update: swapped the installed binary",
		"from_version", marker.FromVersion, "target_version", marker.TargetVersion,
		"binary", installed, "retained", l.RetainedPath())

	// --- step 4: `systemctl restart --no-block llamaman.service`. That is SPEC
	// section 3.8's literal "restarts llamaman.service". Instance units are
	// untouched and keep serving throughout, and the public gateway ports stay
	// open across the restart by section 9.4.
	if opts.Restart != nil {
		if err := opts.Restart.RestartNoBlock(ctx, DaemonUnit); err != nil {
			return res, fmt.Errorf("selfupdate: restart %s: %w", DaemonUnit, err)
		}
		res.Restarted = true
	}
	return res, nil
}

// DaemonUnit, SwapUnit and JudgeUnit are the three unit names this protocol
// speaks. They are restated here rather than imported from internal/systemd
// because the two privileged actors must not pull the D-Bus vocabulary in: they
// run when the daemon is gone and must carry no client library (section 12.2).
const (
	DaemonUnit = "llamaman.service"
	SwapUnit   = "llamaman-selfupdate.service"
	JudgeUnit  = "llamaman-update-verify.service"
)

// retain is step 1: copy → fsync → digest-check → rename.
func retain(src, tmp, dest string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("selfupdate: open %s to retain it: %w", src, err)
	}
	defer in.Close()

	mode, owner, err := statHandle(in, src)
	if err != nil {
		return "", err
	}

	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return "", fmt.Errorf("selfupdate: create %s: %w", tmp, err)
	}
	defer func() { _ = os.Remove(tmp) }()

	read := newHasher()
	if _, err := io.Copy(io.MultiWriter(out, read), in); err != nil {
		out.Close()
		return "", fmt.Errorf("selfupdate: copy %s to %s: %w", src, tmp, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", fmt.Errorf("selfupdate: fsync %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("selfupdate: close %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return "", fmt.Errorf("selfupdate: chmod %s: %w", tmp, err)
	}
	if err := chownLike(tmp, owner); err != nil {
		return "", err
	}

	// The copy is verified against the digest of the bytes that produced it,
	// on disk, after the fsync — so "the retained binary is byte-identical to the
	// one that was replaced" is a checked fact rather than an assumption about
	// io.Copy.
	written, err := FileSHA256(tmp)
	if err != nil {
		return "", err
	}
	if written != read.hex() {
		return "", fmt.Errorf("selfupdate: the retained copy of %s hashes to %s, the bytes read hash to %s",
			src, written, read.hex())
	}

	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("selfupdate: retain %s as %s: %w", src, dest, err)
	}
	if err := fsyncDir(filepath.Dir(dest)); err != nil {
		return "", err
	}
	return written, nil
}

// fileOwner is a uid/gid pair, or "do not change ownership" when unknown.
type fileOwner struct {
	uid, gid int
	known    bool
}

func statFile(path string) (os.FileMode, fileOwner, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fileOwner{}, fmt.Errorf("selfupdate: stat %s: %w", path, err)
	}
	return fi.Mode().Perm(), ownerOf(fi), nil
}

func statHandle(f *os.File, name string) (os.FileMode, fileOwner, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, fileOwner{}, fmt.Errorf("selfupdate: stat %s: %w", name, err)
	}
	return fi.Mode().Perm(), ownerOf(fi), nil
}

func ownerOf(fi os.FileInfo) fileOwner {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileOwner{}
	}
	return fileOwner{uid: int(st.Uid), gid: int(st.Gid), known: true}
}

// chownLike gives a staged file the same owner as the binary it will replace.
//
// A failure is deliberately NOT fatal when the ids already match, which is the
// ordinary case in user scope where every file involved is one uid and chown is
// refused for an unprivileged process. In system scope this actor is root and
// the call succeeds.
func chownLike(path string, owner fileOwner) error {
	if !owner.known {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("selfupdate: stat %s: %w", path, err)
	}
	if cur := ownerOf(fi); cur.known && cur.uid == owner.uid && cur.gid == owner.gid {
		return nil
	}
	if err := os.Chown(path, owner.uid, owner.gid); err != nil {
		return fmt.Errorf("selfupdate: chown %s to %d:%d: %w", path, owner.uid, owner.gid, err)
	}
	return nil
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("selfupdate: stat %s: %w", path, err)
	}
	return fi.Size(), nil
}

// ErrNoRoom is step 0's last check: `<prefix>` must have room for two more
// copies of the binary — the retained one and the staged new one — because
// discovering ENOSPC halfway through a swap is the one failure this sequence
// cannot make harmless.
var ErrNoRoom = errors.New("selfupdate: not enough free space in the installation directory")

func checkRoom(dir string, need int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		// A filesystem that will not answer is not a reason to refuse an update;
		// the write below will fail with a real error if there is genuinely no
		// room, and that failure is stop-point row 6 or 7, both of which the
		// protocol exits from.
		return nil
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < need {
		return fmt.Errorf("%w: %s has %d bytes free, the swap needs %d", ErrNoRoom, dir, free, need)
	}
	return nil
}
