package prebuilt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/toolchain"
)

// The prebuilt pipeline end to end (DESIGN section 6.4), as one function over a
// filesystem and a Fetcher, returning everything the install worker needs to
// write rows — and touching no database itself (D49 invariant 1).
//
//	1. download  the asset into tmp/, SHA-256 inline, compared with GitHub's
//	             digest when present
//	2. extract   into versions/<id>.staging with the hardened reader
//	3. verify    D18's execute-on-this-host check, plus the help capture
//	4. install   write manifest.json, then rename the staging tree into place
//
// The step boundaries are the ones section 2.5's transition table names, so a
// failure's `failing_step` is a value that table already defines rather than a
// string invented here.

// InstallRequest is one prebuilt install.
type InstallRequest struct {
	// VersionID is `<tag>-<backend>-bin` (D60) and is also the directory name.
	VersionID string
	// Tag is `llamacpp_versions.tag`; BuildTag the `b#####` a stable release
	// pinned. Both are recorded in the manifest and neither is derived here —
	// resolution is internal/llamacpp/github's job.
	Tag      string
	BuildTag string
	Channel  model.LlamacppChannel
	Backend  model.Backend

	// The asset to download. AssetURL is a CDN URL and is fetched with NO
	// Authorization header (section 6.2).
	AssetName string
	AssetURL  string
	// PublishedSHA256 is GitHub's own digest for the asset, when it published
	// one. Empty means there is nothing to compare against, which is not an
	// error — plenty of older releases carry no digest — and the computed hash
	// is recorded either way.
	PublishedSHA256 string
	// AssetReleaseTag is the release the asset actually came from, which on the
	// stable channel is the pinned build rather than the semver tag.
	AssetReleaseTag string

	// VersionsRoot is `<state_dir>/versions`; TmpDir is `<state_dir>/tmp`.
	VersionsRoot string
	TmpDir       string

	// Fetcher downloads the asset. Nil uses HTTPFetcher.
	Fetcher Fetcher
	// Guard is D25's live-process check, asked before a swap. Nil is correct
	// only when the target directory does not exist.
	Guard DirGuard
	// Run executes the verification probes. Nil uses internal/procx.
	Run Runner
	// HostLibc is this host's C library. The zero value probes it once.
	HostLibc toolchain.Libc
	// BuiltBy is the llamaman version doing the install, recorded in the
	// manifest.
	BuiltBy string
	// Progress is called as each step advances. Nil is allowed.
	Progress ProgressReporter
	// KeepArchive leaves the downloaded tarball in TmpDir. Default false: it is
	// tens of megabytes and the extracted tree is what matters.
	KeepArchive bool
	// Now supplies the manifest timestamp. Nil uses time.Now.
	Now func() time.Time
	// Extract overrides the extraction limits. The zero value uses
	// DefaultExtractOptions.
	Extract ExtractOptions
}

// ProgressReporter receives step progress. `done`/`total` are bytes during the
// download and zero elsewhere; `note` is a short human phrase.
type ProgressReporter func(step model.FailingStep, done, total int64, note string)

func (r ProgressReporter) report(step model.FailingStep, done, total int64, note string) {
	if r != nil {
		r(step, done, total, note)
	}
}

// InstallResult is what the worker turns into `llamacpp_versions` columns.
type InstallResult struct {
	// VersionDir is the published directory, empty when the install did not
	// reach the publish step.
	VersionDir string
	// Manifest is what was written into the tree.
	Manifest Manifest
	// Verify is the acceptance test's full verdict, including the ELF
	// diagnosis when D18 rejected the binary.
	Verify VerifyResult
	// Extraction is what the hardened reader produced.
	Extraction ExtractResult
	// ArchiveSHA256 is the digest of the bytes that were extracted.
	ArchiveSHA256 string
	SizeBytes     int64
	// FailingStep is section 2.5's `failing_step`, empty on success.
	FailingStep model.FailingStep
	// SourceFallback is D18's signal to insert a `<tag>-<backend>-src` row
	// beside this one and link them through `superseded_by`.
	SourceFallback bool
}

