package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The confirmation gate (DESIGN section 12.3), and D92's disarm that precedes
// it (section 11.1 step 4).
//
// ONE routine — ResolveUpdateMarkers — with three callers: the boot gate
// (section 11.1 step 11, which runs BEFORE READY=1), a 30 s ticker that runs
// only while `update/pending` exists, and `POST /update/apply` immediately
// before its guard. `llamaman doctor` reports the same facts read-only and
// writes nothing. The routine is idempotent and does all of its writing in one
// transaction, so two callers racing produce one resolution and one
// notification.
//
// Boot alone was never enough — after a refusal, section 9.4 step 7's 120 s
// failsafe returns THIS daemon to service and the next boot may be weeks away —
// and the endpoint caller is what lets the next Update click start from a clean
// directory instead of being refused by debris.
//
// # There is one marker and one question about it
//
// `update/pending` names a `target_version`; this binary knows its own; and
// `llamaman-selfupdate.service` either is or is not active, which systemd
// answers authoritatively and bounds with that unit's own TimeoutStartSec=120
// (D91). Those three facts decide the branch. **No decision here is measured
// from a clock** — not from `staged_at`, not from a deadline, not from anything
// this design would have had to freeze across two binaries.

// Branch names one of section 12.3's three outcomes, plus the two shapes that
// are not branches at all.
type Branch string

const (
	// BranchNone is "there was no marker": the routine went straight to the
	// closing pass.
	BranchNone Branch = "none"
	// BranchConfirmed is branch 1 — `pending.target_version` equals this binary's
	// version, so the update took.
	BranchConfirmed Branch = "confirmed"
	// BranchDeferred is branch 2 — the target is not this version and
	// `llamaman-selfupdate.service` reports `activating` or `active`, so an actor
	// is working. This is the ONLY deferral in the protocol, it exists for the
	// intermediate boot section 9.4 step 7's failsafe can produce, and it is
	// bounded by that unit's own TimeoutStartSec=120 rather than by any clock
	// this design keeps.
	BranchDeferred Branch = "deferred"
	// BranchNotApplied is branch 3 — the target is not this version and no actor
	// is active. Either the swap never happened or the judge reverted it; both
	// mean the same thing, and the journal tail says which.
	BranchNotApplied Branch = "not_applied"
)

// Result is what one Resolve call decided, for the log line, for
// `GET /update/status` and for the tests that drive all three branches.
type Result struct {
	Branch Branch
	// Marker is the marker the branch acted on, zero when there was none.
	Marker Marker
	// MarkerUnreadable reports the sweep case: a `pending` this binary cannot
	// parse, which takes branch 3 naming the FILE rather than a version.
	MarkerUnreadable bool
	// ActorActive is what systemd said about `llamaman-selfupdate.service`. It is
	// also what `GET /update/status` renders as `pending.actor_active`, from the
	// same fact the gate acted on (§3.14).
	ActorActive bool
	// RowResolved reports whether a `self_updates` row actually moved. It is
	// false for a marker whose `self_update_id` names no row — which is a no-op
	// in both writing branches, not an error (§12.3).
	RowResolved bool
	// ClosedOrphans is how many rows the closing pass closed
	// `failed`/`daemon_restarted`.
	ClosedOrphans int
	// Notified reports whether F24 was raised.
	Notified bool
}

// UnitStater answers "is `llamaman-selfupdate.service` active?" — the one
// question branch 2 turns on. The interface is declared here because the
// consumer owns it (DESIGN section 1) and because naming it this narrowly keeps
// the gate from acquiring a systemd vocabulary it does not need.
type UnitStater interface {
	// ActiveState returns systemd's `ActiveState` for a unit. An error means the
	// question could not be asked, which the gate treats as "no actor is active"
	// — the conservative answer, because deferring on a fact nobody could
	// establish is how a marker outlives every process that knows what it means.
	ActiveState(ctx context.Context, unit string) (string, error)
}

