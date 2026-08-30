package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// `update/pending` — one marker, one parser (DESIGN section 12.1).
//
// # Who reads it, and who deliberately does not
//
// The JUDGE reads nothing out of this file. Its verdict turns on the file's
// EXISTENCE and on the unit state systemd reports, so no format it does not
// understand can disarm it (D13). The one parser is the confirmation gate of
// section 12.3, and it reads in both directions across versions: a newer binary
// confirming an update, and — after a downgrade — an OLDER binary reading a
// marker a newer one wrote.
//
// # The freeze rules, which apply to this file and to nothing else
//
// Fields may be ADDED, never removed and never retyped; a reader ignores fields
// it does not know (encoding/json does that by default, and the test in
// testdata/update/ replays every historical shape to prove it). `format` is the
// one gate: an unknown value is not a "newer marker to wait for", it is a file
// this binary cannot read, and section 12.3 SWEEPS it rather than deferring to
// it forever — which is safe precisely because the sweep's precondition is a
// fact about processes, not about file contents (D91).
//
// `staged_at` is informational. It is what `GET /update/status` renders and what
// the journal line quotes, and NO decision anywhere in this protocol is measured
// from it: liveness is asked of systemd, never of a clock (D91).

// MarkerFormat is the current `format` value. It has been 1 since the v1.0.0
// floor and a bump would be a cross-version contract change, which section 18
// item 7 keeps to exactly this one file.
const MarkerFormat = 1

// Marker is `update/pending`.
type Marker struct {
	// Format is the freeze gate. A reader that does not recognize it treats the
	// file as unreadable rather than guessing.
	Format int `json:"format"`
	// SelfUpdateID names the `self_updates` row this update is. A marker whose id
	// names no row is a no-op in both writing branches of the gate, not an error:
	// that state is ordinary after F12's fresh-DB arm or a `restore-db` to a
	// snapshot older than the update (section 12.3).
	SelfUpdateID string `json:"self_update_id"`
	// FromVersion is the version being replaced; TargetVersion is the version
	// being installed. The gate compares TargetVersion against this binary's own
	// version, and that comparison IS the confirmation.
	FromVersion   string `json:"from_version"`
	TargetVersion string `json:"target_version"`
	// BinaryPath is the daemon's resolved `<prefix>/llamaman`. It exists so the
	// privileged actor can cross-check the daemon's view of `<prefix>` against its
	// own os.Executable() and refuse on a disagreement rather than guess.
	BinaryPath string `json:"binary_path"`
	// StagedAt is Unix milliseconds, informational only. See the note above.
	StagedAt int64 `json:"staged_at"`
}

// The two errors a reader distinguishes, and the only two it needs.
var (
	// ErrNoMarker is "there is no update in flight". The gate goes straight to
	// its closing pass; the actor refuses.
	ErrNoMarker = errors.New("selfupdate: no update/pending marker")
	// ErrMarkerUnreadable is "there is a marker and this binary cannot read it" —
	// truncated, not JSON, or at an unknown `format`. Section 12.3 takes branch 3
	// for it, naming the file rather than a version, because sweeping a file no
	// process is waiting for is safe and leaving it would reproduce the one
	// property section 12 exists to prevent: a file under `update/` outliving
	// every process that knows what it means.
	ErrMarkerUnreadable = errors.New("selfupdate: update/pending is unreadable")
)

// maxMarkerBytes bounds the read. The marker is six small fields; anything
// larger is a file this binary should refuse to parse rather than one it should
// try harder on.
const maxMarkerBytes = 64 << 10

// ReadMarker parses `update/pending`.
//
// It returns ErrNoMarker when the file is absent and ErrMarkerUnreadable when it
// exists and cannot be understood — and the second is deliberately NOT folded
// into the first, because the two lead to different branches: no marker means
// "no update in flight", while an unreadable one means "an update was in flight
// and this is its wreckage", which still has a row and a job to close.
func ReadMarker(path string) (Marker, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Marker{}, ErrNoMarker
	case err != nil:
		// An I/O error or a permission problem is not "absent": something is
		// there. Treat it the way an unparseable file is treated.
		return Marker{}, fmt.Errorf("%w: %s: %v", ErrMarkerUnreadable, path, err)
	}
	if len(b) > maxMarkerBytes {
		return Marker{}, fmt.Errorf("%w: %s is %d bytes", ErrMarkerUnreadable, path, len(b))
	}
	return ParseMarker(b)
}

// ParseMarker is ReadMarker's pure half, which is what the historical-shape
// fixtures in testdata/update/ are replayed against.
//
// Unknown fields are IGNORED rather than rejected — that is the add-only half of
// the freeze rule, and rejecting them would make every field a future release
// adds fatal to the release before it.
func ParseMarker(b []byte) (Marker, error) {
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, fmt.Errorf("%w: %v", ErrMarkerUnreadable, err)
	}
	if m.Format != MarkerFormat {
		return Marker{}, fmt.Errorf("%w: unknown format %d (this binary reads %d)",
			ErrMarkerUnreadable, m.Format, MarkerFormat)
	}
	if m.TargetVersion == "" {
		// A marker with no target version cannot answer the gate's one question.
		// It is wreckage, and it takes the same branch as any other unreadable
		// file rather than being silently treated as a confirmation.
		return Marker{}, fmt.Errorf("%w: target_version is empty", ErrMarkerUnreadable)
	}
	return m, nil
}

// WriteMarker performs section 12.1 step 6: a temp file in `update/`, fsync,
// rename to `pending`, fsync the directory.
//
// The marker is therefore complete or absent on disk, never half-written — which
// is what lets the judge's arming logic be `ConditionPathExists=` and nothing
// else, and what makes stop-point row 3 ("marker written, `swapping` not
// committed") a state with exactly one exit rather than a spectrum.
func WriteMarker(path string, m Marker) error {
	if m.Format == 0 {
		m.Format = MarkerFormat
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("selfupdate: marshal the pending marker: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, PendingFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("selfupdate: create a temp marker in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op once the rename succeeded

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfupdate: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("selfupdate: rename %s to %s: %w", tmpName, path, err)
	}
	return fsyncDir(dir)
}

// RemoveMarker unlinks `update/pending` and reports success when it was already
// gone. Every caller is idempotent by design — the gate's branches are re-run by
// the next boot and by the 30 s ticker — so "already unlinked" is the ordinary
// second call, not a failure.
func RemoveMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("selfupdate: remove %s: %w", path, err)
	}
	return fsyncDir(filepath.Dir(path))
}

// fsyncDir makes a rename or an unlink durable. Without it a power loss can
// leave the directory entry unwritten while the file's own bytes are safely on
// disk, which for THIS file is the difference between an armed judge and a
// disarmed one.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("selfupdate: open %s to fsync it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some filesystems refuse fsync on a directory. That is not a reason to
		// fail an update: the rename itself is still atomic, and the durability
		// this adds is a power-loss nicety rather than the correctness argument.
		return nil
	}
	return nil
}
