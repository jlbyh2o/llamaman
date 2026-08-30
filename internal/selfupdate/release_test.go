package selfupdate

import "testing"

// Version ordering (DESIGN section 12.4, D90).
//
// Two questions in this design are asked of it and no others: "is there an
// update available", and "is this tag older than the one running" — the second
// being what makes the update dialog say "downgrade" and print section 12.4's
// five commands. **Installing an older release is the ordinary update flow**;
// nothing in section 12.1-12.3 distinguishes a downgrade from an upgrade, and
// neither does the judge.
func TestCompareVersions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.0", "v1.1.0", 1},
		{"v1.1.0", "v1.2.0", -1},
		{"v1.2.0", "v1.2.0", 0},
		{"1.2.0", "v1.2.0", 0},
		{"v1.10.0", "v1.9.0", 1}, // numeric, not lexical
		{"v2.0.0", "v1.99.99", 1},
		{"v1.2.0", "v1.2", 0}, // a missing component is zero
		{"v1.2.1", "v1.2", 1},
		{"v1.2.0-rc.1", "v1.2.0", -1}, // a prerelease sorts below its release
		{"v1.2.0", "v1.2.0-rc.1", 1},
		{"v1.2.0-rc.1", "v1.2.0-rc.2", -1},
	}
	for _, tc := range cases {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			t.Parallel()
			if got := CompareVersions(tc.a, tc.b); got != tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestTarballNameIsTheReleaseAssetName pins section 16.2 step 2's asset name,
// because it is a cross-component contract: the release job writes it, the
// checksums file names it, and the daemon asks for it by name.
func TestTarballNameIsTheReleaseAssetName(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"amd64": "llamaman_v1.2.0_linux_amd64.tar.gz",
		"arm64": "llamaman_v1.2.0_linux_arm64.tar.gz",
	}
	for arch, want := range cases {
		if got := TarballName("v1.2.0", arch); got != want {
			t.Errorf("TarballName(v1.2.0, %s) = %q, want %q", arch, got, want)
		}
	}
}

// TestSnapshotNameCarriesTheVersionBeingReplaced is the arithmetic D14's
// retention rule rests on: the label is the version being REPLACED, so the
// newest snapshot is by construction the database the version now at
// `<prefix>/llamaman.prev` left behind.
func TestSnapshotNameCarriesTheVersionBeingReplaced(t *testing.T) {
	t.Parallel()

	if got := SnapshotName("v1.1.0", 1788012345); got != "llamaman-v1.1.0-1788012345.db" {
		t.Errorf("SnapshotName = %q", got)
	}
	// A version string that could walk out of db-backups/ is sanitized rather
	// than trusted: this name is joined onto a directory and written as root's
	// neighbor.
	if got := SnapshotName("../../etc/v1", 1); got != "llamaman-.._.._etc_v1-1.db" {
		t.Errorf("an unsafe version string produced %q", got)
	}
}