// JournalTailer supplies the journal tail branch 3 carries into F24. A nil
// tailer, or one that returns the F23 hint, is the honest answer on a host whose
// identity cannot read the journal (D77): the card says so rather than showing
// an empty tail.
type JournalTailer interface {
	Tail(ctx context.Context, unit string, lines int) (string, error)
}

// GateConfig wires the gate.
type GateConfig struct {
	// Store is the database. The gate is the ONLY component in section 12 that
	// touches it: neither privileged actor may open it at all (section 11.3).
	Store *store.Store
	// Layout supplies `<state_dir>/update`.
	Layout Layout
	// Version is this binary's own version — the left-hand side of the one
	// comparison this routine makes. It is a field rather than a read of
	// internal/buildinfo so a test can be a version.
	Version string
	// BootID is `runtime_info.boot_id`, the lease owner every job this daemon
	// claims is stamped with (§2.3). It answers ONE question, in branch 2: is the
	// actor working on this update THIS process?
	//
	// Section 12.3 gives the closing pass exactly this guard, in exactly these
	// words — `interrupted` "means the lease belongs to a boot that is gone,
	// which is what makes the pass safe in all three callers rather than at boot
	// alone: it can never close work the calling process is itself performing" —
	// and the branch above it needs the same protection for the same reason. The
	// endpoint caller runs while this daemon is serving, so it can be invoked in
	// the window between step 6's marker write and step 7's drain, where the
	// marker names a version this binary is not and no unit is active yet.
	// Without this, a second Update click in that window takes branch 3 and
	// destroys the first update.
	//
	// Empty means "cannot tell", which reads as "not this process" and restores
	// the unguarded behavior.
	BootID string
	// Units answers branch 2's question. Nil means "no actor is active", which is
	// correct on a host with no service manager at all (F10): there is no oneshot
	// there to be working.
	Units UnitStater
	// Journal supplies branch 3's F24 tail. Nil raises the card without one.
	Journal JournalTailer
	// Now is the clock every row this routine writes is stamped from. It is NOT
	// a clock any DECISION is measured against — see the note at the top.
	Now func() time.Time
	// Log receives one line per resolution.
	Log *slog.Logger
	// Events appends the `events` row branch 1 emits, inside the gate's own
	// transaction. Nil skips it.
	Events EventAppender
}

// EventAppender is internal/events' Recorder, narrowed to the one call the gate
// makes inside its transaction.
type EventAppender interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
}

// Gate is ResolveUpdateMarkers plus D92's disarm, which share the in-memory copy
// of a marker that has already been unlinked.
type Gate struct {
	cfg GateConfig
	log *slog.Logger
	now func() time.Time

	// mu guards disarmed. The disarm runs on the boot goroutine before anything
	// else exists; the ticker and the endpoint run later and concurrently.
	mu sync.Mutex
	// disarmed is D92's in-memory copy: the marker step 4 read and unlinked
	// before the first migration was attempted. Section 12.3 is explicit that
	// "the two are the same input and the branches below do not distinguish
	// them", so it is consumed exactly like a marker read from disk.
	disarmed   *Marker
	disarmBad  bool // step 4 found a marker it could not parse and unlinked it anyway
	haveDisarm bool
}

// Attachments are the three collaborators the gate cannot have at the moment it
// is CONSTRUCTED, and the reason it has a setter at all.
//
// D92 forces the ordering: the disarm runs from the migration runner's
// BeforeFirst hook, which is section 11.1 step 4 — before the systemd probe of
// step 6 has chosen a control channel, and long before the event recorder and
// the rest of the services exist. The gate that performs the disarm must be the
// SAME object the step-11 gate is, because it carries the in-memory copy of the
// marker it unlinked (D92's "step 11's gate resolves the in-memory copy exactly
// as if it had read the file"). Constructing a second gate later would silently
// drop that copy and turn every migrating update into branch 3.
//
// So the gate is built early with the two things it must have — the store and
// the state directory — and the rest is attached once step 6 and the service
// construction have produced it.
type Attachments struct {
	Units   UnitStater
	Journal JournalTailer
	Events  EventAppender
	// BootID is the job queue's lease owner. It arrives here rather than at
	// construction for the same reason the three above do: the queue is built
	// well after step 4, because `runtime_info.boot_id` is not minted until the
	// migrations it depends on have run.
	BootID string
}

