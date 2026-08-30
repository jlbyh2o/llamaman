package prebuilt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Publishing a staged tree (DESIGN section 6.4 step 4 and D78).
//
// Every install, prebuilt and source, writes to `versions/<id>.staging` and is
// renamed into place at publish. For a fresh id that is ONE atomic rename into
// a non-existent target. For an id whose directory already exists — a forced
// rebuild — publish re-checks the D25 live-process guard and then swaps:
//
//	rename(versions/<id>      → versions/<id>.old)
//	rename(versions/<id>.staging → versions/<id>)
//	remove versions/<id>.old
//
// `versions/active` names `<id>` and is NEVER touched, so it is correct before
// and after; the only window in which it dangles is between the two renames.
// That window is closed from the other side by data rather than by timing: the
// row is `pending`…`building`…`verifying` for the whole rebuild, and both the
// supervisor and `instance-exec` refuse to start an instance while the
// `is_active=1` row is not `ready` (section 6.2).
//
// The guard is re-asked HERE, immediately before the swap, and not only when
// the request arrived: between the click and this moment an instance may have
// started, and renaming a directory out from under a running `llama-server` is
// the one case D25 makes a hard refusal.

// StagingSuffix is appended to a version id to name its staging directory.
const StagingSuffix = ".staging"

// oldSuffix names the displaced directory during a swap.
const oldSuffix = ".old"

// ErrVersionInUse is D25's refusal: a live process is executing out of the
// directory this publish would replace. It maps to `409 version_in_use`.
var ErrVersionInUse = errors.New("prebuilt: a running process is executing from this version directory")

// StagingDir is the staging path for a version id under a versions root.
func StagingDir(versionsRoot, id string) string {
	return filepath.Join(versionsRoot, id+StagingSuffix)
}

// VersionDir is the final path for a version id under a versions root.
func VersionDir(versionsRoot, id string) string {
	return filepath.Join(versionsRoot, id)
}

// DirGuard answers D25's question: is a live process executing out of this
// directory? The implementation — `readlink /proc/<pid>/exe` over every visible
// process — belongs to the service that owns process inspection; this package
// only asks.
type DirGuard interface {
	InUse(ctx context.Context, dir string) (pid int, inUse bool, err error)
}

// PublishOptions configures one publish.
type PublishOptions struct {
	// Staging is the directory to publish. Required.
	Staging string
	// Target is where it becomes. Required.
	Target string
	// Guard, when set, is asked immediately before a SWAP — never before a
	// fresh install, which displaces nothing. Nil skips the check, which is
	// correct only for a target that does not exist.
	Guard DirGuard
}

// Publish moves a staged tree into its final place.
//
// Both paths must be on the same filesystem, which they are by construction:
// `versions/<id>.staging` and `versions/<id>` are siblings inside the state
// directory. That is what makes the operation a rename — atomic, and impossible
// to observe half-done — rather than a copy.
func Publish(ctx context.Context, opts PublishOptions) error {
	if opts.Staging == "" || opts.Target == "" {
		return errors.New("prebuilt: Publish needs both a staging and a target directory")
	}
	if fi, err := os.Stat(opts.Staging); err != nil || !fi.IsDir() {
		return fmt.Errorf("prebuilt: staging directory %s is not there to publish", opts.Staging)
	}

	_, err := os.Lstat(opts.Target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The fresh case: one atomic rename into a non-existent target.
		if err := os.Rename(opts.Staging, opts.Target); err != nil {
			return fmt.Errorf("prebuilt: publishing %s: %w", opts.Target, err)
		}
		return nil
	case err != nil:
		return err
	}

	// The rebuild case (D78). Re-ask the guard now, not when the job started.
	if opts.Guard != nil {
		pid, inUse, err := opts.Guard.InUse(ctx, opts.Target)
		if err != nil {
			return fmt.Errorf("prebuilt: checking whether %s is in use: %w", opts.Target, err)
		}
		if inUse {
			return fmt.Errorf("%w: pid %d is running from %s", ErrVersionInUse, pid, opts.Target)
		}
	}

	old := opts.Target + oldSuffix
	// A leftover `.old` from a swap that died between its two renames would
	// make the first rename fail. Clearing it is safe: it is by definition a
	// directory nothing points at.
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("prebuilt: clearing %s: %w", old, err)
	}
	if err := os.Rename(opts.Target, old); err != nil {
		return fmt.Errorf("prebuilt: displacing %s: %w", opts.Target, err)
	}
	if err := os.Rename(opts.Staging, opts.Target); err != nil {
		// Put the old tree back rather than leaving the id with no directory at
		// all: a dangling `versions/active` is the one state this protocol
		// exists to keep short.
		if rbErr := os.Rename(old, opts.Target); rbErr != nil {
			return fmt.Errorf("prebuilt: publishing %s failed (%w) AND restoring the previous tree failed (%v); "+
				"the previous build is at %s", opts.Target, err, rbErr, old)
		}
		return fmt.Errorf("prebuilt: publishing %s: %w", opts.Target, err)
	}
	if err := os.RemoveAll(old); err != nil {
		// The swap succeeded; a leftover `.old` is garbage, not a failure.
		return nil
	}
	return nil
}

// CleanStaging removes a staging directory and any partial download beside it.
// It is what a cancellation and a failed install both call (section 6.5's
// cancellation rule), and it is deliberately forgiving: a missing directory is
// success.
func CleanStaging(staging string) error {
	if staging == "" {
		return nil
	}
	if !strings.HasSuffix(staging, StagingSuffix) {
		// A guard against a caller passing the PUBLISHED directory by mistake,
		// which would delete a working version.
		return fmt.Errorf("prebuilt: refusing to remove %s: it is not a %s directory", staging, StagingSuffix)
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("prebuilt: removing %s: %w", staging, err)
	}
	return nil
}
