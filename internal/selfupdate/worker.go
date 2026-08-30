package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The pipeline (DESIGN section 12.1 steps 2 through 7).
//
// Three commits and one file write, in this order and no other:
//
//	planned → downloading → verifying → [snapshot] → staged → [marker] → swapping
//
// The `staged` commit is the CANCEL CUT-OFF (D96): up to it a cancel moves row
// and job to `canceled` in one transaction and clears scratch; from it on the
// answer is `409 selfupdate_not_cancelable`, because the next step writes a
// marker to disk and the step after that hands the work to systemd.

// The error codes this worker writes to `jobs.error_code`. The domain row
// carries the message (§2.3a).
const (
	codeDownloadFailed = "update_download_failed"
	codeVerifyFailed   = "update_verify_failed"
	codeSnapshotFailed = "update_snapshot_failed"
	codeStageFailed    = "update_stage_failed"
)

// Run implements jobs.Worker.
func (s *Service) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	id := t.Job().SubjectID

	var params workerParams
	if t.Job().ParamsJSON != nil {
		if err := json.Unmarshal([]byte(*t.Job().ParamsJSON), &params); err != nil {
			return jobs.Failed(codeStageFailed, "unreadable job parameters",
				s.failRow(id, "the job's parameters could not be read")), nil
		}
	}
	if params.Tag == "" {
		return jobs.Failed(codeStageFailed, "no release tag",
			s.failRow(id, "this update names no release tag")), nil
	}

	// --- step 2: `downloading`. Fetch the tarball, checksums.txt and
	// checksums.txt.sig into `update/`.
	if err := s.setState(ctx, t, id, model.UpdateDownloading); err != nil {
		return jobs.Outcome{}, err
	}
	asset, err := s.download(ctx, params.Tag)
	if err != nil {
		if ctx.Err() != nil {
			return jobs.Outcome{}, err
		}
		return jobs.Failed(codeDownloadFailed, err.Error(),
			s.failRow(id, "the release could not be downloaded: "+err.Error())), nil
	}

	// --- step 3: `verifying`. The tarball's sha256 must match checksums.txt, and
	// checksums.txt must verify against a compiled-in ed25519 key. A signature
	// failure aborts hard: this is the one place where a compromised download
	// would be catastrophic.
	if err := s.setState(ctx, t, id, model.UpdateVerifying); err != nil {
		return jobs.Outcome{}, err
	}
	digest, err := VerifyStaged(s.cfg.Layout.UpdateDir(), asset.tarball, s.keys)
	if err != nil {
		ok := false
		s.recordArtifacts(ctx, id, store.SelfUpdateStaged{SignatureOK: &ok})
		return jobs.Failed(codeVerifyFailed, err.Error(),
			s.failRow(id, "the release did not verify: "+err.Error())), nil
	}
	// The probe extracts from the same tarball, binding what it runs to the
	// digest just verified rather than to the path it sits at.
	if err := s.probeVersion(ctx, asset.tarball, params.Tag, digest); err != nil {
		return jobs.Failed(codeVerifyFailed, err.Error(),
			s.failRow(id, err.Error())), nil
	}

	verified := true
	if err := s.recordArtifacts(ctx, id, store.SelfUpdateStaged{
		AssetURL: &asset.url, AssetSHA256: &digest, SignatureOK: &verified,
	}); err != nil {
		return jobs.Outcome{}, err
	}

	// --- step 4: the D14 snapshot, labeled with the version being REPLACED.
	snapshot, err := Snapshot(ctx, s.cfg.Store, s.cfg.Layout, s.cfg.Version, s.now())
	if err != nil {
		return jobs.Failed(codeSnapshotFailed, err.Error(),
			s.failRow(id, "the pre-update database snapshot failed: "+err.Error())), nil
	}
	if err := s.recordArtifacts(ctx, id, store.SelfUpdateStaged{DBBackupPath: &snapshot}); err != nil {
		return jobs.Outcome{}, err
	}
	s.log.Info("took the pre-update database snapshot (D14)",
		"path", snapshot, "replacing_version", s.cfg.Version, "dir", s.snapshotDir())

	// A cancel is honored right up to the commit below and refused from it on, so
	// this is the last moment it can be observed.
	if t.CancelRequested() {
		return jobs.Canceled(func(ctx context.Context, tx store.Tx, _ model.JobState) error {
			message := "canceled before the update was staged"
			if _, err := s.cfg.Store.FinishSelfUpdate(ctx, tx, id,
				model.UpdateCanceled, &message, s.now().UnixMilli()); err != nil {
				return err
			}
			return s.cfg.Layout.ClearScratch()
		}), nil
	}

	// --- step 5: commit `verifying → staged`. THE CANCEL CUT-OFF (D96).
	if err := s.setState(ctx, t, id, model.UpdateStaged); err != nil {
		return jobs.Outcome{}, err
	}

	// --- step 6: write `update/pending` — a temp file, fsync, rename, fsync the
	// directory. The marker is therefore complete or absent on disk, never
	// half-written, and it is written by the DAEMON, which is not negotiable: the
	// oneshot's only trigger is ConditionPathExists= on this path.
	marker := Marker{
		Format:        MarkerFormat,
		SelfUpdateID:  id,
		FromVersion:   s.cfg.Version,
		TargetVersion: params.Tag,
		BinaryPath:    s.cfg.Layout.InstalledPath(),
		StagedAt:      s.now().UnixMilli(),
	}
	if err := WriteMarker(s.cfg.Layout.PendingPath(), marker); err != nil {
		return jobs.Failed(codeStageFailed, err.Error(),
			s.failRow(id, "the pending marker could not be written: "+err.Error())), nil
	}

	// --- step 7: commit `staged → swapping`, then hand section 9.4 steps 2-6,
	// D93's ResetFailedUnit and the summons to the composition root.
	if err := s.setState(ctx, t, id, model.UpdateSwapping); err != nil {
		return jobs.Outcome{}, err
	}
	if s.cfg.Swap == nil {
		return jobs.Failed(codeStageFailed, "no swap coordinator",
			s.failRow(id, "this daemon was built without a swap coordinator")), nil
	}
	if err := s.cfg.Swap.BeginSwap(ctx); err != nil {
		return jobs.Failed(codeStageFailed, err.Error(),
			s.failRow(id, "the swap could not be summoned: "+err.Error())), nil
	}

	// D79: on this path the daemon does not exit and this job does not finish.
	// It waits to be SIGTERMed by the oneshot's own `systemctl restart`, and the
	// queue's own shutdown branch then leaves this row `running` for the next
	// boot to triage into the `interrupted` section 12.3 row 4 names — where the
	// confirmation gate resolves it against the marker written above.
	<-ctx.Done()
	return jobs.Outcome{}, ctx.Err()
}