// Attach fills in the collaborators. It is called once, from the composition
// root, after step 6.
func (g *Gate) Attach(a Attachments) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a.Units != nil {
		g.cfg.Units = a.Units
	}
	if a.Journal != nil {
		g.cfg.Journal = a.Journal
	}
	if a.Events != nil {
		g.cfg.Events = a.Events
	}
	if a.BootID != "" {
		g.cfg.BootID = a.BootID
	}
}

// NewGate builds the gate.
func NewGate(cfg GateConfig) *Gate {
	g := &Gate{cfg: cfg, log: cfg.Log, now: cfg.Now}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

// DisarmBeforeMigration is section 11.1 step 4's second half, wired into the
// migration runner's BeforeFirst hook (D92).
//
// If `update/pending` exists and this boot is about to apply at least one
// migration, the marker is read into memory and UNLINKED NOW. Applying a
// migration is the exact instant `<prefix>/llamaman.prev` stops being a binary
// that could open this database, so it is the exact instant the judge's second
// ConditionPathExists= must stop holding — otherwise a daemon that migrates and
// THEN starts failing sends the unit to `failed` with both of the judge's
// conditions still true, and the judge renames back a binary that can no longer
// start. That second failure finds `<prefix>/llamaman.prev` consumed, so the
// judge is skipped, its ExecStopPost= does not run either, and the host is left
// with no daemon and no public gateway ports.
//
// The rule fires on "about to migrate", not on "a migration committed", and the
// price is stated rather than hidden: a boot whose FIRST migration fails also
// loses the revert even though the schema never moved (section 12.3 row 11b,
// whose exit is shorter — re-install the previous binary, no database restore).
// Moving the unlink later to reclaim that case is a change to D92 and re-opens
// the one window in this design that ends with a host that has no daemon.
//
// An `update/pending` this binary cannot parse is unlinked on the same rule and
// reaches the gate as the same in-memory fact, which is the sweep branch it
// would have taken from disk.
func (g *Gate) DisarmBeforeMigration() error {
	path := g.cfg.Layout.PendingPath()
	m, err := ReadMarker(path)
	switch {
	case errors.Is(err, ErrNoMarker):
		// Nothing in flight. The overwhelmingly common case, and it is not an
		// event: every ordinary migrating boot takes this arm.
		return nil
	case errors.Is(err, ErrMarkerUnreadable):
		g.mu.Lock()
		g.haveDisarm, g.disarmBad, g.disarmed = true, true, nil
		g.mu.Unlock()
		g.log.Warn("disarming the self-update revert before the first migration: "+
			"update/pending is unreadable and is being swept (D92)",
			"marker", path, "error", err)
	case err != nil:
		return err
	default:
		g.mu.Lock()
		copied := m
		g.haveDisarm, g.disarmBad, g.disarmed = true, false, &copied
		g.mu.Unlock()
		g.log.Info("disarming the self-update revert before the first migration (D92)",
			"marker", path, "from_version", m.FromVersion, "target_version", m.TargetVersion,
			"reason", "a migration is about to be attempted, so <prefix>/llamaman.prev "+
				"stops being a binary that could open this database")
	}
	return RemoveMarker(path)
}

// Resolve is ResolveUpdateMarkers.
func (g *Gate) Resolve(ctx context.Context) (Result, error) {
	marker, unreadable, have := g.takeMarker()

	switch {
	case !have:
		// No marker at all: straight to the closing pass.
		res := Result{Branch: BranchNone}
		n, err := g.closeOrphansOnly(ctx)
		res.ClosedOrphans = n
		return res, err

	case unreadable:
		// A `pending` this binary cannot read takes branch 3, naming the file
		// rather than a version. Sweeping a file no process is waiting for is
		// safe, and leaving it would reproduce the one property section 12 exists
		// to prevent: a file under `update/` outliving every process that knows
		// what it means (D91).
		return g.notApplied(ctx, Marker{}, true, false)

	case marker.TargetVersion == g.cfg.Version:
		return g.confirmed(ctx, marker)
	}

	// The target is not this version. One question decides the rest — "is an
	// actor working on this update?" — and it is a question about PROCESSES
	// rather than about a clock (D91). Two things can be that actor, one per
	// topology, and both are bounded by something other than this routine:
	//
	//   - `llamaman-selfupdate.service`, bounded by its own TimeoutStartSec=120;
	//   - in the D2 user-scope topology, and in the window section 12.1 step 7
	//     opens between the marker write and the drain, THIS daemon, bounded by
	//     section 9.4 step 7's 120 s failsafe and by the fact that a process that
	//     dies leaves its job `interrupted` for the next boot's triage.
	if g.selfIsApplying(ctx, marker) {
		return g.deferredToSelf(ctx, marker)
	}
	if g.unitActorActive(ctx) {
		return g.deferred(ctx, marker)
	}
	return g.notApplied(ctx, marker, false, false)
}

// selfIsApplying reports whether the update this marker names is being performed
// by THIS process right now.
//
// The predicate is the one section 12.3 already gives the closing pass, read
// from the other side: a `self_update` job whose lease owner is this boot and
// whose state is neither terminal nor `interrupted` belongs to a worker running
// in this process. `interrupted` is excluded explicitly rather than incidentally
// — it is exactly section 2.3's "the lease belongs to a boot that is gone", and
// every stop-point row that legitimately reaches branch 3 has it.
//
// It is a fact about a process, not a clock, so it cannot become the frozen
// deadline section 19's third preservation property forbids: the daemon that
// holds the lease either exists, or has died and left the job for triage.
func (g *Gate) selfIsApplying(ctx context.Context, m Marker) bool {
	if g.cfg.BootID == "" || m.SelfUpdateID == "" {
		return false
	}
	var mine bool
	err := g.cfg.Store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		job, err := g.cfg.Store.SelfUpdateJob(ctx, tx, m.SelfUpdateID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.State.IsTerminal() || job.State == model.JobInterrupted {
			return nil
		}
		mine = job.LeaseOwner != nil && *job.LeaseOwner == g.cfg.BootID
		return nil
	})
	if err != nil {
		// The database could not answer. Treat it as "not mine": a gate that
		// deferred on a fact nobody could establish is how a marker outlives every
		// process that knows what it means, which is the failure class D91 exists
		// to remove.
		g.log.Info("could not tell whether this daemon is itself applying the pending update",
			"self_update_id", m.SelfUpdateID, "error", err)
		return false
	}
	return mine
}

