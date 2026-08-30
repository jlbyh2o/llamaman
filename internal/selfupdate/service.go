package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The daemon's half (DESIGN section 12.1): resolve, verify, snapshot, stage.
//
// Seven steps, three commits and one file write, ordered so that a kill at any
// point lands in a state exactly one rule in section 12.3 resolves. The
// stop-point table there walks every one of them, and the fixtures in
// selfupdate_stoppoints_test.go drive every row.

// SwapCoordinator is section 12.1 step 7's second half, and it belongs to the
// composition root rather than to this package: it is section 9.4 steps 2-6 (the
// drain, the WAL checkpoint, the fd-store hand-off), then D93's
// ResetFailedUnit(llamaman.service), then StartNoWait on the swap actor from a
// detached goroutine, and then D79's WAIT — because on this path the daemon does
// not exit. It stops serving and waits to be SIGTERMed by the oneshot's own
// `systemctl restart`, since Restart=always/RestartSec=2 would otherwise bring
// the OLD binary straight back while `selfupdate-apply` was still verifying.
//
// The worker calls BeginSwap and then blocks on its context: the job row stays
// `running` under a boot that is about to end, and the next boot's triage turns
// it into the `interrupted` section 12.3 row 4 names.
type SwapCoordinator interface {
	// BeginSwap hands section 9.4's shutdown to the serve loop and returns
	// immediately. In the D2 user-scope topology it performs the swap in process
	// instead and exits normally (section 5.2a item 2).
	BeginSwap(ctx context.Context) error
}

// UnitFacts answers the two guard clauses that are facts about the INSTALLED
// units' own directives, never about a template hash (D95, section 5.4a): a host
// that self-updated across a release which changed a unit template is
// `drift: stale` and must still be able to update.
type UnitFacts interface {
	// Unit reports whether a unit file is installed, whether it is masked, and
	// its content.
	Unit(name string) (UnitFile, error)
}

// UnitFile is one installed unit.
type UnitFile struct {
	Present bool
	// Masked is a unit symlinked to /dev/null. systemd will not start it, so for
	// both self-update clauses it is exactly as disqualifying as absent.
	Masked  bool
	Content string
}

// Config wires the service.
type Config struct {
	// Store is the database.
	Store *store.Store
	// Jobs is the queue. The apply endpoint inserts the `self_updates` row and
	// its job through EnqueueTx inside ONE BEGIN IMMEDIATE transaction with all
	// four guard clauses (D97).
	Jobs *jobs.Queue
	// Gate is the confirmation gate. `POST /update/apply` runs it FIRST, ahead of
	// its guard, which is what lets the next Update click start from a clean
	// directory instead of being refused by debris.
	Gate *Gate
	// Layout is `<state_dir>` and `<prefix>`.
	Layout Layout
	// Scope selects the topology: in user scope there is no swap actor to summon
	// and `409 selfupdate_unsupported` is inapplicable.
	Scope model.SystemdScope
	// Version is this binary's own version.
	Version string

	// Releases is the release feed. Nil answers the release endpoints with a
	// stated failure rather than an empty list.
	Releases ReleaseSource
	// Units answers the two unit-directive clauses. Nil makes both refuse, which
	// is the honest answer for a daemon that cannot read its own units: an update
	// is never staged without a working revert.
	Units UnitFacts
	// Swap is step 7's hand-off. Nil makes the worker fail the update at step 7
	// rather than stage one nothing can apply.
	Swap SwapCoordinator
	// HTTP fetches the three release artifacts. Nil builds one with a long
	// timeout: a tarball on a slow link is an ordinary case, not a hang.
	HTTP *http.Client
	// Keys are the compiled-in release keys. Nil loads the embedded set.
	Keys KeySet
	// Events appends the `events` rows this pipeline emits.
	Events EventAppender
	// Now, Log and GOARCH are the usual seams.
	Now    func() time.Time
	Log    *slog.Logger
	GOARCH string
}

