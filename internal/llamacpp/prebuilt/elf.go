package prebuilt

import (
	"debug/elf"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// The D18 diagnosis (DESIGN section 6.4 step 3): when `bin/llama-server
// --version` does not exit 0, "`debug/elf` parses `.gnu.version_r` from the ELF
// and compares required `GLIBC_*` versions against the host's, producing
// 'requires GLIBC_2.38, host has 2.36' instead of a raw loader error".
//
// This is the whole reason SPEC section 3.7's distro-agnostic promise can be
// kept, and it costs one stdlib package. Upstream's tarballs are built on
// Ubuntu against whatever glibc that runner had; run one on Debian stable, RHEL
// 9 or Amazon Linux 2 and the dynamic loader says
//
//	./llama-server: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.38' not found
//
// on a good day and `Exec format error` on a bad one. Neither tells a user what
// to do. The version-needed table in the binary says exactly which glibc it
// wants, this host's own libc says what it has, and the difference is a
// sentence a person can act on — followed by the source build that fixes it
// automatically.

// GlibcRequirement is one required symbol version from one library.
type GlibcRequirement struct {
	// Library is the DT_NEEDED name the requirement came from: `libc.so.6`,
	// `libstdc++.so.6`, `libm.so.6`.
	Library string `json:"library"`
	// Version is the symbol-version name: `GLIBC_2.38`, `GLIBCXX_3.4.32`,
	// `CXXABI_1.3.15`.
	Version string `json:"version"`
}

// Diagnosis is why a prebuilt binary would not execute on this host, in the
// form section 6.4 asks for. It is carried into the source-build row's
// `params_json` so the UI can say, in one line, "prebuilt rejected (requires
// GLIBC_2.38, host has 2.36) — built from source instead".
type Diagnosis struct {
	// Binary is the file that was examined.
	Binary string `json:"binary"`
	// MaxGlibc is the highest `GLIBC_*` version the binary requires, the number
	// that actually decides whether it runs.
	MaxGlibc string `json:"max_glibc,omitempty"`
	// HostGlibc is what this host has, empty when it could not be determined.
	HostGlibc string `json:"host_glibc,omitempty"`
	// GlibcTooOld is the finding: the binary requires a newer glibc than this
	// host has. It is false when the host libc is unknown — an unknown is never
	// reported as a mismatch.
	GlibcTooOld bool `json:"glibc_too_old"`
	// Requirements is every symbol version found, sorted, for the details pane.
	Requirements []GlibcRequirement `json:"requirements,omitempty"`
	// Interpreter is PT_INTERP, the dynamic loader the binary asks for. On a
	// musl host this is the giveaway: `/lib64/ld-linux-x86-64.so.2` does not
	// exist there at all, which is why the kernel answers ENOENT and the shell
	// says "No such file or directory" about a file that plainly exists.
	Interpreter string `json:"interpreter,omitempty"`
	// InterpreterMissing is true when Interpreter names a path this host does
	// not have.
	InterpreterMissing bool `json:"interpreter_missing"`
	// Machine is the ELF architecture, and ArchMismatch is true when it is not
	// this host's — an arm64 tarball on an amd64 host, the other way a prebuilt
	// fails to execute.
	Machine      string `json:"machine,omitempty"`
	ArchMismatch bool   `json:"arch_mismatch"`
	// Summary is the one-line human answer.
	Summary string `json:"summary"`
}

// Actionable reports whether the diagnosis found a specific, nameable cause. A
// diagnosis that found none still carries a Summary, but the UI phrases it as
// "would not execute" rather than as an explanation.
func (d Diagnosis) Actionable() bool {
	return d.GlibcTooOld || d.InterpreterMissing || d.ArchMismatch
}

// glibcVersionPrefix is the symbol-version namespace glibc uses.
const glibcVersionPrefix = "GLIBC_"

// Diagnose examines a binary that would not run and explains why, as far as the
// ELF headers allow. It never returns an error for a file it simply cannot
// understand: a diagnosis that says "no specific cause found" is more useful
// than an error that replaces the original failure.
func Diagnose(binary string, hostLibc toolchain.Libc) Diagnosis {
	d := Diagnosis{Binary: binary}
	if hostLibc.Known() {
		d.HostGlibc = hostLibc.VersionString
	}

	f, err := elf.Open(binary)
	if err != nil {
		d.Summary = fmt.Sprintf("%s could not be read as an ELF binary: %v", binary, err)
		return d
	}
	defer f.Close()

	d.Machine = f.Machine.String()
	if want, ok := elfMachineForHost(); ok && f.Machine != want {
		d.ArchMismatch = true
	}

	if interp, err := readInterpreter(f); err == nil && interp != "" {
		d.Interpreter = interp
		if _, err := os.Stat(interp); errors.Is(err, fs.ErrNotExist) {
			d.InterpreterMissing = true
		}
	}

	d.Requirements = readVersionNeeds(f)
	maxGlibc, haveGlibc := maxGlibcVersion(d.Requirements)
	if haveGlibc {
		d.MaxGlibc = maxGlibc
	}

	// The comparison only happens when BOTH numbers are real. A host whose libc
	// could not be determined gets the requirement reported and no verdict —
	// never a mismatch invented from a zero.
	if haveGlibc && hostLibc.Kind == toolchain.LibcGlibc && hostLibc.Known() {
		req := glibcNumber(maxGlibc)
		if req.Known() && req.Compare(hostLibc.Version) > 0 {
			d.GlibcTooOld = true
		}
	}

	d.Summary = summarize(d, hostLibc)
	return d
}

// summarize renders the one-line answer. It is the sentence that reaches a
// user, so its shape is fixed by section 6.4 for the case that matters:
// "requires GLIBC_2.38, host has 2.36".
func summarize(d Diagnosis, hostLibc toolchain.Libc) string {
	switch {
	case d.ArchMismatch:
		return fmt.Sprintf("this tarball is built for %s, not for this host's architecture", d.Machine)

	case d.GlibcTooOld:
		// The sentence section 6.4 specifies, verbatim in shape.
		return fmt.Sprintf("requires GLIBC_%s, host has %s", d.MaxGlibc, d.HostGlibc)

	case d.InterpreterMissing && hostLibc.Kind == toolchain.LibcMusl:
		return fmt.Sprintf("this host uses musl libc and the tarball is glibc-linked "+
			"(it asks for the loader %s, which does not exist here)", d.Interpreter)

	case d.InterpreterMissing:
		return fmt.Sprintf("the dynamic loader it asks for (%s) is not present on this host", d.Interpreter)

	case d.MaxGlibc != "" && d.HostGlibc == "":
		return fmt.Sprintf("requires GLIBC_%s; this host's glibc version could not be determined", d.MaxGlibc)

	case d.MaxGlibc != "":
		return fmt.Sprintf("requires GLIBC_%s and this host has %s, so the glibc version is not the cause",
			d.MaxGlibc, d.HostGlibc)

	default:
		return "the binary would not execute on this host and the ELF headers name no specific cause"
	}
}

// readVersionNeeds reads `.gnu.version_r` — the SHT_GNU_verneed section — which
// lists, per shared library, every symbol version this binary needs from it.
func readVersionNeeds(f *elf.File) []GlibcRequirement {
	needs, err := f.DynamicVersionNeeds()
	if err != nil || len(needs) == 0 {
		return nil
	}
	var out []GlibcRequirement
	for _, n := range needs {
		for _, dep := range n.Needs {
			out = append(out, GlibcRequirement{Library: n.Name, Version: dep.Dep})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Library != out[j].Library {
			return out[i].Library < out[j].Library
		}
		return compareVersionNames(out[i].Version, out[j].Version) < 0
	})
	return out
}

// maxGlibcVersion returns the highest `GLIBC_x.y[.z]` requirement, which is the
// one that decides whether the loader succeeds.
func maxGlibcVersion(reqs []GlibcRequirement) (string, bool) {
	var best toolchain.Version
	var bestStr string
	for _, r := range reqs {
		if !strings.HasPrefix(r.Version, glibcVersionPrefix) {
			continue
		}
		v := glibcNumber(r.Version)
		if !v.Known() {
			continue
		}
		if bestStr == "" || v.Compare(best) > 0 {
			best, bestStr = v, strings.TrimPrefix(r.Version, glibcVersionPrefix)
		}
	}
	return bestStr, bestStr != ""
}

// glibcNumber turns `GLIBC_2.38` — or a bare `2.38` — into a comparable
// version.
func glibcNumber(s string) toolchain.Version {
	s = strings.TrimPrefix(s, glibcVersionPrefix)
	v, ok := toolchain.ParseVersion(s)
	if !ok {
		return toolchain.Version{}
	}
	return v
}

// compareVersionNames orders symbol-version names numerically within a
// namespace, so `GLIBC_2.9` sorts before `GLIBC_2.34` rather than after it.
func compareVersionNames(a, b string) int {
	ap, an, aok := strings.Cut(a, "_")
	bp, bn, bok := strings.Cut(b, "_")
	if !aok || !bok || ap != bp {
		return strings.Compare(a, b)
	}
	av, aok := toolchain.ParseVersion(an)
	bv, bok := toolchain.ParseVersion(bn)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	return av.Compare(bv)
}

// readInterpreter returns PT_INTERP, the loader path baked into the binary.
func readInterpreter(f *elf.File) (string, error) {
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		buf := make([]byte, p.Filesz)
		if _, err := p.ReadAt(buf, 0); err != nil {
			return "", err
		}
		return strings.TrimRight(string(buf), "\x00"), nil
	}
	return "", nil
}

