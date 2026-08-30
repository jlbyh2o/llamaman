package source

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// Tools is the resolved toolchain ONE build will exec: absolute paths, plus the
// cmake version the manifest records.
//
// The probe itself belongs to internal/toolchain, which section 6.5's
// `preflight` row names outright and which the wizard's toolchain step and
// `GET /api/v1/llamacpp/plan` already read. This struct is the projection of
// that report onto the four binaries this pipeline actually runs, so a build and
// a plan can never disagree about whether a host can build — they are looking at
// the same probe.
type Tools struct {
	Git   string
	CMake string
	// CMakeVersion is the version the probe parsed, for manifest.json.
	CMakeVersion string
	// Ninja is empty when it is absent, in which case Make must be present and
	// the generator is Unix Makefiles.
	Ninja string
	Make  string
	CC    string
	CXX   string
	// NVCC is required only for a CUDA build. It is recorded rather than
	// executed: cmake is what invokes it.
	NVCC string
	// CCache is optional. When present the build sets
	// CMAKE_{C,CXX}_COMPILER_LAUNCHER, which makes rebuilds of nearby commits
	// fast (section 6.5).
	CCache string
}

// Generator is the cmake generator name: Ninja when it is installed, else Unix
// Makefiles (section 6.5).
func (t Tools) Generator() string {
	if t.Ninja != "" {
		return "Ninja"
	}
	return "Unix Makefiles"
}

// ToolsFromReport projects a toolchain report onto the paths this pipeline
// execs. A tool the report did not find is left empty rather than guessed at.
func ToolsFromReport(r toolchain.Report) Tools {
	path := func(name string) string {
		t, ok := r.Tool(name)
		if !ok || !t.Found {
			return ""
		}
		return t.Path
	}
	t := Tools{
		Git:    path(toolchain.ToolGit),
		CMake:  path(toolchain.ToolCMake),
		Ninja:  path(toolchain.ToolNinja),
		Make:   path(toolchain.ToolMake),
		CC:     path(toolchain.ToolGCC),
		CXX:    path(toolchain.ToolGXX),
		NVCC:   path(toolchain.ToolNvcc),
		CCache: path(toolchain.ToolCcache),
	}
	if cm, ok := r.Tool(toolchain.ToolCMake); ok {
		t.CMakeVersion = cm.Version
	}
	return t
}

// probeToolchain runs section 6.5's `preflight` and refuses the build when the
// host cannot complete it.
//
// The refusal carries the probe's own per-tool notes, which name the package to
// install for this distribution and deliberately contain no command to run as
// root — section 6.5's "per-distro guidance and NEVER a package-manager call".
func probeToolchain(ctx context.Context, opts toolchain.Options, backend model.Backend) (Tools, toolchain.Report, error) {
	report := toolchain.Probe(ctx, opts)
	tools := ToolsFromReport(report)
	if report.CanBuild(backend) {
		return tools, report, nil
	}

	// Missing() can name `make` on a host that has ninja and can build
	// perfectly well, so the refusal is gated on CanBuild and only then
	// itemized — never the other way round.
	missing := report.Missing(backend)
	details := make([]string, 0, len(missing))
	hints := make([]string, 0, len(missing))
	for _, name := range missing {
		t, ok := report.Tool(name)
		if !ok {
			details = append(details, name)
			continue
		}
		if t.Note != "" {
			details = append(details, fmt.Sprintf("%s (%s)", name, t.Note))
			hints = append(hints, fmt.Sprintf("%s: %s", name, t.Note))
			continue
		}
		details = append(details, name)
	}
	if len(details) == 0 {
		// CanBuild is false with nothing itemized only when the generator pair
		// is the problem: neither ninja nor make.
		details = append(details, "ninja or make")
		hints = append(hints, "ninja or make: one build generator has to be present; "+
			"ninja is preferred and make is the fallback")
	}

	return tools, report, &Failure{
		Phase: PhasePreflight,
		Code:  CodeToolchainMissing,
		Message: fmt.Sprintf("this host cannot build llama.cpp from source (%s backend): missing %s",
			backend, strings.Join(details, "; ")),
		Hint: strings.Join(hints, " "),
	}
}