// deferredToSelf is branch 2 with this process as the actor: do nothing, and say
// so in terms a reader of the journal can act on.
func (g *Gate) deferredToSelf(ctx context.Context, m Marker) (Result, error) {
	res := Result{Branch: BranchDeferred, Marker: m, ActorActive: true}
	n, err := g.closeOrphansExcept(ctx, m.SelfUpdateID)
	res.ClosedOrphans = n
	if err != nil {
		return res, err
	}
	g.log.Info("this daemon is itself applying the pending update; the gate is standing down",
		"target_version", m.TargetVersion, "self_update_id", m.SelfUpdateID,
		"bound_by", "section 9.4 step 7's 120 s failsafe")
	return res, nil
}

// takeMarker returns the marker this call resolves: D92's in-memory copy on the
// first call of a boot that migrated, and the file otherwise. Section 12.3: "the
// two are the same input and the branches below do not distinguish them".
func (g *Gate) takeMarker() (m Marker, unreadable, have bool) {
	g.mu.Lock()
	if g.haveDisarm {
		g.haveDisarm = false
		bad, copied := g.disarmBad, g.disarmed
		g.disarmed = nil
		g.mu.Unlock()
		if bad {
			return Marker{}, true, true
		}
		return *copied, false, true
	}
	g.mu.Unlock()

	read, err := ReadMarker(g.cfg.Layout.PendingPath())
	switch {
	case errors.Is(err, ErrNoMarker):
		return Marker{}, false, false
	case err != nil:
		return Marker{}, true, true
	default:
		return read, false, true
	}
}