// setState commits one of the pipeline's transitions, with the job's progress
// beside it so the UI has something to show for a two-minute download.
func (s *Service) setState(ctx context.Context, t *jobs.Task, id string,
	state model.SelfUpdateState) error {

	if err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.cfg.Store.SetSelfUpdateState(ctx, tx, id, state)
		return err
	}); err != nil {
		return err
	}
	return t.SetProgress(ctx, map[string]any{"phase": string(state)})
}

// failRow is the domain half of a failed outcome: §2.3a puts the code on the job
// and the message on the domain row, and one transaction writes both.
func (s *Service) failRow(id, message string) jobs.CommitFunc {
	return func(ctx context.Context, tx store.Tx, state model.JobState) error {
		if state != model.JobFailed {
			// A retryable failure that is going back to `queued` must not close
			// the domain row: the activity has not ended.
			_, err := s.cfg.Store.SetSelfUpdateState(ctx, tx, id, model.UpdatePlanned)
			return err
		}
		m := message
		_, err := s.cfg.Store.FinishSelfUpdate(ctx, tx, id, model.UpdateFailed, &m,
			s.now().UnixMilli())
		return err
	}
}

func (s *Service) recordArtifacts(ctx context.Context, id string, a store.SelfUpdateStaged) error {
	return s.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.cfg.Store.SetSelfUpdateArtifacts(ctx, tx, id, a)
		return err
	})
}