// Service is the daemon's half of section 12.
type Service struct {
	cfg  Config
	log  *slog.Logger
	http *http.Client
	keys KeySet
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("selfupdate: a store is required")
	}
	if cfg.Gate == nil {
		return nil, errors.New("selfupdate: a confirmation gate is required")
	}
	s := &Service{cfg: cfg, log: cfg.Log, http: cfg.HTTP, keys: cfg.Keys}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.http == nil {
		s.http = &http.Client{Timeout: 30 * time.Minute}
	}
	if s.keys == nil {
		keys, err := EmbeddedKeys()
		if err != nil {
			// A binary with no usable release key cannot verify an update — but it
			// must still BOOT, serve, and above all run the confirmation gate:
			// resolving a marker a previous boot left behind is not conditional on
			// this host being able to start a new update, and refusing to
			// construct here would turn a release-engineering gap into a host with
			// no daemon.
			//
			// The empty key set fails CLOSED: every signature check answers
			// ErrSignature, so `POST /update/apply` stages nothing rather than
			// installing something unverified.
			s.log.Error("no usable release signing key is compiled into this binary; "+
				"self-update will refuse every download at the signature step",
				"error", err)
			s.keys = KeySet{}
		} else {
			s.keys = keys
		}
	}
	return s, nil
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Service) goarch() string {
	if s.cfg.GOARCH != "" {
		return s.cfg.GOARCH
	}
	return runtime.GOARCH
}

// The four codes section 12.1 step 1, section 3.14 and section 15's fixture
// enumerate identically, so the three can never drift.
const (
	// CodeSelfUpdateUnavailable is `systemd_control='unavailable'` (§11.1a): the
	// swap needs a service manager, and this is the row §11.1a already reports as
	// `self_update: no`.
	CodeSelfUpdateUnavailable model.ErrorCode = "selfupdate_unavailable"
	// CodeSelfUpdateUnsupported is "the swap actor cannot be summoned": in system
	// scope, `llamaman-selfupdate.service` is absent or masked. The predicate is a
	// fact about the INSTALLED UNIT, not about this binary — "this binary has no
	// selfupdate-apply subcommand" was the earlier wording and it is a
	// compile-time constant evaluated by the very binary that would have to
	// contain the endpoint to evaluate it.
	CodeSelfUpdateUnsupported model.ErrorCode = "selfupdate_unsupported"
	// CodeRevertUnavailable is llamaman.service carrying no
	// OnFailure=llamaman-update-verify.service, or the judge unit being absent or
	// masked. An update is NEVER staged without a working revert, and this is the
	// one place that is decided — by the process that can still talk to a human,
	// before anything on disk has moved (D88).
	CodeRevertUnavailable model.ErrorCode = "revert_unavailable"
)

// ApplyRequest is the body of `POST /api/v1/update/apply`.
type ApplyRequest struct {
	// Tag is any tag `/update/releases` lists, NEWER OR OLDER: a downgrade is
	// this same pipeline, and step 3's probe requires the extracted binary to
	// print exactly this tag (D90).
	Tag string
	// Idempotency is D65's replay window, filled by the middleware.
	Idempotency *jobs.Idempotency
}

// StageResult is what the endpoint answers with.
type StageResult struct {
	SelfUpdateID string
	Job          model.Job
	Replayed     bool
	// SchemaWarning is set for a target older than the running version: the
	// response and the dialog carry section 12.4's warning and its five-command
	// procedure (D94).
	SchemaWarning bool
	// Procedure is those five commands, verbatim, so nothing downstream has to
	// reconstruct them — and so nothing can print `restore-db` alone or omit the
	// `reset-failed` step.
	Procedure []string
}