// unitActorActive is branch 2's question about the ROOT ONESHOT, and it is also
// exactly what `GET /update/status` renders as `pending.actor_active` (§3.14):
// `systemctl is-active llamaman-selfupdate.service`, nothing more.
//
// `deactivating` is deliberately NOT in the active set: it means the ExecStart
// process is already gone and only ExecStopPost= is left, so the swap is decided
// one way or the other and branch 3 is the right answer.
func (g *Gate) unitActorActive(ctx context.Context) bool {
	if g.cfg.Units == nil {
		return false
	}
	state, err := g.cfg.Units.ActiveState(ctx, SwapUnit)
	if err != nil {
		g.log.Info("could not ask the service manager whether a swap is in flight; "+
			"treating it as not active", "unit", SwapUnit, "error", err)
		return false
	}
	return state == "active" || state == "activating"
}

// confirmed is branch 1: the update took.
//
// Commit first and unlink second, so a kill between them leaves a terminal row
// beside a marker the next call resolves as a no-op — the branch is idempotent
// for a row already `succeeded`. `<prefix>/llamaman.prev` is deliberately KEPT:
// it is the emergency manual restore the F24 card names, it costs one binary's
// disk, and it is replaced wholesale by the next update's step 1, so it has a
// writer and can never go stale.
func (g *Gate) confirmed(ctx context.Context, m Marker) (Result, error) {
	res := Result{Branch: BranchConfirmed, Marker: m}
	now := g.now()

	err := g.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		moved, err := g.finishRow(ctx, tx, m.SelfUpdateID, model.UpdateSucceeded,
			model.JobSucceeded, "", nil, now)
		if err != nil {
			return err
		}
		res.RowResolved = moved
		if moved && g.cfg.Events != nil {
			if err := g.cfg.Events.Append(ctx, tx, g.event(now, m, model.LevelInfo,
				"update.confirmed", string(model.UpdateSwapping), string(model.UpdateSucceeded),
				fmt.Sprintf("self-update to %s confirmed by the boot that installed it", m.TargetVersion),
			)); err != nil {
				return err
			}
		}
		n, err := g.closeOrphans(ctx, tx, m.SelfUpdateID, now)
		res.ClosedOrphans = n
		return err
	})
	if err != nil {
		return res, err
	}

	if err := RemoveMarker(g.cfg.Layout.PendingPath()); err != nil {
		return res, err
	}
	if err := g.cfg.Layout.ClearScratch(); err != nil {
		return res, err
	}
	g.log.Info("self-update confirmed",
		"from_version", m.FromVersion, "target_version", m.TargetVersion,
		"self_update_id", m.SelfUpdateID, "row_resolved", res.RowResolved,
		"retained_binary", "kept — the next update replaces it")
	return res, nil
}

// deferred is branch 2: an actor is working, so do nothing.
//
// Leave the marker, leave the row `staged`/`swapping` and its job `interrupted`,
// and log at info. The deferral cannot outlive `llamaman-selfupdate.service`,
// which carries TimeoutStartSec=120: after that systemd kills it,
// ExecStopPost= runs, the unit is no longer active, and the next caller takes
// branch 3 (D91). The closing pass still runs, and it cannot touch THIS row
// because the surviving marker names it.
func (g *Gate) deferred(ctx context.Context, m Marker) (Result, error) {
	res := Result{Branch: BranchDeferred, Marker: m, ActorActive: true}
	n, err := g.closeOrphansExcept(ctx, m.SelfUpdateID)
	res.ClosedOrphans = n
	if err != nil {
		return res, err
	}
	g.log.Info("a self-update swap is in flight; deferring to the service manager",
		"unit", SwapUnit, "target_version", m.TargetVersion, "self_update_id", m.SelfUpdateID,
		"bound_by", SwapUnit+"'s own TimeoutStartSec=120")
	return res, nil
}

