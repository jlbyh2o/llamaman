package toolchain

import (
	"context"
	"strings"
)

// The host C library, which is the second half of DESIGN's D18 acceptance check
// (section 6.4 step 3): a prebuilt tarball that will not execute is diagnosed by
// comparing the `GLIBC_*` versions its ELF requires against THIS host's, so
// something has to know what this host's is.
//
// There is no way to ask libc directly from a CGO_ENABLED=0 binary — that is
// the whole point of the constraint — so the answer comes from the two programs
// every glibc installation ships: `getconf GNU_LIBC_VERSION`, which prints
// exactly `glibc 2.43` and nothing else, and `ldd --version`, whose first line
// is `ldd (GNU libc) 2.43` on glibc and `musl libc (x86_64)` on musl.
//
// musl is detected rather than ignored because it is the one host where the D18
// fallback is not a nicety: upstream's tarballs are Ubuntu-built and glibc-linked,
// so on Alpine every prebuilt fails verification and the source build is the only
// path. Saying so up front is better than saying it after a download.

// LibcKind is which C library the host has.
type LibcKind string

const (
	LibcGlibc   LibcKind = "glibc"
	LibcMusl    LibcKind = "musl"
	LibcUnknown LibcKind = "unknown"
)

// Libc is the host C library as this package could determine it.
type Libc struct {
	Kind    LibcKind `json:"kind"`
	Version Version  `json:"-"`
	// VersionString is Version rendered, and is what result_json carries.
	VersionString string `json:"version,omitempty"`
	// Source names how it was learned — "getconf" or "ldd" — so a surprising
	// answer can be reproduced by hand in one command.
	Source string `json:"source,omitempty"`
}

// Known reports whether a comparison against this host's libc is meaningful. A
// prebuilt whose required GLIBC versions cannot be compared to anything gets a
// diagnosis that says so rather than one that invents a number.
func (l Libc) Known() bool { return l.Kind != LibcUnknown && l.Version.Known() }

// Glibc probes this host's C library with the default runner. It is the
// convenience entry point for callers outside a full Probe — the prebuilt
// pipeline's ELF diagnosis, above all.
func Glibc(ctx context.Context) Libc {
	l, _ := probeLibc(ctx, Options{}, FamilyUnknown)
	return l
}

func probeLibc(ctx context.Context, opts Options, fam Family) (Libc, Tool) {
	t := Tool{Name: ToolGlibc, OK: true}
	if g, okg := GuidanceFor(ToolGlibc); okg {
		t.DocsURL = g.DocsURL
	}

	l := probeLibcVia(ctx, opts, "getconf", []string{"GNU_LIBC_VERSION"}, "getconf")
	if !l.Known() {
		if alt := probeLibcVia(ctx, opts, "ldd", []string{"--version"}, "ldd"); alt.Kind != LibcUnknown {
			l = alt
		}
	}

	t.Found = l.Kind != LibcUnknown
	t.Path = ""
	t.Version = l.Version.String()
	switch l.Kind {
	case LibcGlibc:
		t.Note = "the version a prebuilt tarball is checked against (D18)"
	case LibcMusl:
		t.Note = "musl libc: upstream's prebuilt tarballs are glibc-linked and will not execute here, " +
			"so every install builds from source"
	default:
		t.Note = "could not determine the host C library; a prebuilt that fails to execute will be " +
			"reported without a version comparison"
	}
	_ = fam
	return l, t
}

func probeLibcVia(ctx context.Context, opts Options, bin string, args []string, source string) Libc {
	path, err := opts.lookPath()(bin)
	if err != nil {
		return Libc{Kind: LibcUnknown}
	}
	cctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	out, code, runErr := opts.run()(cctx, path, args...)
	if runErr != nil {
		return Libc{Kind: LibcUnknown}
	}
	// `ldd --version` exits 0 on glibc; musl's exits 1 while printing exactly
	// what we need, so the exit status is not the gate — the output is.
	_ = code
	l := ParseLibc(out)
	if l.Kind != LibcUnknown {
		l.Source = source
	}
	return l
}

// ParseLibc reads the output of `getconf GNU_LIBC_VERSION` or `ldd --version`.
// It is exported so the parser can be table-tested against captured output from
// hosts this machine is not.
func ParseLibc(out string) Libc {
	lower := strings.ToLower(out)
	if strings.Contains(lower, "musl libc") {
		l := Libc{Kind: LibcMusl}
		// musl prints the version on its own line after the banner:
		//   musl libc (x86_64)
		//   Version 1.2.5
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(strings.ToLower(line)), "version") {
				if v, okv := ParseVersion(line); okv {
					l.Version = v
					l.VersionString = v.String()
				}
				break
			}
		}
		return l
	}
	for _, line := range strings.Split(out, "\n") {
		low := strings.ToLower(line)
		if !strings.Contains(low, "glibc") && !strings.Contains(low, "gnu libc") {
			continue
		}
		if v, okv := ParseVersion(line); okv {
			return Libc{Kind: LibcGlibc, Version: v, VersionString: v.String()}
		}
	}
	return Libc{Kind: LibcUnknown}
}
