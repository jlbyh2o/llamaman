package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// One reconcile pass for one instance (§5.8).
//
// A pass is three phases in a fixed order, and the order is the correctness
// argument. OBSERVE reads the unit and the ledger and decides nothing. RECORD
// writes what was observed — the status row, the ledger closure, the ready
// stamp — in ONE transaction, so no reader ever sees a run that is half over.
// Only then does ACT take at most one corrective action, against a database
// that already agrees with reality. A pass that acted first would routinely
// start an instance whose ledger still said it was running.

// unitState is the ActiveState axis, reduced to the five answers the transition
// table of §2.8 distinguishes plus the honest sixth.
type unitState int

const (
	// unitUnknown is a GAP IN OBSERVATION: no control channel, a unit systemd
	// does not know, or a properties read that failed. §2.8 treats it as such —
	// leaving it is a re-derivation, not a transition with its own rule — and
	// no corrective action is ever taken from it. Starting a unit whose state
	// cannot be read is how a supervisor ends up running two of something.
	unitUnknown unitState = iota
	unitInactive
	unitActivating
	unitActive
	unitDeactivating
	unitFailed
)

// observation is what one pass learned before it wrote anything.
type observation struct {
	unit   unitState
	props  systemd.UnitProps
	health Health
	// probed reports whether the health probe ran at all. A probe is made only
	// while the unit is active: probing a dead port would turn every stopped
	// instance into a 2 s timeout on every tick.
	probed bool
	// props2 is `/props`, read only on the transition INTO ready; gotProps says
	// whether it answered.
	props2   Props
	gotProps bool
}