// downloaded names what step 2 fetched.
type downloaded struct {
	tarball string
	url     string
}

// download fetches the three artifacts into `update/`.
//
// The GitHub token is never sent: these are `browser_download_url` assets on a
// CDN, not api.github.com, and section 6.2's cross-host rule is the same one
// here. Nothing about a public release asset needs a credential.
func (s *Service) download(ctx context.Context, tag string) (downloaded, error) {
	if s.cfg.Releases == nil {
		return downloaded{}, errors.New("this daemon was built without a release feed")
	}
	rel, _, err := s.cfg.Releases.ReleaseByTag(ctx, tag)
	if err != nil {
		return downloaded{}, err
	}

	tarball := TarballName(tag, s.goarch())
	want := []string{tarball, ChecksumsName, SignatureName}
	out := downloaded{tarball: tarball}

	if err := s.cfg.Layout.EnsureUpdateDir(); err != nil {
		return downloaded{}, err
	}
	for _, name := range want {
		asset, ok := rel.Asset(name)
		if !ok {
			return downloaded{}, fmt.Errorf("release %s has no asset named %s", tag, name)
		}
		dest := filepath.Join(s.cfg.Layout.UpdateDir(), name)
		if err := s.fetch(ctx, asset.DownloadURL, dest); err != nil {
			return downloaded{}, err
		}
		if name == tarball {
			out.url = asset.DownloadURL
		}
	}
	return out, nil
}

// fetch streams one asset to disk.
func (s *Service) fetch(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxBinaryBytes)); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", dest, err)
	}
	return f.Close()
}

// probeVersion is the second half of step 3: extract `llamaman` to
// `update/llamaman.new`, run `update/llamaman.new version`, and require it to
// print EXACTLY the requested tag. Then unlink the file.
//
// Equality rather than "at least" is deliberate: it catches a wrong-architecture
// or corrupt build before anything is swapped, AND it is what lets a deliberate
// downgrade through (section 12.4, D90).
//
// Nothing ever installs this file. The privileged actor extracts its own copy
// from the tarball it re-verified, so a file the service identity could rewrite
// after verification is never the file that lands on `<prefix>` (D89 (c)) — and
// the unlink here is what the D89 regression test asserts.
func (s *Service) probeVersion(ctx context.Context, tarball, tag, signedDigest string) error {
	staged := s.cfg.Layout.StagedBinaryPath()
	if _, err := ExtractBinary(filepath.Join(s.cfg.Layout.UpdateDir(), tarball),
		staged, 0o750, signedDigest); err != nil {
		return err
	}
	defer func() {
		if err := removeIfPresent(staged); err != nil {
			s.log.Warn("could not unlink the staged binary after its version probe", "error", err)
		}
	}()

	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(probeCtx, staged, "version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded binary could not report its version: %w", err)
	}
	printed := firstToken(string(out))
	if printed != tag {
		return fmt.Errorf(
			"the downloaded binary reports version %q, but %q was requested", printed, tag)
	}
	return nil
}

// versionProbeTimeout bounds `llamaman.new version`, which prints four fields
// and exits. A binary that hangs here is a binary that must not be installed.
const versionProbeTimeout = 30 * time.Second

// firstToken reads the version out of `llamaman version`'s first line, which is
// `<version> (commit …)`. Comparing the whole line would make the probe depend on
// the commit and the build date, which differ per release by design.
func firstToken(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
