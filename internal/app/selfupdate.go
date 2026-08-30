package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The self-update wiring (DESIGN section 12), and the two places section 11.1
// puts it in the boot sequence:
//
//	step 4  the D92 disarm, through MigrateOptions.BeforeFirst — BEFORE the
//	        first migration is attempted.
//	step 11 the confirmation gate, BEFORE READY=1.
//
// Those two orderings are section 19's fourth preservation property in code:
// "the revert is disarmed before the database moves past the binary it would
// restore", so `OnFailure=` can only ever fire for a daemon that never finished
// a boot. Any new code between the schema gate and that unlink, and any change
// that moves the gate back after READY=1, re-opens the one path in this design
// that ends with a host that has no daemon at all.

// buildSelfUpdate attaches the gate's remaining collaborators and constructs the
// service, registering the `self_update` worker.
//
// The gate itself was built at step 4, before the migrations, because the D92
// disarm runs from BeforeFirst and the object that performs it must be the same
// one step 11 resolves against — it carries the in-memory copy of the marker it
// unlinked. What is attached here is everything step 6 and the service
// construction produced in the meantime.
//
// The GATE exists even when nothing else about self-update does: resolving a
// marker a previous boot left behind is not conditional on this host being able
// to START an update. A host whose swap unit was deleted still has to be able to
// close out the update that was in flight when it happened.
func (d *daemon) buildSelfUpdate() error {
	layout := d.updateLayout()

	d.updateGate.Attach(selfupdate.Attachments{
		Units:   unitStates{control: d.systemd.Control},
		Journal: journalText{scope: d.scope},
		Events:  d.recorder,
		// The lease owner, so the gate can tell an update THIS boot is applying
		// from one a boot that is gone left behind. Both of the gate's live
		// callers — the ticker and `POST /update/apply` — run while this daemon
		// is up, and section 12.1 step 7 opens a window in which the marker is on
		// disk and no unit is active yet.
		BootID: d.bootID,
	})

	svc, err := selfupdate.New(selfupdate.Config{
		Store:    d.store,
		Jobs:     d.queue,
		Gate:     d.updateGate,
		Layout:   layout,
		Scope:    d.scope,
		Version:  buildinfo.Version,
		Releases: d.updateReleases(),
		Units:    selfupdate.DiskUnits{Scope: d.scope},
		Swap:     d,
		Events:   d.recorder,
		Now:      d.opts.Now,
		Log:      d.log,
	})
	if err != nil {
		return err
	}
	d.selfupdate = svc
	return d.queue.Register(svc)
}

// updateLayout is `<state_dir>/update` plus the `<prefix>` this binary is
// installed under. A prefix that cannot be resolved is not fatal: the gate needs
// only the state directory, and the two guard clauses that need `<prefix>` fail
// closed on their own.
func (d *daemon) updateLayout() selfupdate.Layout {
	l := selfupdate.Layout{StateDir: d.stateDir}
	if prefix, err := selfupdate.ResolvePrefix(); err == nil {
		l.Prefix = prefix
	} else {
		d.log.Warn("could not resolve this binary's installation directory",
			"error", err, "consequence", "self-update will refuse to stage")
	}
	return l
}

// updateReleases points the section 6.2 GitHub client at THIS project's
// repository. Same client type, same request policy, same conditional requests,
// same cross-host header strip — writing a second GitHub client would be a
// second place for the "never send the token anywhere but api.github.com" rule
// to be got wrong.
//
// It is a separate INSTANCE rather than `d.releases` because that one is bound
// to `ggml-org/llama.cpp`: the repository is a field of the client, and its ETag
// cache is keyed per request path.
func (d *daemon) updateReleases() selfupdate.ReleaseSource {
	var githubToken func(context.Context) (string, error)
	if d.secrets != nil {
		githubToken = d.secrets.TokenFunc(model.SecretGitHubToken)
	}
	return github.New(github.Options{
		Repo:      selfupdate.Repo,
		UserAgent: UserAgent(),
		Token:     githubToken,
		Now:       d.opts.Now,
		Logger:    d.log,
	})
}

