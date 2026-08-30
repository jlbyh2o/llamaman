package llamacpp

import (
	"context"

	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// `GET /api/v1/llamacpp/plan` — DESIGN section 6.3.
//
// "The plan endpoint returns the decision WITH ITS REASON, the detected CUDA
// architectures, the missing toolchain items and a free-space check before the
// user commits. That is the difference between 'build failed after four minutes'
// and 'install cmake first'."
//
// It is deliberately the same code path the install POST takes: `resolve` does
// the channel lookup and `decideAcquisition` makes §6.3's decision, so a plan
// and the install that follows it cannot disagree. What this adds is everything
// that is a fact about the HOST rather than about the request — the toolchain
// probe, the CUDA architectures, the free space — which the install learns only
// once its worker is running.

// PlanRequest is the query of `GET /api/v1/llamacpp/plan`.
type PlanRequest struct {
	// Channel is stable, nightly or custom; empty means stable.
	Channel model.LlamacppChannel
	// Tag pins a release, as it does on the install POST.
	Tag string
	// Backend is cpu or cuda; empty means cpu.
	Backend model.Backend
	// GitURL and GitRef describe a custom build, so a plan can be asked for one
	// without inserting a row.
	GitURL string
	GitRef string
	// ForceSource is §6.3's override, planned rather than performed.
	ForceSource bool
}

// Plan is what that endpoint answers with.
type Plan struct {
	// VersionID is the three-part id this request resolves to (D60). It is on
	// the wire because the answer to "what would happen" includes "and it would
	// be called this" — and because a `ready` row with that id is what makes the
	// UI say "already installed" without a second request.
	VersionID string `json:"version_id"`
	// Acquisition is `prebuilt` or `source` — §6.3's decision.
	Acquisition model.Acquisition     `json:"acquisition"`
	Backend     model.Backend         `json:"backend"`
	Channel     model.LlamacppChannel `json:"channel"`
	Tag         string                `json:"tag"`
	// BuildTag is the `b#####` a stable release pinned through nightly-tag.txt,
	// empty on the other channels. The UI shows both ("v0.3.0 — build b10621").
	BuildTag string `json:"build_tag,omitempty"`
	// Reason is the sentence §6.3 asks for: why this branch and not the other.
	Reason string `json:"reason"`
	// AssetName is the tarball a prebuilt would fetch, empty for a source build.
	AssetName string `json:"asset_name,omitempty"`

	// EstimatedMinutes is a rough wall-clock estimate, not a promise. It is on
	// the wire because "this will take about nine minutes" is the single most
	// useful thing to say before a CUDA compile, and because a user who is told
	// nothing assumes it hung.
	EstimatedMinutes int `json:"estimated_minutes"`
	// MissingTools are the toolchain items that would abort the `preflight`
	// phase — Report.Missing for this backend, which is the same list the build
	// itself would refuse on.
	MissingTools []string `json:"missing_tools"`
	// CUDAArch are D21's detected compute capabilities, in cmake's spelling.
	// Empty on a CPU plan, and empty on a CUDA plan means the build would fail
	// its own preflight rather than guess.
	CUDAArch []string `json:"cuda_arch"`
	// FreeSpaceOK is the `space` phase's check, evaluated now: 12 GiB for CUDA,
	// 3 GiB for CPU (internal/toolchain owns both numbers).
	FreeSpaceOK bool `json:"free_space_ok"`
	// FreeBytes and RequiredBytes are the numbers behind that boolean, because
	// "not enough space" without them is not something a user can act on.
	FreeBytes     int64 `json:"free_bytes"`
	RequiredBytes int64 `json:"required_bytes"`
	// CanProceed folds the three checks: nothing missing, enough room, and — on
	// a CUDA source build — at least one detected architecture.
	CanProceed bool `json:"can_proceed"`
}

// Rough wall-clock estimates for the two pipelines, in minutes. They are
// deliberately coarse: the point of the number is to tell a fetch from a
// compile, not to be right to the minute on hardware this daemon has never seen.
const (
	prebuiltEstimateMinutes  = 1
	cpuBuildEstimateMinutes  = 6
	cudaBuildEstimateMinutes = 12
)

// PlanInstall answers `GET /api/v1/llamacpp/plan` without writing anything.
func (s *Service) PlanInstall(ctx context.Context, req PlanRequest) (Plan, error) {
	ident, err := s.resolve(ctx, InstallRequest{
		Channel:     req.Channel,
		Tag:         req.Tag,
		GitURL:      req.GitURL,
		GitRef:      req.GitRef,
		Backend:     req.Backend,
		ForceSource: req.ForceSource,
	})
	if err != nil {
		return Plan{}, err
	}

	out := Plan{
		VersionID:   ident.ID,
		Acquisition: ident.Acquisition,
		Backend:     ident.Backend,
		Channel:     ident.Channel,
		Tag:         ident.Tag,
		BuildTag:    ident.BuildTag,
		AssetName:   ident.AssetName,
		Reason:      planReason(req, ident),
	}
	if out.Acquisition == model.AcquisitionPrebuilt {
		out.AssetName = ident.AssetName
		out.EstimatedMinutes = prebuiltEstimateMinutes
	} else if ident.Backend == model.BackendCUDA {
		out.EstimatedMinutes = cudaBuildEstimateMinutes
	} else {
		out.EstimatedMinutes = cpuBuildEstimateMinutes
	}

	// The host half. A prebuilt needs no compiler, so its missing-tools list is
	// empty by construction rather than by omission — and its space requirement
	// is still checked, because a tarball still has to land.
	report := s.probeToolchain(ctx)
	out.CUDAArch = report.CUDAArch
	if out.Acquisition == model.AcquisitionSource {
		out.MissingTools = report.Missing(ident.Backend)
	}
	if out.MissingTools == nil {
		out.MissingTools = []string{}
	}
	if out.CUDAArch == nil {
		out.CUDAArch = []string{}
	}

	out.RequiredBytes = toolchain.RequiredFreeBytes(ident.Backend)
	free, err := s.freeSpace(s.layout.VersionsRoot())
	if err == nil {
		out.FreeBytes = int64(free)
		out.FreeSpaceOK = int64(free) >= out.RequiredBytes
	} else {
		// A statfs that could not run is reported as "unknown", not as "fine":
		// this endpoint exists to stop a build that was going to fail.
		s.log.Warn("could not measure free space for the plan", "error", err)
	}

	out.CanProceed = len(out.MissingTools) == 0 && out.FreeSpaceOK
	if out.CanProceed && out.Acquisition == model.AcquisitionSource &&
		ident.Backend == model.BackendCUDA && len(out.CUDAArch) == 0 &&
		ident.CUDAArchList == "" {
		// D21: a CUDA build with no detected capability and no configured list
		// fails its own preflight rather than compiling for something nobody
		// asked for. Saying so here is the whole point of the endpoint.
		out.CanProceed = false
	}
	return out, nil
}

// planReason is §6.3's decision table read back as a sentence.
func planReason(req PlanRequest, id identity) string {
	switch {
	case id.Acquisition == model.AcquisitionPrebuilt:
		return "a Linux CPU prebuilt exists for this architecture and " +
			"llamacpp.prefer_prebuilt_cpu is on"
	case id.Backend == model.BackendCUDA:
		return "no Linux CUDA prebuilt exists, so CUDA is always built from source"
	case id.Channel == model.ChannelCustom:
		return "a custom build is a git ref, which has no release asset"
	case req.ForceSource:
		return "force_source was requested"
	case id.AssetName == "":
		return "this release publishes no prebuilt asset for this architecture"
	default:
		return "llamacpp.prefer_prebuilt_cpu is off"
	}
}

// probeToolchain runs the host probe, or returns the zero report when this
// daemon was built without one. A zero report has no tools, so `Missing` names
// every required one — which reads as "this host cannot build" and is the safe
// direction for a plan to be wrong in.
func (s *Service) probeToolchain(ctx context.Context) toolchain.Report {
	if s.probe != nil {
		return s.probe.Probe(ctx)
	}
	return toolchain.Probe(ctx, toolchain.Options{})
}

// freeSpace measures the filesystem the version directories live on. It is the
// same measurement the source pipeline's `space` phase makes, so a plan that
// said "enough room" and a build that refused for want of it cannot both be
// right.
func (s *Service) freeSpace(path string) (uint64, error) {
	if s.space != nil {
		return s.space(path)
	}
	return source.FreeSpaceBytes(path)
}
