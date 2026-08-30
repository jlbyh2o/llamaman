package source

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Phase is one row of DESIGN section 6.5's table: "each step is a named phase
// written to `jobs.progress_json`, prefixed in `build.log`, and recorded in
// `failing_step` on error".
type Phase string

const (
	// PhasePreflight probes the toolchain. Missing pieces abort here with
	// per-distro guidance and NEVER a package-manager call.
	PhasePreflight Phase = "preflight"
	// PhaseSpace requires 12 GiB free for CUDA, 3 GiB for CPU.
	PhaseSpace Phase = "space"
	// PhaseFetch clones or fetches, then adds the worktree and its submodules.
	PhaseFetch Phase = "fetch"
	// PhaseConfigure runs cmake with section 6.5's flag set.
	PhaseConfigure Phase = "configure"
	// PhaseCompile runs `cmake --build -j N` (D20).
	PhaseCompile Phase = "compile"
	// PhaseInstall runs `cmake --install` into `versions/<id>.staging` (D78).
	PhaseInstall Phase = "install"
	// PhaseVerify runs D18's execute-on-this-host check and, for CUDA, D19's
	// --list-devices check, against the STAGING tree.
	PhaseVerify Phase = "verify"
	// PhasePublish writes manifest.json and renames the staging tree into
	// place.
	PhasePublish Phase = "publish"
)

// Phases lists the pipeline in order.
func Phases() []Phase {
	return []Phase{
		PhasePreflight, PhaseSpace, PhaseFetch, PhaseConfigure,
		PhaseCompile, PhaseInstall, PhaseVerify, PhasePublish,
	}
}

// FailingStep maps a phase to the value written to
// `llamacpp_versions.failing_step`.
//
// `publish` folds to `install` because model.FailingStep is the closed set
// section 2.5's column comment names and `publish` is not in it. Nothing is
// lost: the phase itself is in the Failure and in the log, and a failure at
// publish IS a failure to install — the binaries exist but are not where a
// version directory has to be.
func (p Phase) FailingStep() model.FailingStep {
	if p == PhasePublish {
		return model.StepInstall
	}
	return model.FailingStep(p)
}

// Progress is one frame of `jobs.progress_json` (section 6.5). Every field but
// Phase is omitted when it has nothing to say, so a phase with no counters is
// two keys rather than eight nulls.
type Progress struct {
	// Phase is the step now running.
	Phase Phase `json:"phase"`

	// Pct is 0-100 when the phase can say; nil when it cannot. A build spends
	// most of its life in `compile`, which is the one phase that CAN say —
	// ninja's own `[812/1930]` counter — and pretending the others have a
	// percentage would be inventing one.
	Pct *int `json:"pct,omitempty"`

	// Done and Total are ninja's edge counters, kept beside Pct because "812 of
	// 1930 files" is a better answer than "42%" for a build that is about to
	// spend four minutes on one ggml-cuda translation unit.
	Done  int `json:"done,omitempty"`
	Total int `json:"total,omitempty"`

	// Jobs is the `-j N` in effect (D20), so the UI can say why a build on a
	// memory-starved host is slow.
	Jobs int `json:"jobs,omitempty"`

	// OOMRetry marks the automatic second compile at -j1 after an OOM kill
	// (D20). It is what makes the UI able to state the reason rather than
	// silently taking four times as long.
	OOMRetry bool `json:"oom_retry,omitempty"`

	// Message is one short human sentence, when the phase has one.
	Message string `json:"message,omitempty"`
}

// Observer receives progress frames. The `llamacpp_install` worker implements
// it over jobs.Task.SetProgress; a build nobody is watching passes nil.
//
// An error from Progress does NOT fail the build: a job row that cannot be
// written is a database problem, and abandoning a CUDA compile eight minutes in
// because a progress write failed would be the wrong trade. The build pipeline
// logs it and carries on.
type Observer interface {
	Progress(ctx context.Context, p Progress) error
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(ctx context.Context, p Progress) error

// Progress calls f.
func (f ObserverFunc) Progress(ctx context.Context, p Progress) error { return f(ctx, p) }

// ninjaProgressRe matches ninja's edge counter, which it prints at the start of
// every line when stdout is not a terminal: "[812/1930] Building CXX object …".
var ninjaProgressRe = regexp.MustCompile(`^\[(\d+)/(\d+)\]`)

// makeProgressRe matches the Unix Makefiles generator's own form, used on a host
// with no ninja: "[ 42%] Building CXX object …".
var makeProgressRe = regexp.MustCompile(`^\[\s*(\d{1,3})%\]`)

// parseBuildProgress reads one line of build output and reports what it says
// about progress. done/total are ninja's counters; pct is derived from them, or
// taken directly from the make generator's percentage.
func parseBuildProgress(line string) (done, total, pct int, ok bool) {
	line = strings.TrimSpace(line)
	if m := ninjaProgressRe.FindStringSubmatch(line); m != nil {
		done, _ = strconv.Atoi(m[1])
		total, _ = strconv.Atoi(m[2])
		if total <= 0 {
			return 0, 0, 0, false
		}
		if done > total {
			done = total
		}
		return done, total, done * 100 / total, true
	}
	if m := makeProgressRe.FindStringSubmatch(line); m != nil {
		pct, _ = strconv.Atoi(m[1])
		if pct > 100 {
			pct = 100
		}
		return 0, 0, pct, true
	}
	return 0, 0, 0, false
}