// notApplied is branch 3: the update did not take.
//
// Either the swap never happened, or it happened and the judge reverted it; both
// mean the same thing, and the journal tail says which. Deleting the marker here
// also DISARMS the judge — its ConditionPathExists= no longer holds — which is
// correct in both readings: if the swap never happened there is nothing to
// revert, and if the judge already reverted, `<prefix>/llamaman.prev` is gone
// anyway.
func (g *Gate) notApplied(ctx context.Context, m Marker, unreadable, _ bool) (Result, error) {
	res := Result{Branch: BranchNotApplied, Marker: m, MarkerUnreadable: unreadable}
	now := g.now()

	// The journal tail is read BEFORE the transaction: it forks `journalctl`,
	// and holding the one write connection open across a subprocess would block
	// every other writer for as long as the journal takes. The daemon sends
	// EXTEND_TIMEOUT_USEC= while the gate runs for exactly this reason
	// (section 11.1 step 11).
	tail := g.journalTail(ctx)

	message := g.notAppliedMessage(m, unreadable)
	err := g.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		moved := false
		if !unreadable && m.SelfUpdateID != "" {
			var err error
			moved, err = g.finishRow(ctx, tx, m.SelfUpdateID, model.UpdateFailed,
				model.JobFailed, string(model.CodeUpdateNotApplied), &message, now)
			if err != nil {
				return err
			}
		}
		res.RowResolved = moved

		if g.cfg.Events != nil {
			if err := g.cfg.Events.Append(ctx, tx, g.event(now, m, model.LevelWarn,
				"update.not_applied", "", string(model.UpdateFailed), message)); err != nil {
				return err
			}
		}
		// F24 is raised whether or not a row moved: a marker whose
		// `self_update_id` names no row still raises it, taking both version
		// strings from the marker rather than from a row (§12.3).
		if err := g.cfg.Store.AppendNotification(ctx, tx,
			g.f24(now, m, unreadable, message, tail)); err != nil {
			return err
		}
		res.Notified = true

		n, err := g.closeOrphans(ctx, tx, m.SelfUpdateID, now)
		res.ClosedOrphans = n
		return err
	})
	if err != nil {
		return res, err
	}

	if err := RemoveMarker(g.cfg.Layout.PendingPath()); err != nil {
		return res, err
	}
	if err := g.cfg.Layout.ClearScratch(); err != nil {
		return res, err
	}
	g.log.Warn("the self-update did not take", "detail", message,
		"installed_version", g.cfg.Version, "row_resolved", res.RowResolved)
	return res, nil
}

func (g *Gate) notAppliedMessage(m Marker, unreadable bool) string {
	if unreadable {
		return fmt.Sprintf(
			"%s could not be read by this binary and was swept; the installed version is %s",
			g.cfg.Layout.PendingPath(), g.cfg.Version)
	}
	return fmt.Sprintf(
		"the update from %s to %s did not take: either the swap never happened or it was reverted; "+
			"the installed version is %s",
		m.FromVersion, m.TargetVersion, g.cfg.Version)
}