// Install runs the whole prebuilt pipeline.
//
// On a verification rejection it returns a result with SourceFallback set and
// an error wrapping ErrVerification: that outcome is `failed_verification` plus
// a source build, not a crash, and the caller needs the diagnosis that travels
// in the result to carry into the replacement row's `params_json`.
//
// The staging directory is NOT removed on failure — the same rule section 6.5
// applies to build directories (D4). What went wrong is usually visible in the
// tree, and a retry that has to download 40 MB again for no reason is a worse
// product. Cancellation and a caller that wants it gone call CleanStaging.
func Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	var res InstallResult
	if req.VersionID == "" || req.VersionsRoot == "" {
		return res, errors.New("prebuilt: Install needs a version id and a versions root")
	}
	if req.AssetURL == "" {
		return res, errors.New("prebuilt: Install needs an asset URL")
	}
	now := req.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	hostLibc := req.HostLibc
	if hostLibc.Kind == "" {
		hostLibc = toolchain.Glibc(ctx)
	}
	fetcher := req.Fetcher
	if fetcher == nil {
		fetcher = &HTTPFetcher{}
	}
	tmpDir := req.TmpDir
	if tmpDir == "" {
		tmpDir = filepath.Join(req.VersionsRoot, "..", "tmp")
	}

	staging := StagingDir(req.VersionsRoot, req.VersionID)
	target := VersionDir(req.VersionsRoot, req.VersionID)

	// --- 1. download ---------------------------------------------------------
	name := req.AssetName
	if name == "" {
		name = req.VersionID + ".tar.gz"
	}
	archive := filepath.Join(tmpDir, name)
	req.Progress.report(model.StepFetch, 0, 0, "downloading "+name)

	sum, err := fetcher.Fetch(ctx, req.AssetURL, archive, req.PublishedSHA256,
		func(done, total int64) { req.Progress.report(model.StepFetch, done, total, "") })
	if err != nil {
		res.FailingStep = model.StepFetch
		return res, err
	}
	res.ArchiveSHA256 = sum
	if !req.KeepArchive {
		defer os.Remove(archive)
	}

	// --- 2. extract ----------------------------------------------------------
	req.Progress.report(model.StepFetch, 0, 0, "extracting into "+filepath.Base(staging))
	// A staging directory left by a previous attempt is cleared rather than
	// merged into: the hardened reader refuses to overwrite an existing file,
	// so a half-extracted tree would fail every retry until someone deleted it
	// by hand.
	if err := os.RemoveAll(staging); err != nil {
		res.FailingStep = model.StepFetch
		return res, fmt.Errorf("prebuilt: clearing %s: %w", staging, err)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		res.FailingStep = model.StepFetch
		return res, err
	}

	f, err := os.Open(archive)
	if err != nil {
		res.FailingStep = model.StepFetch
		return res, err
	}
	extractOpts := req.Extract
	if extractOpts == (ExtractOptions{}) {
		extractOpts = DefaultExtractOptions()
	}
	ex, err := ExtractTarGz(f, staging, extractOpts)
	f.Close()
	if err != nil {
		res.FailingStep = model.StepFetch
		res.Extraction = ex
		return res, err
	}
	res.Extraction = ex

	// --- 2a. normalize the layout --------------------------------------------
	// Upstream ships two shapes and only one of them is what StripTopLevel was
	// written for. See layout.go: a flat release grows a `bin/` and `lib/` view
	// of itself here, and a tree that already has `bin/` is untouched.
	normalized, err := NormalizeLayout(staging)
	if err != nil {
		res.FailingStep = model.StepFetch
		return res, err
	}
	if normalized {
		req.Progress.report(model.StepFetch, 0, 0,
			"normalized a flat release into bin/ and lib/")
	}

	// --- 3. verify (D18, D19) ------------------------------------------------
	req.Progress.report(model.StepVerify, 0, 0, "running llama-server --version on this host")
	vr, err := Verify(ctx, VerifyOptions{
		Root:     staging,
		Backend:  req.Backend,
		HostLibc: hostLibc,
		Run:      req.Run,
	})
	res.Verify = vr
	if err != nil {
		res.FailingStep = model.StepVerify
		return res, err
	}
	if !vr.OK {
		res.FailingStep = model.StepVerify
		res.SourceFallback = vr.SourceFallback
		return res, vr.Err
	}

	// --- 4. install ----------------------------------------------------------
	size, err := DirSize(staging)
	if err != nil {
		res.FailingStep = model.StepInstall
		return res, err
	}
	res.SizeBytes = size

	m := Manifest{
		ManifestVersion: ManifestVersion,
		VersionID:       req.VersionID,
		Tag:             req.Tag,
		BuildTag:        req.BuildTag,
		Channel:         string(req.Channel),
		Acquisition:     string(model.AcquisitionPrebuilt),
		Backend:         string(req.Backend),
		AssetName:       req.AssetName,
		AssetURL:        req.AssetURL,
		AssetSHA256:     sum,
		PublishedSHA256: req.PublishedSHA256,
		AssetReleaseTag: req.AssetReleaseTag,
		BuiltAt:         now(),
		BuiltBy:         req.BuiltBy,
		HostGlibc:       hostLibc.VersionString,
		TopLevel:        ex.TopLevel,
		Binaries:        vr.Binaries,
		SizeBytes:       size,
		ServerHelp:      vr.HelpOutput,
		HelpFlags:       []string(vr.HelpFlags),
		SupportsFit:     vr.SupportsFit,
		DevicesOutput:   vr.DevicesOutput,
		VersionOutput:   vr.VersionOutput,
	}
	if err := WriteManifest(staging, m); err != nil {
		res.FailingStep = model.StepInstall
		return res, err
	}
	res.Manifest = m

	req.Progress.report(model.StepInstall, 0, 0, "publishing "+req.VersionID)
	if err := Publish(ctx, PublishOptions{Staging: staging, Target: target, Guard: req.Guard}); err != nil {
		res.FailingStep = model.StepInstall
		return res, err
	}
	res.VersionDir = target
	return res, nil
}
