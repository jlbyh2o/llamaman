package model

// llama.cpp versions (DESIGN section 2.5).
//
// A version's identity is THREE-PART (D60) — tag, backend, acquisition — because
// the same upstream tag may legitimately exist on one host as a CPU prebuilt, a
// CPU source build (the D18 fallback for that very prebuilt) and a CUDA source
// build. `llamacpp_versions.id` is `<tag>-<backend>-<acq>` and `dir_name` equals
// it, so the three enums below are also the on-disk layout.

// LlamacppChannel is `llamacpp_versions.channel` (§2.5) and the value of the
// `llamacpp.channel` setting.
type LlamacppChannel string

const (
	ChannelStable  LlamacppChannel = "stable"
	ChannelNightly LlamacppChannel = "nightly"
	ChannelCustom  LlamacppChannel = "custom"
)

// LlamacppChannelValues lists the members of the `llamacpp_versions.channel`
// CHECK constraint, in order.
func LlamacppChannelValues() []LlamacppChannel {
	return []LlamacppChannel{ChannelStable, ChannelNightly, ChannelCustom}
}

// Valid reports whether c is a member of the CHECK constraint.
func (c LlamacppChannel) Valid() bool { return valid(c, LlamacppChannelValues()) }

// Acquisition is `llamacpp_versions.acquisition` (§2.5): how the binaries got
// here. It is part of the identity, not a detail — a prebuilt that fails D18's
// execute-on-this-host check is superseded by a SOURCE build of the same tag,
// and both rows coexist.
type Acquisition string

const (
	AcquisitionPrebuilt Acquisition = "prebuilt"
	AcquisitionSource   Acquisition = "source"
)

// AcquisitionValues lists the members of the `llamacpp_versions.acquisition`
// CHECK constraint, in order.
func AcquisitionValues() []Acquisition {
	return []Acquisition{AcquisitionPrebuilt, AcquisitionSource}
}

// Valid reports whether a is a member of the CHECK constraint.
func (a Acquisition) Valid() bool { return valid(a, AcquisitionValues()) }

// Backend is `llamacpp_versions.backend` (§2.5).
type Backend string

const (
	BackendCPU  Backend = "cpu"
	BackendCUDA Backend = "cuda"
)

// BackendValues lists the members of the `llamacpp_versions.backend` CHECK
// constraint, in order.
func BackendValues() []Backend { return []Backend{BackendCPU, BackendCUDA} }

// Valid reports whether b is a member of the CHECK constraint.
func (b Backend) Valid() bool { return valid(b, BackendValues()) }

// VersionState is `llamacpp_versions.state` (§2.5).
//
// Transitions — every one of them writes an `events` row:
//
//	from            event                                    to                    side effect
//	--------------- ---------------------------------------- --------------------- --------------------------------
//	—               POST /llamacpp/versions                   pending               insert row + llamacpp_install job
//	pending         worker leases                             resolving             resolve channel → tag → asset,
//	                                                                                or git ls-remote the ref
//	resolving       prebuilt asset exists, cpu, pref on       fetching              download tarball to tmp/
//	resolving       cuda, custom, no asset, D18 rejection     building              fetch + configure + compile
//	fetching        hardened extract ok                       verifying             D18 execute-on-this-host check
//	building        compile + install exit 0                  verifying             D18 + D19 checks
//	verifying       ok                                        ready                 write manifest.json, binaries_json,
//	                                                                                size_bytes, help_flags_json,
//	                                                                                supports_fit
//	terminal        POST for the same id, or …/retry          pending               reuse-and-reset (D71): clear the
//	failure                                                                         error columns and superseded_by,
//	                                                                                rotate the build log, enqueue a
//	                                                                                fresh install job. The prior
//	                                                                                failure survives in `events`
//	verifying       prebuilt fails to execute                 failed_verification   auto-enqueue a CPU SOURCE build as
//	                                                                                a NEW row beside it and link the
//	                                                                                two through superseded_by
//	verifying       CUDA build lists no CUDA device           failed_verification   terminal; log kept
//	any pre-ready   error                                     failed                keep log + failing_step
//	any pre-ready   cancel                                    canceled              SIGTERM the group, remove partials
//	ready           activate                                  ready, is_active=1    prior active → previous_active=1;
//	                                                                                RecomputeConfigHash; symlink flip;
//	                                                                                canary roll
//	ready,          canary failed (§6.6 step 5)               ready, both flags     ONE transaction: revert the flags,
//	is_active=1                                               restored              re-run RecomputeConfigHash, repair
//	                                                                                the symlinks FROM the rows, restart
//	                                                                                the canary on the old build
//	ready           delete (not active, not previous,         deleting → deleted    remove dir
//	                not /proc/*/exe)
//	deleting        removal failed or daemon restarted,       ready                 usable again; the failure is in
//	                bin/llama-server still executes                                 `events` and in the job row
//	deleting        removal failed, directory incomplete      failed                failing_step='delete',
//	                                                                                error_code='delete_incomplete'
//
// The symlink invariant behind the last rows: `versions/active` always points at
// the is_active=1 row's directory, and on boot THE ROW WINS and the symlink is
// repaired from it. That is why a canary revert must be a database write and not
// a filesystem operation — flipping the symlink back while leaving is_active=1
// on the build whose canary just failed creates a disagreement the next boot
// resolves IN FAVOR OF THE FAILED BUILD, re-pointing the symlink at it and
// restarting every instance onto it.
type VersionState string

