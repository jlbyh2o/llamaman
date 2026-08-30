package prebuilt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

func elfFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", "elf", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return p
}

func glibcHost(t *testing.T, version string) toolchain.Libc {
	t.Helper()
	v, ok := toolchain.ParseVersion(version)
	if !ok {
		t.Fatalf("bad host version in the test table: %q", version)
	}
	return toolchain.Libc{Kind: toolchain.LibcGlibc, Version: v, VersionString: v.String(), Source: "test"}
}

// TestDiagnoseGlibcTooOld is D18's headline: the sentence a user reads instead
// of a loader error. Section 6.4 fixes its shape — "requires GLIBC_2.38, host
// has 2.36" — because it is quoted straight into the UI.
func TestDiagnoseGlibcTooOld(t *testing.T) {
	d := Diagnose(elfFixture(t, "needs-glibc-2.38"), glibcHost(t, "2.36"))

	if !d.GlibcTooOld {
		t.Fatalf("a binary requiring GLIBC_2.38 was accepted on a 2.36 host: %+v", d)
	}
	if d.MaxGlibc != "2.38" || d.HostGlibc != "2.36" {
		t.Errorf("versions = required %q, host %q", d.MaxGlibc, d.HostGlibc)
	}
	if d.Summary != "requires GLIBC_2.38, host has 2.36" {
		t.Errorf("summary = %q, want the sentence section 6.4 specifies", d.Summary)
	}
	if !d.Actionable() {
		t.Error("the diagnosis is not marked actionable")
	}
	if len(d.Requirements) == 0 {
		t.Error("no requirements recorded for the details pane")
	}
}

func TestDiagnoseGlibcNewEnough(t *testing.T) {
	// The same binary on a host that HAS 2.38: glibc is not the cause, and the
	// diagnosis must say so rather than blaming it anyway. A prebuilt can fail
	// to run for other reasons, and a wrong explanation is worse than none.
	tests := []struct {
		name string
		host string
	}{
		{name: "exactly the required version", host: "2.38"},
		{name: "newer", host: "2.43"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(elfFixture(t, "needs-glibc-2.38"), glibcHost(t, tc.host))
			if d.GlibcTooOld {
				t.Errorf("GLIBC_2.38 reported as too new for a %s host", tc.host)
			}
			if !strings.Contains(d.Summary, "not the cause") {
				t.Errorf("summary = %q", d.Summary)
			}
		})
	}
}

func TestDiagnoseOldSymbolVersionsRunAnywhere(t *testing.T) {
	d := Diagnose(elfFixture(t, "needs-glibc-2.2.5"), glibcHost(t, "2.28"))
	if d.GlibcTooOld {
		t.Errorf("a binary needing only GLIBC_2.2.5 was rejected: %+v", d)
	}
	if d.MaxGlibc != "2.2.5" {
		t.Errorf("max glibc = %q, want 2.2.5", d.MaxGlibc)
	}
}

func TestDiagnoseUnknownHostLibcMakesNoVerdict(t *testing.T) {
	// Never invent a comparison. A host whose libc could not be determined gets
	// the requirement reported and an explicit "could not be determined" —
	// a zero-valued host version would read as 0.0 and condemn every prebuilt.
	d := Diagnose(elfFixture(t, "needs-glibc-2.38"), toolchain.Libc{Kind: toolchain.LibcUnknown})
	if d.GlibcTooOld {
		t.Error("a verdict was reached against an unknown host libc")
	}
	if d.MaxGlibc != "2.38" {
		t.Errorf("max glibc = %q", d.MaxGlibc)
	}
	if !strings.Contains(d.Summary, "could not be determined") {
		t.Errorf("summary = %q", d.Summary)
	}
}