// f24 builds the remediation card. It states which version is actually
// installed, and it carries the two actor units' journal tail — or the F23 hint
// when `journal_read` is denied — because "the swap never happened" and "the
// judge reverted it" are the same row and only the journal says which.
func (g *Gate) f24(now time.Time, m Marker, unreadable bool, message, tail string) store.Notification {
	action := map[string]any{
		"installed_version": g.cfg.Version,
		"units":             []string{SwapUnit, JudgeUnit},
	}
	if tail != "" {
		action["journal_tail"] = tail
	}
	if !unreadable {
		action["from_version"] = m.FromVersion
		action["target_version"] = m.TargetVersion
	}
	var actionJSON *string
	if b, err := json.Marshal(action); err == nil {
		s := string(b)
		actionJSON = &s
	}

	subjectType := string(model.SubjectSelfUpdate)
	n := store.Notification{
		ID:         store.NewID(now),
		At:         now.UnixMilli(),
		Severity:   model.SeverityError,
		Code:       string(model.CodeUpdateNotApplied),
		Title:      "The self-update did not take",
		Body:       message,
		ActionJSON: actionJSON,
	}
	if m.SelfUpdateID != "" {
		id := m.SelfUpdateID
		n.SubjectType, n.SubjectID = &subjectType, &id
	}
	return n
}

func (g *Gate) journalTail(ctx context.Context) string {
	if g.cfg.Journal == nil {
		return ""
	}
	var out string
	for _, unit := range []string{SwapUnit, JudgeUnit} {
		t, err := g.cfg.Journal.Tail(ctx, unit, journalTailLines)
		if err != nil || t == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += "-- " + unit + " --\n" + t
	}
	return out
}

// journalTailLines is how much of each actor unit's journal the F24 card
// carries. It is enough for the one structured line an actor logs on a refusal
// plus systemd's own start and stop lines around it.
const journalTailLines = 50

// finishRow closes a `self_updates` row and its paired job in one transaction —
// section 2.3a's rule that one transaction writes both, with the job carrying
// the error_code and the domain row the message.
//
// A marker whose `self_update_id` names no row is a NO-OP rather than an error.
// That state is reachable on the ordinary path and must not abort a boot:
// section 11.1 step 3's integrity check fails, F12 takes its "else start a fresh
// DB" arm, and step 11 then reads a marker whose id matches nothing; a
// `llamaman restore-db` to a snapshot older than the update produces the same
// shape.
func (g *Gate) finishRow(ctx context.Context, tx store.Tx, id string,
	rowState model.SelfUpdateState, jobState model.JobState,
	errorCode string, message *string, now time.Time) (bool, error) {

	if id == "" {
		return false, nil
	}
	moved, err := g.cfg.Store.FinishSelfUpdate(ctx, tx, id, rowState, message, now.UnixMilli())
	if err != nil {
		return false, err
	}
	if !moved {
		// Either the row does not exist, or it is already terminal. Both are
		// idempotent no-ops, and neither is an error.
		return false, nil
	}

	job, err := g.cfg.Store.SelfUpdateJob(ctx, tx, id)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if job.State.IsTerminal() {
		return true, nil
	}
	var codePtr *string
	if errorCode != "" {
		codePtr = &errorCode
	}
	return true, g.cfg.Store.FinishJob(ctx, tx, job.ID, jobState, codePtr, message, now.UnixMilli())
}

// closeOrphans is THE CLOSING PASS, run in the same transaction as whichever
// branch matched (section 12.3).
//
// Every non-terminal `self_updates` row whose paired `self_update` job is
// `interrupted` and which a surviving `update/pending` does not name is closed
// `failed` / `error_code='daemon_restarted'`, row and job together. Three
// properties make that guard exactly right, and all three are why the same pass
// can run in all three callers rather than at boot alone:
//
//   - `interrupted` means the lease belongs to a boot that is gone (§2.3), so
//     the pass can never close work the CALLING process is itself performing —
//     including a forward update that is `downloading` on this boot's own lease.
//   - It cannot touch branch 2's deferral, because that marker names that row.
//   - It cannot touch the row a matched branch just resolved, because that row
//     is now terminal.
//
// Without the pass, such a row's live job would refuse every future update at
// `409 job_in_flight` with no marker for any caller to find.
func (g *Gate) closeOrphans(ctx context.Context, tx store.Tx, excludeID string,
	now time.Time) (int, error) {

	orphans, err := g.cfg.Store.OrphanedSelfUpdates(ctx, tx, excludeID)
	if err != nil {
		return 0, err
	}
	code := string(model.CodeDaemonRestarted)
	message := "the daemon restarted while this update was in flight, " +
		"and no update/pending marker names it"
	closed := 0
	for _, o := range orphans {
		if _, err := g.cfg.Store.FinishSelfUpdate(ctx, tx, o.SelfUpdateID,
			model.UpdateFailed, &message, now.UnixMilli()); err != nil {
			return closed, err
		}
		if err := g.cfg.Store.FinishJob(ctx, tx, o.JobID,
			model.JobFailed, &code, &message, now.UnixMilli()); err != nil {
			return closed, err
		}
		closed++
	}
	if closed > 0 {
		g.log.Info("closed self-update rows a restart orphaned", "count", closed,
			"error_code", code)
	}
	return closed, nil
}

