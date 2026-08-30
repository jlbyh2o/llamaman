package prebuilt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

func readFixture(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// fakeTree writes a version tree with the binaries named, so Verify's stat
// checks see a real filesystem while the Runner answers for the executions.
func fakeTree(t *testing.T, bins ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range bins {
		if err := os.WriteFile(filepath.Join(root, "bin", b), []byte("ELF"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runs is a scripted Runner: one canned answer per `<binary> <first arg>`.
type runs map[string]struct {
	out  string
	code int
	err  error
}

func (r runs) runner() Runner {
	return func(_ context.Context, name string, args ...string) (string, int, error) {
		key := filepath.Base(name)
		if len(args) > 0 {
			key += " " + args[0]
		}
		a, ok := r[key]
		if !ok {
			return "", 127, nil
		}
		return a.out, a.code, a.err
	}
}

// healthyRuns is what a working llama.cpp tree answers.
func healthyRuns(t *testing.T) runs {
	t.Helper()
	return runs{
		"llama-server --version":      {out: "version: 6821 (a1b2c3d4)\nbuilt with cc (GCC) 13.2.0 for x86_64-linux-gnu\n"},
		"llama-server --help":         {out: readFixture(t, "help", "llama-server-help.txt")},
		"llama-server --list-devices": {out: readFixture(t, "devices", "cuda-two-gpus.txt")},
		"llama-bench -h":              {out: "usage: llama-bench [options]\n", code: 1},
	}
}

func TestVerifyAcceptsAWorkingTree(t *testing.T) {
	root := fakeTree(t, BinServer, BinBench, BinCLI)
	res, err := Verify(t.Context(), VerifyOptions{
		Root: root, Backend: model.BackendCUDA,
		HostLibc: glibcHost(t, "2.43"), Run: healthyRuns(t).runner(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("verification failed: %v (check %q)", res.Err, res.FailingCheck)
	}
	if !strings.Contains(res.VersionOutput, "6821") {
		t.Errorf("version output not captured: %q", res.VersionOutput)
	}
	// The two columns section 5.7 turns the help capture into.
	if !res.SupportsFit {
		t.Error("supports_fit is false for a build whose help advertises --fit")
	}
	if !res.HelpFlags.Has("-ngl") || !res.HelpFlags.Has("--no-webui") {
		t.Errorf("help flags are missing entries: %v", res.HelpFlags)
	}
	if res.HelpOutput == "" {
		t.Error("the verbatim help capture is empty; manifest.json would have nothing to record")
	}
	if !strings.Contains(res.DevicesOutput, "CUDA0") {
		t.Errorf("devices_output = %q", res.DevicesOutput)
	}
	if len(res.Binaries) != 3 {
		t.Errorf("binaries = %v", res.Binaries)
	}
}

// TestVerifyD18Rejection is the acceptance test's whole reason for existing: the
// tarball is fine, the file is there, and it does not run on THIS host.
func TestVerifyD18Rejection(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real ELF requiring GLIBC_2.38, so the diagnosis has something to parse.
	elf, err := os.ReadFile(elfFixture(t, "needs-glibc-2.38"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", BinServer), elf, 0o755); err != nil {
		t.Fatal(err)
	}

	r := healthyRuns(t)
	r["llama-server --version"] = struct {
		out  string
		code int
		err  error
	}{out: "./llama-server: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.38' not found\n", code: 127}

	res, err := Verify(t.Context(), VerifyOptions{
		Root: root, Backend: model.BackendCPU,
		HostLibc: glibcHost(t, "2.36"), Run: r.runner(),
	})
	if err != nil {
		t.Fatalf("Verify returned a Go error for a rejected binary: %v", err)
	}
	if res.OK {
		t.Fatal("a binary that would not execute was accepted")
	}
	if res.FailingCheck != "execute" {
		t.Errorf("failing check = %q, want execute", res.FailingCheck)
	}
	// D18's fallback signal: the caller inserts `<tag>-cpu-src` beside this row
	// and links them with superseded_by.
	if !res.SourceFallback {
		t.Error("no source-build fallback was signaled; SPEC section 3.7's promise depends on it")
	}
	if res.Diagnosis == nil || !res.Diagnosis.GlibcTooOld {
		t.Fatalf("no glibc diagnosis: %+v", res.Diagnosis)
	}
	if !strings.Contains(res.Err.Error(), "requires GLIBC_2.38, host has 2.36") {
		t.Errorf("error %q does not carry the diagnosis", res.Err)
	}
	if !errors.Is(res.Err, ErrVerification) {
		t.Errorf("error %v is not an ErrVerification", res.Err)
	}
}

func TestVerifyD19CUDADeviceCheck(t *testing.T) {
	tests := []struct {
		name         string
		backend      model.Backend
		devices      string
		wantOK       bool
		wantFallback bool
		wantCheck    string
	}{
		{name: "cuda build sees its GPUs", backend: model.BackendCUDA, devices: "devices/cuda-two-gpus.txt", wantOK: true},
		{
			name: "cuda build silently fell back to CPU", backend: model.BackendCUDA,
			devices: "devices/cuda-fell-back.txt", wantCheck: "cuda-devices",
			// Terminal: rebuilding on this host would list no device either.
			wantFallback: false,
		},
		{
			name: "cuda build listing only a CPU device", backend: model.BackendCUDA,
			devices: "devices/cpu-only.txt", wantCheck: "cuda-devices",
		},
		{
			name: "a CPU build is not asked for a CUDA device", backend: model.BackendCPU,
			devices: "devices/cpu-only.txt", wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeTree(t, BinServer, BinBench)
			r := healthyRuns(t)
			parts := strings.Split(tc.devices, "/")
			r["llama-server --list-devices"] = struct {
				out  string
				code int
				err  error
			}{out: readFixture(t, parts...)}

			res, err := Verify(t.Context(), VerifyOptions{
				Root: root, Backend: tc.backend, HostLibc: glibcHost(t, "2.43"), Run: r.runner(),
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (err %v)", res.OK, tc.wantOK, res.Err)
			}
			if res.FailingCheck != tc.wantCheck {
				t.Errorf("failing check = %q, want %q", res.FailingCheck, tc.wantCheck)
			}
			if res.SourceFallback != tc.wantFallback {
				t.Errorf("source fallback = %v, want %v", res.SourceFallback, tc.wantFallback)
			}
			// The devices output is captured for every backend, because "what
			// did this build see" is worth having on a CPU build too.
			if res.DevicesOutput == "" {
				t.Error("devices_output was not captured")
			}
		})
	}
}

func TestVerifyBenchIsCheckedByIdentityNotExitStatus(t *testing.T) {
	// Section 6.4 step 3 is explicit: llama-bench has its own argument parser
	// and an unrecognized flag exits non-zero, so its presence is asserted by
	// stat plus a `-h` run whose OUTPUT mentions llama-bench.
	tests := []struct {
		name    string
		present bool
		out     string
		code    int
		wantOK  bool
	}{
		{name: "non-zero exit but identifies itself", present: true, out: "usage: llama-bench [options]\n", code: 1, wantOK: true},
		{name: "exit 0 and identifies itself", present: true, out: "llama-bench\n", code: 0, wantOK: true},
		{name: "runs but is some other program", present: true, out: "usage: something-else\n", code: 0},
		{name: "missing from the archive", present: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bins := []string{BinServer}
			if tc.present {
				bins = append(bins, BinBench)
			}
			root := fakeTree(t, bins...)
			r := healthyRuns(t)
			r["llama-bench -h"] = struct {
				out  string
				code int
				err  error
			}{out: tc.out, code: tc.code}

			res, err := Verify(t.Context(), VerifyOptions{
				Root: root, Backend: model.BackendCPU, HostLibc: glibcHost(t, "2.43"), Run: r.runner(),
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (err %v)", res.OK, tc.wantOK, res.Err)
			}
			if !tc.wantOK && res.FailingCheck != BinBench {
				t.Errorf("failing check = %q, want llama-bench", res.FailingCheck)
			}
		})
	}
}

func TestVerifyMissingServerIsNotASourceFallback(t *testing.T) {
	// A tarball with no server binary is a broken or wrong download, and a
	// source build of the same tag would not "fix" it. Signaling a fallback
	// here would enqueue a build for the wrong reason.
	root := fakeTree(t, BinBench)
	res, err := Verify(t.Context(), VerifyOptions{
		Root: root, Backend: model.BackendCPU, HostLibc: glibcHost(t, "2.43"), Run: healthyRuns(t).runner(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK || res.SourceFallback {
		t.Errorf("OK = %v, fallback = %v; want a plain failure", res.OK, res.SourceFallback)
	}
}

func TestVerifyProbesTheHostLibcWhenNotSupplied(t *testing.T) {
	// The zero Libc means "probe this host", not "unknown": a caller that
	// forgets the field must still get a real diagnosis.
	root := fakeTree(t, BinServer, BinBench)
	res, err := Verify(t.Context(), VerifyOptions{
		Root: root, Backend: model.BackendCPU, Run: healthyRuns(t).runner(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("verification failed: %v", res.Err)
	}
}

func TestVerifyNeedsARoot(t *testing.T) {
	if _, err := Verify(t.Context(), VerifyOptions{}); err == nil {
		t.Fatal("Verify with no root succeeded")
	}
}

// --------------------------------------------------------------- help parsing

func TestParseHelpFlags(t *testing.T) {
	flags := ParseHelpFlags(readFixture(t, "help", "llama-server-help.txt"))

	// Every flag RenderArgv can emit must be recognized, or the flag-churn
	// guard (section 5.7) would warn about flags the build plainly has.
	for _, want := range []string{
		"-m", "-c", "-b", "-ub", "-np", "-ngl", "-fa", "-ctk", "-ctv", "-a", "-md",
		"--model", "--ctx-size", "--gpu-layers", "--n-gpu-layers", "--jinja",
		"--host", "--port", "--no-webui", "--props", "--slots", "--metrics",
		"--tensor-split", "--main-gpu", "--device", "--list-devices", "--fit",
	} {
		if !flags.Has(want) {
			t.Errorf("help flags are missing %q", want)
		}
	}
	// Aliases on one line are all recorded: `-ngl, --gpu-layers, --n-gpu-layers`
	// is three names for one flag and argv may carry any of them.
	if !flags.Has("-dkvc") || !flags.Has("--dump-kv-cache") {
		t.Error("a comma-separated alias run was not fully parsed")
	}
	// Value placeholders are not flags.
	for _, unwanted := range []string{"FNAME", "N", "TYPE", "[on|off]", "--", "-"} {
		if flags.Has(unwanted) {
			t.Errorf("%q was parsed as a flag", unwanted)
		}
	}
	if !flags.Available() {
		t.Error("a parsed capture reports itself unavailable")
	}
}

func TestParseHelpFlagsIgnoresProse(t *testing.T) {
	// A description mentioning a flag is a mention, not an advertisement.
	// Treating it as one silences the flag-churn guard exactly when it matters.
	help := "-m, --model FNAME    model path; use --hf-repo to download instead\n" +
		"                     see also --no-mmap\n"
	flags := ParseHelpFlags(help)
	if !flags.Has("-m") || !flags.Has("--model") {
		t.Errorf("flags = %v", flags)
	}
	if flags.Has("--hf-repo") {
		t.Error("a flag mentioned in a description was recorded as advertised")
	}
	if flags.Has("--no-mmap") {
		t.Error("a continuation line's prose was parsed as a flag list")
	}
}

func TestSupportsFitIsDerivedFromTheCapture(t *testing.T) {
	// D51: a build that predates `--fit` renders `-ngl auto` as `-ngl 999`
	// instead of omitting the flag, and this capture is where that is decided.
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{name: "current build", fixture: "llama-server-help.txt", want: true},
		{name: "build predating --fit", fixture: "llama-server-help-no-fit.txt", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flags := ParseHelpFlags(readFixture(t, "help", tc.fixture))
			if got := flags.Has(FitFlag); got != tc.want {
				t.Errorf("supports_fit = %v, want %v", got, tc.want)
			}
			// The rest of the flag set is unaffected either way.
			if !flags.Has("-ngl") {
				t.Error("-ngl went missing")
			}
		})
	}
}

func TestParseHelpFlagsEmptyCapture(t *testing.T) {
	// `help_flags_json IS NULL` must read as "check unavailable", never as
	// "every flag is unknown" (section 5.7).
	flags := ParseHelpFlags("")
	if flags.Available() {
		t.Error("an empty capture reports itself available")
	}
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none", flags)
	}
}

func TestHasCUDADevice(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{name: "two CUDA devices", fixture: "cuda-two-gpus.txt", want: true},
		{name: "CPU only", fixture: "cpu-only.txt", want: false},
		{name: "CUDA init failed", fixture: "cuda-fell-back.txt", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasCUDADevice(readFixture(t, "devices", tc.fixture)); got != tc.want {
				t.Errorf("HasCUDADevice = %v, want %v", got, tc.want)
			}
		})
	}
	if HasCUDADevice("") {
		t.Error("empty output reported a CUDA device")
	}
}