// disarmRevert is D92's hook, called by the migration runner after the schema
// gate and the checksum verification have passed and BEFORE the first migration
// is applied — but only when there is at least one migration to apply.
//
// The gate has to exist by then, and it does: it is built in boot() before
// Migrate is called, because it needs nothing but the store and the state
// directory. Everything else about self-update — the service, the worker, the
// release client — is built afterwards, with the rest of the subsystems.
func (d *daemon) disarmRevert(pending []store.Migration) error {
	if d.updateGate == nil {
		// Unreachable in the composition root, and a refusal rather than a
		// silent skip if a future refactor makes it reachable: migrating with the
		// revert still armed is the one ordering this design cannot tolerate.
		return errors.New("app: about to migrate with no self-update gate to disarm the revert (D92)")
	}
	d.log.Info("about to apply migrations; disarming the self-update revert first (D92)",
		"pending", len(pending))
	return d.updateGate.DisarmBeforeMigration()
}

// resolveUpdateMarkers is section 11.1 step 11, run from serve() BEFORE READY=1.
//
// It runs there deliberately. Everything it needs is already resolved — the
// database (step 4), the systemd controller and `journal_read` (step 6) — and
// putting it here buys one property worth stating on its own: **a daemon that
// ever signals readiness has already resolved the marker**, so the judge cannot
// be armed against a version that demonstrably booted. Together with step 4's
// disarm that leaves exactly one shape of unconfirmed update — a binary that
// never finished a boot — which is what section 12.2 claims the judge is for.
//
// The daemon sends EXTEND_TIMEOUT_USEC= while it runs, for the same reason it
// does during a migration: branch 3 reads a journal tail, and a slow journal must
// extend TimeoutStartSec= rather than trip it.
func (d *daemon) resolveUpdateMarkers(ctx context.Context) {
	if d.updateGate == nil {
		return
	}
	stop := d.extendTimeoutWhile()
	defer stop()

	res, err := d.updateGate.Resolve(ctx)
	if err != nil {
		// A gate failure must not stop the boot. The marker survives, the ticker
		// runs every 30 s while it does, and the next call resolves it — whereas
		// refusing to start would turn a resolvable state into a host with no
		// daemon, which is the outcome the whole of section 12 exists to avoid.
		d.log.Error("could not resolve the self-update marker at boot", "error", err)
		return
	}
	if res.Branch != selfupdate.BranchNone || res.ClosedOrphans > 0 {
		d.log.Info("resolved the self-update state at boot",
			"branch", string(res.Branch), "closed_orphans", res.ClosedOrphans,
			"row_resolved", res.RowResolved)
	}
}