// closeOrphansOnly is the closing pass with no branch in front of it, for the
// "no marker at all" case.
func (g *Gate) closeOrphansOnly(ctx context.Context) (int, error) {
	return g.closeOrphansExcept(ctx, "")
}

// closeOrphansExcept runs the pass in its own transaction, for the two cases
// with no domain write to share one with.
func (g *Gate) closeOrphansExcept(ctx context.Context, excludeID string) (int, error) {
	var n int
	now := g.now()
	err := g.cfg.Store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = g.closeOrphans(ctx, tx, excludeID, now)
		return err
	})
	return n, err
}

func (g *Gate) event(now time.Time, m Marker, level model.EventLevel,
	action, from, to, message string) model.Event {

	ev := model.Event{
		ID:       store.NewID(now),
		At:       now.UnixMilli(),
		Level:    level,
		Category: model.CategoryUpdate,
		Action:   action,
		Actor:    model.ActorSystem,
		Message:  message,
	}
	if from != "" {
		ev.FromState = &from
	}
	if to != "" {
		ev.ToState = &to
	}
	if m.SelfUpdateID != "" {
		subjectType := string(model.SubjectSelfUpdate)
		id := m.SelfUpdateID
		ev.SubjectType, ev.SubjectID = &subjectType, &id
	}
	return ev
}

// TickerInterval is section 12.3's 30 s ticker: the gate's second caller, which
// runs ONLY while `update/pending` exists and is stopped by section 9.4's
// shutdown along with the other background workers.
//
// It is not a deadline and it measures nothing. It exists so that every stop
// point in section 12.3's table has an exit that does not wait for the next
// boot — after a refusal, section 9.4 step 7's 120 s failsafe returns this
// daemon to service and the next boot may be weeks away.
const TickerInterval = 30 * time.Second

// RunTicker is that second caller. It returns when ctx is done.
func (g *Gate) RunTicker(ctx context.Context) {
	t := time.NewTicker(TickerInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !g.MarkerExists() {
				continue
			}
			if _, err := g.Resolve(ctx); err != nil && ctx.Err() == nil {
				g.log.Error("could not resolve the self-update marker", "error", err)
			}
		}
	}
}

// MarkerExists reports whether `update/pending` is on disk. It is what gates the
// ticker's work and what `GET /update/status` and `doctor` read.
func (g *Gate) MarkerExists() bool {
	_, err := ReadMarker(g.cfg.Layout.PendingPath())
	return !errors.Is(err, ErrNoMarker)
}

// Pending reports the one self-update fact `GET /update/status` renders: the
// marker, if any, and whether the swap actor is active — the same fact the gate
// itself defers on (§3.14, D91).
func (g *Gate) Pending(ctx context.Context) (Marker, bool, bool) {
	m, err := ReadMarker(g.cfg.Layout.PendingPath())
	if errors.Is(err, ErrNoMarker) {
		return Marker{}, false, false
	}
	active := g.unitActorActive(ctx)
	if err != nil {
		// An unreadable marker is still a pending update as far as the UI is
		// concerned: the next gate call will sweep it, and until then saying
		// "nothing is pending" would be a lie about a file that is on disk.
		return Marker{}, true, active
	}
	return m, true, active
}