func TestDiagnoseMuslHost(t *testing.T) {
	// The Alpine case: the tarball is glibc-linked and asks for a loader that
	// does not exist here, so the kernel answers ENOENT about a file that
	// plainly exists. The diagnosis has to name the real cause.
	p := elfFixture(t, "needs-musl-loader")
	d := Diagnose(p, toolchain.Libc{Kind: toolchain.LibcMusl, VersionString: "1.2.5"})

	if d.Interpreter != "/lib/ld-musl-x86_64.so.1" {
		t.Errorf("interpreter = %q", d.Interpreter)
	}
	if _, err := os.Stat(d.Interpreter); err == nil {
		t.Skip("this host actually has a musl loader; the missing-interpreter branch cannot be exercised here")
	}
	if !d.InterpreterMissing {
		t.Error("the missing loader was not detected")
	}
	if !strings.Contains(d.Summary, "musl") || !strings.Contains(d.Summary, "glibc-linked") {
		t.Errorf("summary = %q, want one naming musl and the glibc linkage", d.Summary)
	}
	if !d.Actionable() {
		t.Error("the diagnosis is not marked actionable")
	}
}

func TestDiagnoseWrongArchitecture(t *testing.T) {
	// An arm64 tarball on an amd64 host is the other way a prebuilt fails to
	// execute, and it is one no source build of the same asset would fix.
	d := Diagnose(elfFixture(t, "wrong-arch-aarch64"), glibcHost(t, "2.43"))
	if !d.ArchMismatch {
		t.Fatalf("an AArch64 binary was accepted on this host: %+v", d)
	}
	if !strings.Contains(d.Summary, "architecture") {
		t.Errorf("summary = %q", d.Summary)
	}
}

func TestDiagnoseMultipleLibraries(t *testing.T) {
	d := Diagnose(elfFixture(t, "needs-glibc-and-libstdcxx"), glibcHost(t, "2.36"))

	var libs []string
	for _, r := range d.Requirements {
		libs = append(libs, r.Library+" "+r.Version)
	}
	joined := strings.Join(libs, ", ")
	for _, want := range []string{"libc.so.6 GLIBC_2.38", "libstdc++.so.6 CXXABI_1.3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("requirements %q are missing %q", joined, want)
		}
	}
	// Only GLIBC_* decides the glibc verdict; a CXXABI version must not be
	// parsed as one.
	if d.MaxGlibc != "2.38" {
		t.Errorf("max glibc = %q, want 2.38", d.MaxGlibc)
	}
	formatted := FormatRequirements(d.Requirements)
	if !strings.Contains(formatted, "libc.so.6 [") || !strings.Contains(formatted, "libstdc++.so.6 [") {
		t.Errorf("FormatRequirements = %q", formatted)
	}
}

func TestDiagnoseUnreadableFile(t *testing.T) {
	// A file that is not an ELF at all must produce a diagnosis, not an error:
	// it is replacing an already-failed execution, and swallowing that failure
	// behind a parse error would leave the user with nothing.
	tests := []struct {
		name string
		path string
	}{
		{name: "not an ELF", path: elfFixture(t, "not-an-elf")},
		{name: "does not exist", path: filepath.Join(t.TempDir(), "absent")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Diagnose(tc.path, glibcHost(t, "2.36"))
			if d.Actionable() {
				t.Errorf("an unreadable file produced an actionable finding: %+v", d)
			}
			if d.Summary == "" {
				t.Error("no summary; the user would see nothing")
			}
		})
	}
}

func TestGlibcVersionOrdering(t *testing.T) {
	// The regression this guards: GLIBC_2.9 must not sort above GLIBC_2.34, and
	// the MAXIMUM requirement is the one that decides whether the loader
	// succeeds.
	reqs := []GlibcRequirement{
		{Library: "libc.so.6", Version: "GLIBC_2.9"},
		{Library: "libc.so.6", Version: "GLIBC_2.34"},
		{Library: "libc.so.6", Version: "GLIBC_2.2.5"},
		{Library: "libstdc++.so.6", Version: "GLIBCXX_3.4.32"},
	}
	got, ok := maxGlibcVersion(reqs)
	if !ok || got != "2.34" {
		t.Errorf("maxGlibcVersion = (%q, %v), want 2.34", got, ok)
	}
}