// Apply is `POST /api/v1/update/apply`.
//
// It runs the section 12.3 resolver FIRST — so debris from a previous update
// cannot refuse this one — and then a guard of exactly four clauses, all of them
// evaluated inside the ONE BEGIN IMMEDIATE transaction that inserts the row and
// its job (D97). `idx_jobs_one_live_per_subject` cannot enforce this on its own:
// `jobs.subject_id` for a self-update is a fresh `self_updates.id`, so two
// concurrent applies have different subjects and the index is silent — exactly
// the hole D70 closed for builds and D75 for benches. Here there is one producer
// in one process (flock, F11), so SQLite's writer serialization is the whole
// mechanism, PROVIDED the read and the write sit inside the same immediate
// transaction rather than either side of it.
func (s *Service) Apply(ctx context.Context, req ApplyRequest) (StageResult, error) {
	if req.Tag == "" {
		return StageResult{}, model.Error{Code: model.CodeSettingInvalid,
			Message: "a release tag is required"}
	}

	// The resolver, first. Its own closing pass is what stops a `self_update` job
	// a restart orphaned from refusing this apply at `409 job_in_flight` with no
	// marker for any caller to find.
	if _, err := s.cfg.Gate.Resolve(ctx); err != nil {
		return StageResult{}, err
	}

	now := s.now()
	id := store.NewID(now)
	binaryPath := ""
	if s.cfg.Layout.Prefix != "" {
		binaryPath = s.cfg.Layout.InstalledPath()
	}

	var res StageResult
	err := s.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.guard(ctx, tx); err != nil {
			return err
		}

		row := store.SelfUpdate{
			ID:          id,
			FromVersion: s.cfg.Version,
			ToVersion:   req.Tag,
			Channel:     "stable",
			State:       model.UpdatePlanned,
			CreatedAt:   now.UnixMilli(),
		}
		if binaryPath != "" {
			row.BinaryPath = &binaryPath
		}

		out, err := s.cfg.Jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{
			Kind:        model.JobSelfUpdate,
			DomainID:    id,
			Params:      workerParams{Tag: req.Tag},
			Idempotency: req.Idempotency,
			Domain: func(ctx context.Context, tx store.Tx, _ model.Job) error {
				return s.cfg.Store.InsertSelfUpdate(ctx, tx, row)
			},
		})
		if err != nil {
			return err
		}
		res.Job, res.Replayed = out.Job, out.Replayed
		res.SelfUpdateID = out.Job.SubjectID
		return nil
	})
	if err != nil {
		return StageResult{}, err
	}

	if !res.Replayed {
		// `update/` is emptied only after that transaction commits — of everything
		// except a live `pending`, which the guard has just proved absent — so
		// every stage begins from a clean directory, no scratch file can outlive
		// the update that created it, and a second apply can never delete the
		// tarball the first one is still downloading.
		if err := s.cfg.Layout.EnsureUpdateDir(); err != nil {
			return StageResult{}, err
		}
		if err := s.cfg.Layout.ClearScratch(); err != nil {
			return StageResult{}, err
		}
		s.cfg.Jobs.Wake()
	}

	if CompareVersions(req.Tag, s.cfg.Version) < 0 {
		res.SchemaWarning = true
		res.Procedure = DowngradeProcedure(s.cfg.Layout, req.Tag, "<newest db-backups/ snapshot>")
	}
	return res, nil
}