const (
	VersionPending            VersionState = "pending"
	VersionResolving          VersionState = "resolving"
	VersionFetching           VersionState = "fetching"
	VersionBuilding           VersionState = "building"
	VersionVerifying          VersionState = "verifying"
	VersionReady              VersionState = "ready"
	VersionFailed             VersionState = "failed"
	VersionFailedVerification VersionState = "failed_verification"
	VersionCanceled           VersionState = "canceled"
	VersionDeleting           VersionState = "deleting"
	VersionDeleted            VersionState = "deleted"
)

// VersionStateValues lists the members of the `llamacpp_versions.state` CHECK
// constraint, in order.
func VersionStateValues() []VersionState {
	return []VersionState{
		VersionPending, VersionResolving, VersionFetching, VersionBuilding,
		VersionVerifying, VersionReady, VersionFailed, VersionFailedVerification,
		VersionCanceled, VersionDeleting, VersionDeleted,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s VersionState) Valid() bool { return valid(s, VersionStateValues()) }

// IsTerminalFailure reports whether a row in this state is eligible for D71's
// reuse-and-reset back to `pending`.
func (s VersionState) IsTerminalFailure() bool {
	return s == VersionFailed || s == VersionFailedVerification ||
		s == VersionCanceled || s == VersionDeleted
}

// FailingStep is `llamacpp_versions.failing_step` (§2.5). The column is
// documented by a comment rather than closed by a CHECK — a new build pipeline
// step must not be a migration — so this type is the application's own closed
// set and is deliberately absent from ClosedEnums. 'delete' is a member because
// §2.5's edge out of `deleting` writes it.
type FailingStep string

const (
	StepPreflight FailingStep = "preflight"
	StepSpace     FailingStep = "space"
	StepFetch     FailingStep = "fetch"
	StepConfigure FailingStep = "configure"
	StepCompile   FailingStep = "compile"
	StepInstall   FailingStep = "install"
	StepVerify    FailingStep = "verify"
	StepDelete    FailingStep = "delete"
)

// FailingStepValues lists the build steps the design names, in pipeline order.
func FailingStepValues() []FailingStep {
	return []FailingStep{
		StepPreflight, StepSpace, StepFetch, StepConfigure, StepCompile,
		StepInstall, StepVerify, StepDelete,
	}
}

// Valid reports whether s is one of the steps the design names.
func (s FailingStep) Valid() bool { return valid(s, FailingStepValues()) }

// ReleaseSource is `release_cache.source` (§2.5): whose GitHub releases this
// cached row came from.
type ReleaseSource string

const (
	ReleaseLlamacpp ReleaseSource = "llamacpp"
	ReleaseLlamaman ReleaseSource = "llamaman"
)

// ReleaseSourceValues lists the members of the `release_cache.source` CHECK
// constraint, in order.
func ReleaseSourceValues() []ReleaseSource { return []ReleaseSource{ReleaseLlamacpp, ReleaseLlamaman} }

// Valid reports whether s is a member of the CHECK constraint.
func (s ReleaseSource) Valid() bool { return valid(s, ReleaseSourceValues()) }