// pass runs one instance's reconcile pass and reports whether that instance is
// still on its way up, which is what shortens the tick to one second.
func (s *Supervisor) pass(ctx context.Context, inst model.Instance,
	active store.ActiveVersion, runtimeReady bool) (bool, error) {

	now := s.now()

	// --- OBSERVE ------------------------------------------------------------
	var (
		status     model.InstanceStatus
		open       *model.InstanceStart
		lastClosed *model.InstanceStart
	)
	err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		st, err := s.st.InstanceStatus(ctx, tx, inst.ID)
		if err != nil {
			return err
		}
		status = st

		if row, err := s.st.OpenInstanceStart(ctx, tx, inst.ID); err == nil {
			open = &row
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if row, err := s.st.LastClosedInstanceStart(ctx, tx, inst.ID); err == nil {
			lastClosed = &row
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read %s: %w", inst.Name, err)
	}

	obs := s.observe(ctx, inst, status)

	// --- RECORD -------------------------------------------------------------
	next := status
	next.SystemdActive, next.SystemdSub, next.SystemdResult = nil, nil, nil
	next.MainPID = nil
	if obs.unit != unitUnknown {
		next.SystemdActive = ptr(activeStateName(obs.unit))
		next.SystemdSub = ptr(obs.props.SubState)
		next.SystemdResult = ptr(obs.props.Result)
		if obs.props.MainPID != 0 {
			next.MainPID = ptr(int64(obs.props.MainPID))
		}
	}
	if obs.probed {
		next.LastHealthAt = ptr(now.UnixMilli())
		next.HealthCode = ptr(int64(obs.health.Code))
	}

	// D25's version truth. A mismatch between the running executable and
	// `versions/active` is recorded and NOTHING else happens: the instance is
	// still ready and still serving, which is the entire point of
	// `stale_version` being a derived badge rather than a state.
	s.stampExeVersion(&next, obs, active)

	closure, closeRow := s.ledgerClosure(inst, obs, open, now)
	nextState, fails := s.deriveState(ctx, inst, status, obs, open, closure, now)
	s.mu.Lock()
	if obs.unit == unitActive {
		s.healthFails[inst.ID] = fails
	} else {
		delete(s.healthFails, inst.ID)
	}
	s.mu.Unlock()

	// The FIRST 200 of a run, in one transaction with everything else this pass
	// records: `instance_starts.ready_at` is stamped, that row's
	// `effective_config_hash` is copied into `instance_status.applied_config_hash`
	// — the only write of that column anywhere — and the state becomes `ready`.
	firstReady := obs.probed && obs.health.OK() && open != nil && open.ReadyAt == nil

	if nextState != status.State {
		next.LastChangeAt = now.UnixMilli()
	}
	next.State = nextState
	if nextState == model.InstanceReady && status.State != model.InstanceReady {
		next.ReadyAt = ptr(now.UnixMilli())
	}
	if firstReady && open.EffectiveConfigHash != nil {
		next.AppliedConfigHash = open.EffectiveConfigHash
	}
	if obs.gotProps {
		// What the SERVER is really serving with, not what was asked for:
		// llama.cpp is entitled to clamp a context to the model's trained
		// maximum, and the number beside a running instance should be the real
		// one.
		next.CtxSize = obs.props2.CtxSize
		next.SlotsTotal = obs.props2.SlotsTotal
	}
	if closeRow && closure.ExitCode != nil {
		next.LastExitCode = closure.ExitCode
		next.LastError = closure.ErrorCode
	}

	// D64's window reset: a start that WORKED. `restart_window_reset_at` moves
	// to the run's own `ready_at` — not to now — so an instance that served for
	// an hour and then crashed twice is at 2, not at 2 plus whatever it
	// accumulated last week.
	if open != nil && open.ReadyAt != nil &&
		now.Unix()-(*open.ReadyAt/1000) >= ReadySettleSec &&
		next.RestartWindowResetAt < *open.ReadyAt {
		next.RestartWindowResetAt = *open.ReadyAt
	}

	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if closeRow && open != nil {
			if _, err := s.st.CloseInstanceStart(ctx, tx, open.ID, closure); err != nil {
				return err
			}
		}
		if firstReady {
			if _, err := s.st.StampStartReady(ctx, tx, open.ID, now.UnixMilli()); err != nil {
				return err
			}
		}
		if err := s.synthesizeIfOwed(ctx, tx, inst, obs, open, lastClosed, now); err != nil {
			return err
		}
		if _, err := s.st.UpdateInstanceStatus(ctx, tx, next); err != nil {
			return err
		}
		if nextState != status.State {
			return s.appendEvent(ctx, tx, inst, now, "state_changed", model.LevelInfo,
				"", ptr(string(status.State)), ptr(string(nextState)))
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("record %s: %w", inst.Name, err)
	}
	if nextState != status.State {
		s.publish(inst, now, "state_changed", ptr(string(status.State)), ptr(string(nextState)))
	}
	if closeRow && open != nil {
		s.mu.Lock()
		delete(s.timedOut, open.ID)
		s.mu.Unlock()
		// The closure this pass wrote IS the previous completed run from here
		// on, so the policy below reads it rather than the row it superseded.
		// Getting this wrong would cost one whole tick of latency on every
		// restart decision, and — worse — would evaluate `on-failure` against
		// the run before last.
		lastClosed = closedRow(*open, closure)
		open = nil
	}

	comingUp := nextState == model.InstanceStarting || nextState == model.InstanceLoading

	// --- ACT ----------------------------------------------------------------
	if err := s.act(ctx, actInput{
		inst:         inst,
		status:       next,
		obs:          obs,
		lastClosed:   lastClosed,
		runtimeReady: runtimeReady,
		now:          now,
	}); err != nil {
		return comingUp, err
	}
	return comingUp, nil
}

// observe reads the unit's properties and, when it is active, probes health.
func (s *Supervisor) observe(ctx context.Context, inst model.Instance,
	status model.InstanceStatus) observation {

	if s.control == nil {
		// `systemd_control='unavailable'` (F10). The daemon starts into this
		// mode rather than refusing to, and it NEVER means it spawns
		// llama-server itself: every instance simply reads `unknown`.
		return observation{unit: unitUnknown}
	}

	props, err := s.control.Props(ctx, unitName(inst))
	if err != nil {
		if !errors.Is(err, systemd.ErrNoSuchUnit) {
			s.log.Debug("supervisor: unit properties unreadable",
				slog.String("instance", inst.Name), slog.String("error", err.Error()))
		}
		return observation{unit: unitUnknown}
	}

	obs := observation{unit: activeStateOf(props.ActiveState), props: props}
	if obs.unit != unitActive {
		return obs
	}

	probeCtx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()
	obs.health = s.prober.Health(probeCtx, inst.InternalPort)
	obs.probed = true

	// `/props` on the FIRST ready fills `ctx_size` and `slots_total`. It is
	// read HERE, in the observe phase, so the write phase stays one transaction
	// — and only on the first ready, because these are properties of a loaded
	// model rather than a per-tick measurement.
	//
	// A failure is silent by design: the two fields stay as they were, which is
	// honest, and a `/props` that a build does not serve must not stop an
	// instance from being recorded as ready.
	if obs.health.OK() && status.State != model.InstanceReady {
		if p, err := s.prober.Props(probeCtx, inst.InternalPort); err == nil {
			obs.props2, obs.gotProps = p, true
		}
	}
	return obs
}

// stampExeVersion records D25's comparison.
func (s *Supervisor) stampExeVersion(next *model.InstanceStatus,
	obs observation, active store.ActiveVersion) {

	if obs.props.MainPID == 0 || s.exe == nil {
		return
	}
	exe, err := s.exe(int(obs.props.MainPID))
	if err != nil {
		return
	}
	dir := s.activeDir()
	if dir == "" {
		return
	}
	// The executable lives at <version dir>/bin/llama-server, so the comparison
	// is a prefix test against the resolved active directory. Equal means this
	// process is running the active build; different means it is running the
	// one it was started with, which is exactly the fact F8's badge reports.
	if len(exe) > len(dir) && exe[:len(dir)] == dir {
		next.ExeVersionID = ptr(active.ID)
		return
	}
	// A build this daemon cannot name is still worth recording as "not the
	// active one": the badge says "restart to apply <active>", and it is right.
	next.ExeVersionID = ptr(exeVersionID(exe))
}

// exeVersionID recovers a version id from an executable path of the shape
// `<state_dir>/versions/<id>/bin/llama-server`. An unrecognizable path yields
// the path itself, which is more useful in a badge than an empty string.
func exeVersionID(exe string) string {
	const marker = "/versions/"
	i := strings.Index(exe, marker)
	if i < 0 {
		return exe
	}
	rest := exe[i+len(marker):]
	if j := strings.Index(rest, "/"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// ledgerClosure decides whether the open row must be closed now, and as what.
//
// It is §5.6's writer table, in the table's own precedence: an explicit stop
// wins over the exit status, a start this supervisor timed out is a failure
// whatever the unit says, and everything else is decided by ExecMainStatus and
// Result. Because `outcome` is written EXACTLY ONCE (D63) and `ready` is not one
// of its values, no two rules can ever close the same row, and there is no
// precedence question left to resolve after this function returns.
func (s *Supervisor) ledgerClosure(inst model.Instance, obs observation,
	open *model.InstanceStart, now time.Time) (store.StartClosure, bool) {

	if open == nil {
		return store.StartClosure{}, false
	}
	if obs.unit != unitInactive && obs.unit != unitFailed {
		// A unit still active leaves the row OPEN — the process it describes is
		// still running — and an unobservable unit leaves it open too, because
		// closing a row from a gap in observation would record an outcome
		// nothing witnessed.
		return store.StartClosure{}, false
	}

	ended := now.UnixMilli()
	if !obs.props.ExecMainExitTimestamp.IsZero() {
		ended = obs.props.ExecMainExitTimestamp.UnixMilli()
	}
	exit := int64(obs.props.ExecMainStatus)
	closure := store.StartClosure{ExitCode: &exit, EndedAt: ended}

	switch {
	case inst.DesiredState == model.DesiredStopped:
		// A stop the user asked for is not a failure even when llama-server
		// exits non-zero on SIGINT, and `desired_state` IS the record that one
		// was asked for — which is why nothing else has to remember it.
		closure.Outcome = model.OutcomeStopped

	case s.wasTimedOut(open.ID):
		closure.Outcome = model.OutcomeFailed
		closure.ErrorCode = ptr(ErrStartTimeout)
		closure.ErrorMessage = ptr("the instance did not become ready within start_timeout_sec")

	case obs.props.ExecMainStatus != 0 || (obs.props.Result != "" && obs.props.Result != "success"):
		closure.Outcome = model.OutcomeFailed
		if WritesOwnLedgerRow(int(obs.props.ExecMainStatus)) &&
			obs.props.ExecMainStatus != 0 && open.ArgvJSON == nil {
			// The launcher failed preflight and could not close its own row —
			// a locked or unwritable database (§5.6). The exit status is the
			// contract, so the code it names is recoverable even though the
			// process that knew it is gone.
			if code, ok := launcherErrorCode(int(obs.props.ExecMainStatus)); ok {
				closure.ErrorCode = ptr(code)
			}
		}
		if closure.ErrorCode == nil {
			closure.ErrorCode = ptr(ErrUnitFailed)
		}

	default:
		// A clean, UNREQUESTED exit. The ledger says `stopped`, which is what
		// makes `on-failure` decline to restart it, and the STATE says `failed`,
		// which is what makes the `clean_exit` inhibit reason visible — two
		// different questions, answered separately on purpose (§2.8).
		closure.Outcome = model.OutcomeStopped
	}
	return closure, true
}

// launcherErrorCode maps a launcher exit status onto the `error_code` §5.6's
// table pairs it with. Exit 69 has two codes and this returns the one whose
// remediation is the safe default: `runtime_missing` offers a rebuild, which is
// correct advice even when the truth was `runtime_rebuilding` and the wait
// would have sufficed.
func launcherErrorCode(status int) (string, bool) {
	switch status {
	case ExitBadFlags:
		return ErrBadFlags, true
	case ExitRuntimeMissing:
		return ErrRuntimeMissing, true
	case ExitModelMissing:
		return ErrModelMissing, true
	case ExitPortConflict:
		return ErrPortConflict, true
	default:
		return "", false
	}
}

// deriveState is §2.8's transition table, and it returns the consecutive
// health-failure count alongside the state because the two are computed from
// the same observation and stored in different places.
func (s *Supervisor) deriveState(ctx context.Context, inst model.Instance,
	status model.InstanceStatus, obs observation, open *model.InstanceStart,
	closure store.StartClosure, now time.Time) (model.InstanceState, int) {

	switch obs.unit {
	case unitUnknown:
		return model.InstanceUnknown, 0

	case unitActivating:
		return model.InstanceStarting, 0

	case unitDeactivating:
		return model.InstanceStopping, 0

	case unitInactive, unitFailed:
		// A stop the user asked for is the one thing that leaves
		// `crash-looping`, besides the two recovery endpoints — §2.8's
		// `failed`/`crash-looping` + stop requested → `stopped` row.
		if inst.DesiredState == model.DesiredStopped {
			return model.InstanceStopped, 0
		}
		// `failed` and `crash-looping` are STICKY against an inactive unit, and
		// both stick for the same reason: §2.8's table has no edge out of either
		// except a requested stop and the two recovery endpoints. Re-deriving
		// them each pass would decay a failure to `stopped` the moment its
		// ledger row was closed — and `stopped` is not a failure, so
		// `restart_policy` would stop applying, `inhibited` would go false, and
		// an instance the supervisor is deliberately declining to restart would
		// be restarted on the very next tick.
		if status.State == model.InstanceCrashLooping || status.State == model.InstanceFailed {
			return status.State, 0
		}
		if obs.unit == unitFailed {
			return model.InstanceFailed, 0
		}
		// The `ready → failed` row is load-bearing, not tidiness: a `ready`
		// instance whose llama-server exits by itself — a segfault, an upstream
		// exit(0) on an internal error, an operator's kill — lands in `failed`,
		// which is the state `inhibit_reason='clean_exit'` is visible from. An
		// UNREQUESTED exit is `failed` regardless of exit code; the exit code
		// decides the ledger `outcome`, and the ledger `outcome` decides whether
		// the supervisor restarts. Two different questions, answered separately.
		if open != nil {
			return model.InstanceFailed, 0
		}
		return model.InstanceStopped, 0

	case unitActive:
		fails := s.healthFailures(inst.ID)
		switch {
		case obs.health.OK():
			return model.InstanceReady, 0
		case obs.health.Loading():
			return model.InstanceLoading, 0
		}
		fails++

		// Three consecutive failures with the unit still active is `degraded` —
		// a state, not a ledger row: a run that recovers has one row, not three.
		if status.State == model.InstanceReady || status.State == model.InstanceDegraded {
			if fails >= 3 {
				return model.InstanceDegraded, fails
			}
			return status.State, fails
		}

		// Still coming up. `start_timeout_sec` is the bound on that, and it is
		// measured from the ledger row's own `at` rather than from a timer this
		// process holds, so a daemon restart mid-load does not restart the
		// clock.
		if open != nil && open.ReadyAt == nil {
			timeout := s.settingInt(ctx, "instances.start_timeout_sec", 900)
			if now.UnixMilli()-open.At > timeout*1000 {
				s.markTimedOut(open.ID)
				return model.InstanceFailed, fails
			}
		}
		if status.State == model.InstanceLoading {
			return model.InstanceLoading, fails
		}
		return model.InstanceStarting, fails
	}
	return model.InstanceUnknown, 0
}

// synthesizeIfOwed writes the closed row §5.6 owes for a launcher that exited
// BEFORE its own row existed — exits 70 and 75.
//
// Nothing is written for exit 64: the instance row is gone, so the foreign key
// has no parent and the insert could not succeed. The synthesized rows are
// ordinary closed rows and appear in `GET /instances/{id}/starts` like any
// other; they differ in one respect only, which is that `argv_json`,
// `effective_config_hash` and `llamacpp_version_id` are NULL, because nothing
// was rendered.
func (s *Supervisor) synthesizeIfOwed(ctx context.Context, tx store.Tx,
	inst model.Instance, obs observation, open, lastClosed *model.InstanceStart,
	now time.Time) error {

	if obs.unit != unitFailed || open != nil {
		s.forgetSynthesis(inst.ID)
		return nil
	}
	status := int(obs.props.ExecMainStatus)
	code, owed := SynthesizedErrorCode(status)
	if !owed {
		return nil
	}

	exitAt := now.UnixMilli()
	if !obs.props.ExecMainExitTimestamp.IsZero() {
		exitAt = obs.props.ExecMainExitTimestamp.UnixMilli()
	}
	// One row per EXIT, not one per pass. The unit stays `failed` until somebody
	// starts it again, and this runs every tick.
	if !s.claimSynthesis(inst.ID, exitAt) {
		return nil
	}
	if lastClosed != nil && lastClosed.EndedAt != nil && *lastClosed.EndedAt >= exitAt {
		return nil
	}

	exit := int64(status)
	return s.st.InsertInstanceStart(ctx, tx, model.InstanceStart{
		ID:           s.newID(now),
		InstanceID:   inst.ID,
		At:           exitAt,
		Trigger:      model.StartByExternal,
		ConfigHash:   inst.ConfigHash,
		Outcome:      ptr(model.OutcomeFailed),
		ExitCode:     &exit,
		ErrorCode:    &code,
		ErrorMessage: ptr("the launcher exited before it could open a start row"),
		EndedAt:      &exitAt,
	})
}

// closedRow projects a closure back onto the row it closed, so the policy in
// the same pass reads the run that just ended rather than the one before it.
//
// `At` is deliberately the row's OWN `at`, not the closure's `ended_at`: §2.8's
// one-row-per-refusal-episode rule compares an `inhibited` row's `at` against
// LAST_CLOSED's, and moving it forward to the end of the run would make a
// refusal written moments later look older than the run it refused to follow.
func closedRow(row model.InstanceStart, c store.StartClosure) *model.InstanceStart {
	row.Outcome = ptr(c.Outcome)
	row.ExitCode = c.ExitCode
	row.ErrorCode = c.ErrorCode
	row.ErrorMessage = c.ErrorMessage
	row.DetailJSON = c.DetailJSON
	row.EndedAt = &c.EndedAt
	return &row
}

func activeStateOf(s string) unitState {
	switch s {
	case "active", "reloading":
		return unitActive
	case "activating":
		return unitActivating
	case "deactivating":
		return unitDeactivating
	case "failed":
		return unitFailed
	case "inactive":
		return unitInactive
	default:
		return unitUnknown
	}
}

func activeStateName(u unitState) string {
	switch u {
	case unitActive:
		return "active"
	case unitActivating:
		return "activating"
	case unitDeactivating:
		return "deactivating"
	case unitFailed:
		return "failed"
	case unitInactive:
		return "inactive"
	default:
		return "unknown"
	}
}

func (s *Supervisor) healthFailures(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthFails[id]
}

func (s *Supervisor) markTimedOut(startID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timedOut[startID] = true
}

func (s *Supervisor) wasTimedOut(startID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timedOut[startID]
}

// claimSynthesis reports whether this exit still needs its synthesized row, and
// records that this pass is writing it.
func (s *Supervisor) claimSynthesis(instanceID string, exitAt int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.synthesized == nil {
		s.synthesized = map[string]int64{}
	}
	if at, ok := s.synthesized[instanceID]; ok && at == exitAt {
		return false
	}
	s.synthesized[instanceID] = exitAt
	return true
}

func (s *Supervisor) forgetSynthesis(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.synthesized, instanceID)
}

// appendEvent writes one `events` row inside the caller's transaction.
func (s *Supervisor) appendEvent(ctx context.Context, tx store.Tx, inst model.Instance,
	now time.Time, action string, level model.EventLevel, message string, from, to *string) error {

	if s.events == nil {
		return nil
	}
	return s.events.Append(ctx, tx, s.newEvent(inst, now, action, level, message, from, to))
}

// publish fans an event out AFTER the transaction commits.
func (s *Supervisor) publish(inst model.Instance, now time.Time, action string, from, to *string) {
	if s.events == nil {
		return
	}
	s.events.Publish(s.newEvent(inst, now, action, model.LevelInfo, "", from, to))
}

func (s *Supervisor) newEvent(inst model.Instance, now time.Time, action string,
	level model.EventLevel, message string, from, to *string) model.Event {

	subjectType, subjectID := "instance", inst.ID
	return model.Event{
		ID:          s.newID(now),
		At:          now.UnixMilli(),
		Level:       level,
		Category:    model.CategoryInstance,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Action:      action,
		FromState:   from,
		ToState:     to,
		// The supervisor acts on systemd's behalf, never on an admin's: the
		// distinction is what makes the audit log answer "did a person do
		// this?" without reading the message.
		Actor:   model.ActorSystemd,
		Message: message,
	}
}

func ptr[T any](v T) *T { return &v }