// guard is the four clauses, in the order section 12.1 step 1 and section 3.14
// both enumerate them.
func (s *Service) guard(ctx context.Context, tx store.Tx) error {
	// (1) `409 job_in_flight` while a build is running (D4) or any `self_update`
	// job is live — `interrupted` counts (§2.3).
	builds, err := s.cfg.Store.CountLiveJobsByKind(ctx, tx, model.JobLlamacppInstall)
	if err != nil {
		return err
	}
	if builds > 0 {
		return model.Error{Code: model.CodeJobInFlight,
			Message: "a llama.cpp build is running; a self-update would restart the daemon underneath it"}
	}
	updates, err := s.cfg.Store.CountLiveSelfUpdates(ctx, tx)
	if err != nil {
		return err
	}
	if updates > 0 {
		return model.Error{Code: model.CodeJobInFlight,
			Message: "a self-update is already in flight"}
	}

	// (2) `409 selfupdate_unavailable` when systemd_control='unavailable'.
	ri, err := s.cfg.Store.RuntimeInfo(ctx, tx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if ri.SystemdControl != nil && *ri.SystemdControl == model.ControlUnavailable {
		return model.Error{
			Code: CodeSelfUpdateUnavailable,
			Message: "this host has no service manager, so the binary swap cannot be performed; " +
				"update with the install.sh one-liner instead",
			Details: map[string]any{
				"command": "curl -fsSL https://raw.githubusercontent.com/" + Repo +
					"/main/installer/install.sh | sudo sh",
			},
		}
	}

	// (3) `409 selfupdate_unsupported` — the swap actor cannot be summoned.
	// Inapplicable in user scope and never fires there: there is no oneshot,
	// because the daemon performs the swap in process (§5.2a item 2).
	if s.cfg.Scope != model.ScopeUser {
		if err := s.requireUnit(SwapUnit, CodeSelfUpdateUnsupported,
			"the privileged swap actor is not installed on this host"); err != nil {
			return err
		}
	}

	// (4) `409 revert_unavailable` — no working revert. An update is never staged
	// without one, and this is the one place that is decided.
	if err := s.requireUnit(JudgeUnit, CodeRevertUnavailable,
		"the self-update revert unit is not installed on this host"); err != nil {
		return err
	}
	daemon, err := s.unit(DaemonUnit)
	if err != nil {
		return err
	}
	if !daemon.Present || !systemd.HasDirective(daemon.Content, "OnFailure", JudgeUnit) {
		return model.Error{
			Code: CodeRevertUnavailable,
			Message: DaemonUnit + " does not carry OnFailure=" + JudgeUnit +
				", so an update staged now would have no automatic revert",
			Details: map[string]any{"command": installUnitsCommand},
		}
	}
	return nil
}

// installUnitsCommand is the repair line both unit clauses print. `<user>` is
// left literal because the service identity is an installer-time decision this
// process must not guess at (D57).
const installUnitsCommand = "sudo llamaman install-units --identity <user>"

func (s *Service) requireUnit(name string, code model.ErrorCode, message string) error {
	u, err := s.unit(name)
	if err != nil {
		return err
	}
	switch {
	case !u.Present:
		return model.Error{Code: code, Message: message + " (" + name + " is absent)",
			Details: map[string]any{"unit": name, "command": installUnitsCommand}}
	case u.Masked:
		return model.Error{Code: code, Message: message + " (" + name + " is masked)",
			Details: map[string]any{"unit": name,
				"command": "sudo systemctl unmask " + name}}
	}
	return nil
}

func (s *Service) unit(name string) (UnitFile, error) {
	if s.cfg.Units == nil {
		// A daemon that cannot read its own units cannot prove a working revert,
		// and section 12.1 is explicit that an update is never staged without one.
		return UnitFile{}, model.Error{Code: CodeRevertUnavailable,
			Message: "this daemon cannot read its own unit files, so it cannot confirm " +
				"that the automatic revert is installed",
			Details: map[string]any{"command": installUnitsCommand}}
	}
	return s.cfg.Units.Unit(name)
}

// DowngradeProcedure is section 12.4's five commands, verbatim and in order.
//
// It is a function rather than five strings at four call sites because section
// 12.4 ends with the sentence this exists to enforce: "**Nothing prints the
// `restore-db` line alone, and nothing prints the procedure without step 4.**"
// The `reset-failed` step is what an earlier four-command reading omitted, and
// without it the final `start` is refused with "start request repeated too
// quickly" for the remainder of a 600 s window a fast-panicking binary leaves
// nearly whole.
func DowngradeProcedure(l Layout, tag, snapshot string) []string {
	return []string{
		"sudo systemctl stop " + DaemonUnit,
		"curl -fsSL https://raw.githubusercontent.com/" + Repo + "/main/installer/install.sh | " +
			"sudo sh -s -- --version " + tag + " --no-start",
		"sudo llamaman restore-db " + snapshot,
		"sudo systemctl reset-failed " + DaemonUnit,
		"sudo systemctl start " + DaemonUnit,
	}
}

// Status is `GET /api/v1/update/status`.
type Status struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	// LastCheckedAt is when the release cache was last refreshed, in Unix
	// milliseconds.
	LastCheckedAt *int64
	// InFlight is the non-terminal `self_updates` row, if there is one.
	InFlight *store.SelfUpdate
	// Pending is the one self-update fact the gate last computed: nil on a
	// settled host, otherwise the marker plus `actor_active` — which is
	// `systemctl is-active llamaman-selfupdate.service`, the same fact the gate
	// itself defers on (D91), so the UI renders "a swap is in flight" and the F24
	// card from the fact the daemon acted on.
	Pending *PendingStatus
}

// PendingStatus is `status.pending`.
type PendingStatus struct {
	SelfUpdateID  string
	FromVersion   string
	TargetVersion string
	StagedAt      int64
	ActorActive   bool
}