// extendTimeoutWhile keeps sending EXTEND_TIMEOUT_USEC= until the returned func
// is called, which is what section 11.1 step 11 requires of the gate and step 4
// already does for the migrations.
func (d *daemon) extendTimeoutWhile() func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(extendTimeoutEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := d.opts.Notifier.ExtendTimeout(2 * extendTimeoutEvery); err != nil {
					d.log.Debug("could not extend the start timeout", "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// extendTimeoutEvery is section 11.1 step 4's 10 s heartbeat interval, reused by
// the gate for the same reason.
const extendTimeoutEvery = 10 * time.Second

// BeginSwap implements selfupdate.SwapCoordinator: section 12.1 step 7's second
// half.
//
// It hands the work to the serve loop rather than doing it here, because the
// serve loop is what owns the listeners section 9.4 steps 3-6 pause, drain and
// hand to the fd store — and because D79's "the daemon does not exit, it waits
// to be SIGTERMed" is a property of that loop and of nothing else.
//
// The signal is non-blocking and idempotent: a second BeginSwap on a daemon that
// is already swapping is a no-op rather than a second drain.
func (d *daemon) BeginSwap(ctx context.Context) error {
	select {
	case d.swap <- struct{}{}:
		return nil
	default:
		// Already signaled. The buffered slot is the whole state machine.
		return nil
	}
}

// runSwap is section 12.1 step 7 in the serve loop: section 9.4 steps 2-6, then
// D93's ResetFailedUnit, then StartNoWait on the swap actor from a detached
// goroutine, and then D79's wait.
//
// **The daemon does not exit here.** `llamaman.service` is Restart=always with
// RestartSec=2, so a daemon that exited voluntarily two milliseconds after
// summoning the oneshot would be restarted by systemd — as the OLD binary —
// while `selfupdate-apply` was still re-verifying the tarball. Waiting removes
// that race entirely: the oneshot's own `systemctl restart --no-block` is what
// ends this process, at the moment the binary on disk is the one that should
// come back.
//
// The 120 s failsafe exists because a blocked wait with no bound is a hang. If
// no signal arrives the daemon logs at error, **deletes nothing**, and exits,
// letting Restart=always bring a binary back for an ordinary boot — the new one
// if the swap has already happened, the old one if it has not. Either way that
// boot is inert: the gate confirms if the swap took, defers while the oneshot is
// still active, and closes the update out once it is not.
// **In the D2 user-scope topology there is no oneshot and no wait**, and that
// second branch is section 12.1 step 7's own: "the daemon performs section
// 12.2's swap sequence itself and then exits normally" (section 5.2a item 2).
// `install-units` writes no `llamaman-selfupdate.service` in user scope and
// `selfupdate-apply` refuses to run there, so summoning a unit would summon
// nothing — which is what left a `--user-units` host staging updates it could
// never apply, burning the failsafe with no management UI and raising F24.
// Exiting rather than waiting is correct there for the reason D79 gives: by the
// time this process exits the binary on disk is already the new one, so
// Restart=always starting it is the intended outcome rather than a race.
func (d *daemon) runSwap(ctx context.Context, errc <-chan error) error {
	// The gate's 30 s ticker is stopped BEFORE any of this, by the caller, for
	// the reason section 12.3 states: it is "stopped by the section 9.4 shutdown
	// along with the other background workers". A ticker still running here
	// would see `update/pending` naming a version this binary is not, with no
	// actor yet active, and take branch 3 against the swap this very boot is
	// performing — unlinking the marker that is the oneshot's only trigger and
	// the judge's second condition.

	// Steps 3-6: the same drain and fd-store hand-off every other caller
	// performs. Step 1 (the domain commit) already happened — it is the
	// `staged → swapping` commit the worker made before calling BeginSwap — and
	// step 2's `202` was flushed by the endpoint long ago.
	err := d.shutdown(errc)

	// D93, and it is topology-independent (section 5.2a): the swap and the
	// revert deadline that follows it begin with the full StartLimitBurst=
	// budget rather than whatever this boot has already spent. In user scope the
	// same call goes to the user's own manager and needs no polkit at all.
	if d.systemd.Control != nil {
		if resetErr := d.systemd.Control.ResetFailed(context.WithoutCancel(ctx),
			systemd.UnitDaemon); resetErr != nil {
			d.log.Warn("could not clear the start-limit counter before the swap",
				"unit", systemd.UnitDaemon, "error", resetErr)
		}
	}

	if d.scope == model.ScopeUser {
		d.swapInProcess(ctx, d.updateLayout())
		return err
	}

	// The summons, from a DETACHED goroutine and without waiting: a Type=oneshot
	// start job does not complete until its ExecStart exits, and this one's
	// ExecStart ends by restarting the very process that would be waiting on the
	// job (section 5.3).
	if d.systemd.Control != nil {
		go func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swapSummonTimeout)
			defer cancel()
			if _, startErr := d.systemd.Control.StartNoWait(sctx, selfupdate.SwapUnit); startErr != nil {
				d.log.Error("could not summon the self-update swap actor",
					"unit", selfupdate.SwapUnit, "error", startErr)
			}
		}()
	} else {
		d.log.Error("no service manager to summon the self-update swap actor",
			"unit", selfupdate.SwapUnit)
	}

	d.log.Info("staged self-update handed to the service manager; waiting to be restarted (D79)",
		"unit", selfupdate.SwapUnit, "failsafe_sec", int(SwapWaitFailsafe.Seconds()))

	// D79's wait, with section 9.4 step 7's 120 s failsafe.
	select {
	case <-ctx.Done():
		d.log.Info("the swap actor restarted this unit; exiting for the new binary")
	case <-time.After(SwapWaitFailsafe):
		d.log.Error("no restart arrived within the self-update failsafe; exiting so the unit comes back",
			"failsafe_sec", int(SwapWaitFailsafe.Seconds()),
			"deleted", "nothing")
	}
	return err
}

// swapInProcess is section 12.1 step 7's second paragraph and section 5.2a item
// 2: the D2 user-scope swap, performed by the daemon itself.
//
// It is section 12.2's sequence with the privilege boundary removed, and it is
// literally the same code — selfupdate.Apply, the function the root oneshot
// calls — because a second implementation of a retain-and-rename is how the two
// topologies come to disagree about what a swap is. Three things differ, and all
// three are section 5.2a's:
//
//   - The signature re-verification that exists in system scope only to distrust
//     the stager is performed anyway, once, by the one process doing both jobs.
//   - No `systemctl restart` is issued (Restart is nil): this process exits, and
//     Restart=always starts the binary that is now on disk. Issuing a restart of
//     the unit this process IS would be asking systemd to stop it, and no step in
//     this protocol stops a unit (section 19's first preservation property).
//   - Nothing waits. D79's wait exists to keep Restart=always from bringing the
//     OLD binary back while a separate actor is still verifying; here there is no
//     separate actor and the new binary is already installed when this returns.
//
// A failure changes nothing on disk that matters — every step is a check or one
// rename — and the exit is the same one section 12.3 row 5 describes: the daemon
// comes back on the old binary and its gate takes branch 3, raising F24 with this
// line in the journal.
func (d *daemon) swapInProcess(ctx context.Context, l selfupdate.Layout) {
	res, err := selfupdate.Apply(context.WithoutCancel(ctx), selfupdate.ApplyOptions{
		Scope:  d.scope,
		Layout: l,
		Log:    d.log,
		// Restart is deliberately nil: exiting is the restart here (D79).
	})
	if err != nil {
		d.log.Error("the in-process self-update swap refused; exiting on the installed binary",
			"error", err, "scope", string(d.scope), "touched", "nothing")
		return
	}
	d.log.Info("in-process self-update swap complete; exiting so the new binary starts",
		"from_version", res.FromVersion, "target_version", res.TargetVersion,
		"retained_sha256", res.RetainedSHA256, "installed_sha256", res.InstalledSHA256)
}

// SwapWaitFailsafe is section 9.4 step 7's 120 s bound on D79's wait.
const SwapWaitFailsafe = 120 * time.Second

// swapSummonTimeout bounds the StartNoWait call itself — not the oneshot, whose
// own TimeoutStartSec=120 bounds that (D91). This is only how long the daemon
// waits for the bus to accept the job.
const swapSummonTimeout = 30 * time.Second

// unitStates satisfies selfupdate.UnitStater: the ONE question branch 2 of the
// confirmation gate turns on, and the same fact `GET /update/status` renders as
// `pending.actor_active`.
//
// A nil controller answers "not active", which is correct on a host with no
// service manager at all (F10): there is no oneshot there to be working.
type unitStates struct{ control systemd.Controller }

func (u unitStates) ActiveState(ctx context.Context, unit string) (string, error) {
	if u.control == nil {
		return "inactive", nil
	}
	props, err := u.control.Props(ctx, unit)
	if err != nil {
		return "", err
	}
	return props.ActiveState, nil
}

// journalText satisfies selfupdate.JournalTailer over the same reader
// journalTail uses, joining the lines into the block the F24 card carries.
type journalText struct{ scope model.SystemdScope }

func (j journalText) Tail(ctx context.Context, unit string, lines int) (string, error) {
	out, err := journalTail{scope: j.scope}.Tail(ctx, unit, lines)
	if err != nil {
		return "", err
	}
	return strings.Join(out, "\n"), nil
}

// updateJobKinds is the one assertion this file makes about the queue: the
// self-update worker is registered under the kind section 2.3a pairs with
// `self_updates.id`, and nothing else claims it.
var _ jobs.Worker = (*selfupdate.Service)(nil)

var (
	_ jobs.DomainWriter = (*selfupdate.Service)(nil)
	_ jobs.CancelGuard  = (*selfupdate.Service)(nil)
)

// ErrNoSelfUpdate is what the API answers with when this daemon was built
// without the subsystem. It is a 503 rather than a missing route: a documented
// endpoint whose subsystem is nil reports the gap, it never fakes an answer.
var ErrNoSelfUpdate = fmt.Errorf("app: this daemon was built without the self-update subsystem")