// FormatRequirements renders the requirement list for a log line, most
// significant first. It exists so the build log and the API say the same thing.
func FormatRequirements(reqs []GlibcRequirement) string {
	if len(reqs) == 0 {
		return ""
	}
	byLib := map[string][]string{}
	var libs []string
	for _, r := range reqs {
		if _, seen := byLib[r.Library]; !seen {
			libs = append(libs, r.Library)
		}
		byLib[r.Library] = append(byLib[r.Library], r.Version)
	}
	sort.Strings(libs)
	var parts []string
	for _, lib := range libs {
		vs := byLib[lib]
		parts = append(parts, lib+" ["+strings.Join(vs, " ")+"]")
	}
	return strings.Join(parts, "; ")
}

// elfMachineForHost is the ELF machine this host executes. The second return
// is false for an architecture this table does not know, in which case no
// mismatch is reported — an unknown is never a finding.
func elfMachineForHost() (elf.Machine, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return elf.EM_X86_64, true
	case "arm64":
		return elf.EM_AARCH64, true
	case "386":
		return elf.EM_386, true
	case "arm":
		return elf.EM_ARM, true
	case "s390x":
		return elf.EM_S390, true
	case "riscv64":
		return elf.EM_RISCV, true
	case "ppc64le", "ppc64":
		return elf.EM_PPC64, true
	default:
		return 0, false
	}
}
