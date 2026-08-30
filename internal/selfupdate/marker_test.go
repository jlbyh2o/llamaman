package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The marker-format freeze (DESIGN section 12.1, section 15, section 18 item 7).
//
// `update/pending` is the ONE cross-version contract left in this design, and
// the gate reads it in BOTH directions: a newer binary confirming an update, and
// — after a downgrade — an older binary reading a marker a newer one wrote. So
// the rules are frozen: fields may be added, never removed and never retyped;
// a reader ignores fields it does not know; and an unknown `format` is SWEPT
// rather than deferred to forever.
//
// `testdata/update/` carries every historical shape and this suite replays all
// of them. A release that broke one of these rules would have to change a
// checked-in fixture to make its tests pass, which is the point.

// TestMarkerHistoricalShapes is section 15's "a `pending` format fixture per
// historical shape is replayed against the current gate, plus one at
// `"format":99` asserting the SWEEP branch".
func TestMarkerHistoricalShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		file string
		// wantErr is nil for a shape this binary must read, and
		// ErrMarkerUnreadable for one it must SWEEP rather than defer to.
		wantErr error
		// want is checked only for the readable shapes.
		want Marker
	}{
		{
			file: "v1.0.0-floor.json",
			want: Marker{
				Format:        1,
				SelfUpdateID:  "01J8ZQ7X0000000000000FLOOR",
				FromVersion:   "v1.0.0",
				TargetVersion: "v1.1.0",
				BinaryPath:    "/usr/local/bin/llamaman",
				StagedAt:      1788012345678,
			},
		},
		{
			// The add-only half of the rule, and the direction that actually
			// matters: an OLDER binary, after a downgrade, reading what a newer
			// one wrote. Rejecting the unknown field would make every field a
			// future release adds fatal to the release before it.
			file: "v1.2.0-added-field.json",
			want: Marker{
				Format:        1,
				SelfUpdateID:  "01J8ZQ7X0000000000000ADDED",
				FromVersion:   "v1.1.0",
				TargetVersion: "v1.2.0",
				BinaryPath:    "/usr/local/bin/llamaman",
				StagedAt:      1788012345678,
			},
		},
		{
			file: "user-scope-prefix.json",
			want: Marker{
				Format:        1,
				SelfUpdateID:  "01J8ZQ7X00000000000000USER",
				FromVersion:   "v1.1.0",
				TargetVersion: "v1.2.0",
				BinaryPath:    "/home/llamaman/.local/bin/llamaman",
				StagedAt:      1788012345678,
			},
		},
		{
			// The property that stops a file no reader understands from
			// outliving every process that does (D91).
			file:    "unknown-format.json",
			wantErr: ErrMarkerUnreadable,
		},
		{
			file:    "truncated.json",
			wantErr: ErrMarkerUnreadable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "update", tc.file))
			if err != nil {
				t.Fatalf("read the fixture: %v", err)
			}

			got, err := ParseMarker(body)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMarker: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestMarkerFieldsAreFrozen asserts the wire names, because they are the
// contract: a rename is indistinguishable on the wire from a removal, and a
// removal is what the freeze rule forbids.
func TestMarkerFieldsAreFrozen(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(Marker{
		Format: MarkerFormat, SelfUpdateID: "id", FromVersion: "a",
		TargetVersion: "b", BinaryPath: "/p", StagedAt: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"format", "self_update_id", "from_version", "target_version",
		"binary_path", "staged_at",
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Errorf("the frozen field %q is missing from the marker", name)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("the marker carries %d fields, the frozen set has %d: %v",
			len(fields), len(want), fields)
	}
}

// TestWriteMarkerIsCompleteOrAbsent asserts the write of section 12.1 step 6: a
// temp file, fsync, rename, fsync the directory — so the marker is complete or
// absent on disk, never half-written.
//
// The observable half of that promise is what this checks: after the write the
// directory contains exactly `pending` and nothing else, so no `.tmp` scratch
// survives to be mistaken for a marker or swept as debris.
func TestWriteMarkerIsCompleteOrAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l := Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}

	m := Marker{
		Format: MarkerFormat, SelfUpdateID: "01J", FromVersion: "v1.1.0",
		TargetVersion: "v1.2.0", BinaryPath: "/usr/local/bin/llamaman", StagedAt: 7,
	}
	if err := WriteMarker(l.PendingPath(), m); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	entries, err := os.ReadDir(l.UpdateDir())
	if err != nil {
		t.Fatalf("read the update directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != PendingFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("after the write the update directory holds %v, want just %q",
			names, PendingFileName)
	}

	got, err := ReadMarker(l.PendingPath())
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if got != m {
		t.Errorf("round trip: got %+v, want %+v", got, m)
	}
}

// TestReadMarkerDistinguishesAbsentFromUnreadable is the distinction the two
// sentinels exist for, and folding them together would be a real bug: "no
// marker" means no update is in flight and the gate goes straight to its closing
// pass, while an unreadable one means an update WAS in flight and this is its
// wreckage — which still has a row and a job to close.
func TestReadMarkerDistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l := Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}

	if _, err := ReadMarker(l.PendingPath()); !errors.Is(err, ErrNoMarker) {
		t.Errorf("an absent marker: got %v, want ErrNoMarker", err)
	}

	writeFile(t, l.PendingPath(), []byte("{not json"))
	if _, err := ReadMarker(l.PendingPath()); !errors.Is(err, ErrMarkerUnreadable) {
		t.Errorf("a malformed marker: got %v, want ErrMarkerUnreadable", err)
	}
}

// TestRemoveMarkerIsIdempotent: every caller of the unlink is re-run by the next
// boot and by the 30 s ticker, so "already gone" is the ordinary second call.
func TestRemoveMarkerIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l := Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}
	if err := WriteMarker(l.PendingPath(), Marker{
		Format: MarkerFormat, TargetVersion: "v1.2.0",
	}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := RemoveMarker(l.PendingPath()); err != nil {
			t.Fatalf("RemoveMarker call %d: %v", i+1, err)
		}
	}
}

// TestClearScratchKeepsALiveMarker is the ordering the two writing branches of
// the gate depend on: the caller decides the marker's fate, and a sweep that
// took it along would race the one file the whole protocol is keyed on.
func TestClearScratchKeepsALiveMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	l := Layout{StateDir: dir}
	if err := l.EnsureUpdateDir(); err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}
	if err := WriteMarker(l.PendingPath(), Marker{
		Format: MarkerFormat, TargetVersion: "v1.2.0",
	}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	writeFile(t, filepath.Join(l.UpdateDir(), "llamaman_v1.2.0_linux_amd64.tar.gz"), []byte("tarball"))
	writeFile(t, filepath.Join(l.UpdateDir(), ChecksumsName), []byte("checksums"))

	if err := l.ClearScratch(); err != nil {
		t.Fatalf("ClearScratch: %v", err)
	}
	if !exists(l.PendingPath()) {
		t.Error("ClearScratch removed the live marker")
	}
	if exists(filepath.Join(l.UpdateDir(), ChecksumsName)) {
		t.Error("ClearScratch left the scratch behind")
	}
}