// Status answers the endpoint.
func (s *Service) Status(ctx context.Context) (Status, error) {
	out := Status{CurrentVersion: s.cfg.Version}

	releases, err := s.Releases(ctx)
	if err != nil {
		return Status{}, err
	}
	for _, r := range releases {
		if out.LatestVersion == "" || CompareVersions(r.Tag, out.LatestVersion) > 0 {
			out.LatestVersion = r.Tag
		}
		// The last CHECK is when the cache was last confirmed fresh, not when the
		// newest release was published: "as of 14:02, GitHub unreachable" is a
		// statement about this host's knowledge.
		if out.LastCheckedAt == nil || r.FetchedAt > *out.LastCheckedAt {
			at := r.FetchedAt
			out.LastCheckedAt = &at
		}
	}
	out.UpdateAvailable = out.LatestVersion != "" &&
		CompareVersions(out.LatestVersion, s.cfg.Version) > 0

	err = s.cfg.Store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.cfg.Store.LiveSelfUpdate(ctx, tx)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		out.InFlight = &row
		return nil
	})
	if err != nil {
		return Status{}, err
	}

	if m, present, active := s.cfg.Gate.Pending(ctx); present {
		out.Pending = &PendingStatus{
			SelfUpdateID:  m.SelfUpdateID,
			FromVersion:   m.FromVersion,
			TargetVersion: m.TargetVersion,
			StagedAt:      m.StagedAt,
			ActorActive:   active,
		}
	}
	return out, nil
}

// Check is `POST /api/v1/update/check`: refresh the release cache.
func (s *Service) Check(ctx context.Context) error {
	_, err := s.RefreshReleases(ctx)
	return err
}

// CheckCancel is D96's cut-off, implementing jobs.CancelGuard.
//
// A cancel is honored while the row is `planned`/`downloading`/`verifying`: row
// and job move to `canceled` in one transaction and `update/` scratch is
// cleared. At or after the `staged` commit it answers
// `409 selfupdate_not_cancelable`, because the next step writes a marker to disk
// and the step after that hands the work to systemd — and nothing downstream
// reads `cancel_requested`.
func (s *Service) CheckCancel(ctx context.Context, tx store.Tx, j model.Job) error {
	row, err := s.cfg.Store.SelfUpdate(ctx, tx, j.SubjectID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.State.Cancelable() {
		return nil
	}
	return model.Error{
		Code: model.CodeSelfUpdateNotCancelable,
		Message: fmt.Sprintf(
			"this update is %s: the marker is on disk and the swap belongs to the service manager",
			row.State),
	}
}

// SetDomainState implements jobs.DomainWriter for the transitions the QUEUE
// performs with no worker running: boot triage, a cancel of a job nobody holds,
// and a retry.
//
// For `interrupted` it is a NO-OP, and that is the whole point of section 2.3's
// second bucket: the `self_updates` row keeps whichever non-terminal state it
// held, because that state is precisely the confirmation gate's input and
// overwriting it destroys the recovery that follows.
func (s *Service) SetDomainState(ctx context.Context, tx store.Tx, j model.Job,
	state model.JobState) error {

	switch state {
	case model.JobInterrupted:
		return nil
	case model.JobCanceled:
		message := "canceled before the update was staged"
		if _, err := s.cfg.Store.FinishSelfUpdate(ctx, tx, j.SubjectID,
			model.UpdateCanceled, &message, s.now().UnixMilli()); err != nil {
			return err
		}
		// The scratch goes with it: a canceled update must leave no tarball for
		// the next one to inherit, and it leaves NO MARKER, because a cancel is
		// only ever accepted before the marker is written.
		return s.cfg.Layout.ClearScratch()
	case model.JobQueued:
		// A retry re-runs from `planned`. The pipeline is idempotent from there:
		// step 1 already emptied `update/`, and the worker re-downloads.
		_, err := s.cfg.Store.SetSelfUpdateState(ctx, tx, j.SubjectID, model.UpdatePlanned)
		return err
	case model.JobFailed:
		message := "the daemon could not run this update"
		_, err := s.cfg.Store.FinishSelfUpdate(ctx, tx, j.SubjectID,
			model.UpdateFailed, &message, s.now().UnixMilli())
		return err
	default:
		return nil
	}
}

// Kind implements jobs.Worker.
func (s *Service) Kind() model.JobKind { return model.JobSelfUpdate }

// workerParams is `jobs.params_json` for a self-update: the tag, so a worker
// that comes back after a restart knows what it was asked for.
type workerParams struct {
	Tag string `json:"tag"`
}

// removeIfPresent unlinks a path and treats "already gone" as success.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("selfupdate: remove %s: %w", path, err)
	}
	return nil
}

// snapshotDir is the directory the D14 snapshot lands in, exposed for the log
// line the worker writes.
func (s *Service) snapshotDir() string { return filepath.Join(s.cfg.Layout.StateDir, DBBackupsDirName) }
