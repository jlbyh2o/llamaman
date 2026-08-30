package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The corrective half of a pass (§5.8), and AT MOST ONE action leaves this
// file per instance per pass.
//
// "One action" is enforced by returning after each branch rather than by
// convention, because the branches are individually reasonable and collectively
// a disaster: on the pass after a crash a permissive reconciler would reassign
// a port, start the unit, enable it and write three ledger rows for one event,
// and the following pass would observe a state none of the three describes.

// actInput is everything the action phase reads. It is the RECORDED state, not
// the state the pass started from: the status row and the ledger both already
// agree with what was observed, so a decision made here is made against the
// database a user would see.
type actInput struct {
	inst   model.Instance
	status model.InstanceStatus
	obs    observation
	// lastClosed is LAST_CLOSED including the closure this pass just wrote.
	lastClosed   *model.InstanceStart
	runtimeReady bool
	now          time.Time
}

// act takes at most one corrective action.
func (s *Supervisor) act(ctx context.Context, in actInput) error {
	// No control channel, or a unit nobody can read: there is nothing to act
	// with, and acting on a gap in observation is how a supervisor ends up
	// running two of something.
	if s.control == nil || in.obs.unit == unitUnknown {
		return nil
	}
	// A soft-deleted instance is in the subject set for exactly one purpose —
	// so its open ledger row is closed by the one writer allowed to close it —
	// and that has already happened above.
	if in.inst.Deleted() {
		return s.stopIfRunning(ctx, in)
	}

	if in.inst.DesiredState == model.DesiredStopped {
		if err := s.stopIfRunning(ctx, in); err != nil {
			return err
		}
		// Issuing the stop WAS this pass's one action.
		if in.obs.unit == unitActive || in.obs.unit == unitActivating {
			return nil
		}
		return s.reconcileAutostart(ctx, in)
	}

	// desired `running`, actual stopped/failed/crash-looping.
	switch in.status.State {
	case model.InstanceStopped, model.InstanceFailed, model.InstanceCrashLooping:
		acted, err := s.startIfPolicyAllows(ctx, in)
		if err != nil || acted {
			return err
		}
	}
	return s.reconcileAutostart(ctx, in)
}

// stopIfRunning issues the one StopUnit a `desired_state='stopped'` instance
// needs. The ledger row is NOT closed here: the unit is still active, and the
// row closes on the pass that observes it inactive — which is the only moment
// the exit status exists to close it with.
func (s *Supervisor) stopIfRunning(ctx context.Context, in actInput) error {
	switch in.obs.unit {
	case unitActive, unitActivating:
	default:
		return nil
	}
	if _, err := s.control.Stop(ctx, unitName(in.inst)); err != nil {
		return fmt.Errorf("stop %s: %w", in.inst.Name, err)
	}
	return nil
}

// startIfPolicyAllows evaluates D7/D8/D63/D64 and either starts, declines, or
// waits. It reports whether it took the pass's one action.
func (s *Supervisor) startIfPolicyAllows(ctx context.Context, in actInput) (bool, error) {
	inst := in.inst

	var failed int
	from := CrashWindowStart(in.now, inst.RestartWindowSec, in.status.RestartWindowResetAt)
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := s.st.CountFailedStartsSince(ctx, tx, inst.ID, from)
		failed = n
		return err
	}); err != nil {
		return false, fmt.Errorf("count failed starts of %s: %w", inst.Name, err)
	}

	verdict := EvaluateStart(StartInput{
		State:            in.status.State,
		Policy:           inst.RestartPolicy,
		RestartMax:       inst.RestartMax,
		RestartWindowSec: inst.RestartWindowSec,
		LastClosed:       in.lastClosed,
		FailedInWindow:   failed,
		BackoffUntil:     in.status.ReconcileBackoffUntil,
		RuntimeReady:     in.runtimeReady,
		Now:              in.now,
	})

	switch verdict.Decision {
	case DecideWait:
		return false, nil
	case DecideInhibit:
		return true, s.inhibit(ctx, in, verdict)
	}

	// F5. A start that closed with exit 78 means the internal port was taken by
	// something else; the SUPERVISOR reassigns it, not the launcher, because
	// only the supervisor can see the whole pool. The write bumps `updated_at`
	// and emits an event; it does NOT bump `generation` and does NOT change
	// `config_hash` (D52), so a concurrent PATCH is not spuriously rejected and
	// no `restart_required` badge appears for a change the user did not make.
	if s.needsPortReassignment(in) {
		reassigned, err := s.reassignInternalPort(ctx, in)
		if err != nil {
			return true, err
		}
		if !reassigned {
			// The pool is exhausted. Stop retrying rather than cycling: the
			// state is `failed`, and F5's notification names the collision.
			return true, s.markPortPoolExhausted(ctx, in)
		}
		// The reassignment WAS this pass's action. Starting in the same pass
		// would launch against a row the pass has only just rewritten.
		return true, nil
	}

	return true, s.start(ctx, in, failed)
}

// start stamps the trigger and calls StartUnit.
//
// The stamp and the decision to start are one transaction, and the launcher
// consumes the stamp in its own (§5.6 step 3). Without this hand-off
// `instance_starts.trigger` would be a constant, because the launcher sees only
// `%i` and a row — and a start nobody stamped would have to be guessed rather
// than honestly recorded as `external`.
func (s *Supervisor) start(ctx context.Context, in actInput, failed int) error {
	inst := in.inst

	backoff := BackoffFor(failed + 1)
	until := in.now.Add(backoff).UnixMilli()
	next := in.status
	next.ReconcileBackoffUntil = &until

	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.st.StampPendingStart(ctx, tx, inst.ID,
			model.TriggerSupervisorRestart, nil, in.now.UnixMilli()); err != nil {
			return err
		}
		if _, err := s.st.UpdateInstanceStatus(ctx, tx, next); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, inst, in.now, "start_requested", model.LevelInfo,
			fmt.Sprintf("supervisor is starting %s", inst.Name), nil, nil)
	}); err != nil {
		return fmt.Errorf("stamp the pending start of %s: %w", inst.Name, err)
	}

	if _, err := s.control.Start(ctx, unitName(inst)); err != nil {
		s.log.Warn("supervisor: start failed",
			slog.String("instance", inst.Name), slog.String("error", err.Error()))
		// The start job failing is not a reason to fail the pass: the ledger
		// records what happened either way, on the next pass, and the backoff
		// above is already in place so this does not become a tight loop.
		return nil
	}
	s.publish(inst, in.now, "start_requested", nil, nil)
	return nil
}

// inhibit records the refusal — once per episode — and emits the event.
//
// The conditional is §2.8's, and it is the difference between a start history
// and 17 000 rows a day: the reconciler runs every `health_poll_sec`, so an
// unconditional write would bury the actual history of a
// `restart_policy='never'` instance within the hour. The `events` row and the
// derived badge are unconditional, because neither accumulates.
func (s *Supervisor) inhibit(ctx context.Context, in actInput, verdict StartVerdict) error {
	inst := in.inst

	var after int64
	if in.lastClosed != nil {
		after = in.lastClosed.At
	}

	return s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if verdict.CrashLooping && in.status.State != model.InstanceCrashLooping {
			next := in.status
			next.State = model.InstanceCrashLooping
			next.LastChangeAt = in.now.UnixMilli()
			if _, err := s.st.UpdateInstanceStatus(ctx, tx, next); err != nil {
				return err
			}
			if err := s.appendEvent(ctx, tx, inst, in.now, "crash_looping", model.LevelError,
				fmt.Sprintf("%s exceeded %d failed starts in %d s",
					inst.Name, inst.RestartMax, inst.RestartWindowSec),
				ptr(string(in.status.State)), ptr(string(model.InstanceCrashLooping))); err != nil {
				return err
			}
		}

		recorded, err := s.st.HasInhibitedStartSince(ctx, tx, inst.ID, verdict.Reason, after)
		if err != nil {
			return err
		}
		if !recorded {
			// `exit_code` is NULL and `ended_at` is now: this row describes a
			// REFUSAL, not a run. No execve happened, so there is no status to
			// record, and D64 never counts it.
			at := in.now.UnixMilli()
			reason := string(verdict.Reason)
			if err := s.st.InsertInstanceStart(ctx, tx, model.InstanceStart{
				ID:           s.newID(in.now),
				InstanceID:   inst.ID,
				At:           at,
				Trigger:      model.StartBySupervisorRestart,
				ConfigHash:   inst.ConfigHash,
				Outcome:      ptr(model.OutcomeInhibited),
				ErrorCode:    &reason,
				ErrorMessage: ptr(inhibitMessage(verdict.Reason, inst)),
				EndedAt:      &at,
			}); err != nil {
				return err
			}
		}

		return s.appendEvent(ctx, tx, inst, in.now, "restart_inhibited", model.LevelWarn,
			inhibitMessage(verdict.Reason, inst), nil, nil)
	})
}

// inhibitMessage names the policy, which is what §5.8 asks the event to do:
// "let the derived `inhibited` flag surface it" only works if the reason is
// legible without reading the code.
func inhibitMessage(reason model.InhibitReason, inst model.Instance) string {
	switch reason {
	case model.InhibitPolicyNever:
		return fmt.Sprintf("not restarting %s: restart_policy is 'never'", inst.Name)
	case model.InhibitCrashLoop:
		return fmt.Sprintf("not restarting %s: more than %d failed starts in %d s",
			inst.Name, inst.RestartMax, inst.RestartWindowSec)
	case model.InhibitCleanExit:
		return fmt.Sprintf("not restarting %s: it exited cleanly and restart_policy is 'on-failure'",
			inst.Name)
	default:
		return fmt.Sprintf("not restarting %s", inst.Name)
	}
}

// needsPortReassignment reports whether the last completed run lost the race
// for its internal port AND still names the port the instance holds.
//
// The second half is what makes this fire once per conflict rather than on
// every pass until the instance next succeeds: once the port has moved, the
// closed row describes a port the instance no longer has.
func (s *Supervisor) needsPortReassignment(in actInput) bool {
	last := in.lastClosed
	if last == nil || last.ExitCode == nil || *last.ExitCode != ExitPortConflict {
		return false
	}
	if last.DetailJSON == nil {
		return true
	}
	var detail struct {
		InternalPort int `json:"internal_port"`
	}
	if err := json.Unmarshal([]byte(*last.DetailJSON), &detail); err != nil {
		return true
	}
	return detail.InternalPort == in.inst.InternalPort
}

// reassignInternalPort allocates the next free port from
// `[instances.internal_port_min, instances.internal_port_max]`, skipping ports
// held by other instances and failing a live bind probe. It reports whether one
// was found.
func (s *Supervisor) reassignInternalPort(ctx context.Context, in actInput) (bool, error) {
	min := int(s.settingInt(ctx, "instances.internal_port_min", 21000))
	max := int(s.settingInt(ctx, "instances.internal_port_max", 21999))

	var holders []model.InstancePorts
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		h, err := s.st.InstancePortHolders(ctx, tx)
		holders = h
		return err
	}); err != nil {
		return false, fmt.Errorf("read port holders: %w", err)
	}

	taken := map[int]struct{}{}
	for _, h := range holders {
		if h.InstanceID == in.inst.ID {
			continue
		}
		taken[h.InternalPort] = struct{}{}
		taken[h.PublicPort] = struct{}{}
	}

	port := 0
	for candidate := min; candidate <= max; candidate++ {
		if candidate == in.inst.InternalPort {
			continue
		}
		if _, held := taken[candidate]; held {
			continue
		}
		if !s.probe(instances.LoopbackHost, candidate) {
			continue
		}
		port = candidate
		break
	}
	if port == 0 {
		return false, nil
	}

	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.st.ReassignInternalPort(ctx, tx, in.inst.ID, port, in.now.UnixMilli()); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, in.inst, in.now, "internal_port_reassigned", model.LevelWarn,
			fmt.Sprintf("internal port %d was in use; %s moved to %d",
				in.inst.InternalPort, in.inst.Name, port), nil, nil)
	}); err != nil {
		return false, fmt.Errorf("reassign the internal port of %s: %w", in.inst.Name, err)
	}
	s.publish(in.inst, in.now, "internal_port_reassigned", nil, nil)
	return true, nil
}

// markPortPoolExhausted is F5's terminal answer: stop retrying, say `failed`,
// and let the notification name the collision. Cycling through a full pool once
// per tick would burn a bind syscall per port per instance and never converge.
func (s *Supervisor) markPortPoolExhausted(ctx context.Context, in actInput) error {
	next := in.status
	next.State = model.InstanceFailed
	next.LastChangeAt = in.now.UnixMilli()
	next.LastError = ptr(ErrPortConflict)

	return s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.st.UpdateInstanceStatus(ctx, tx, next); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, in.inst, in.now, "internal_port_pool_exhausted",
			model.LevelError,
			fmt.Sprintf("no free internal port for %s in the configured pool", in.inst.Name),
			nil, nil)
	})
}

// reconcileAutostart is the `autostart` ≠ unit-enabled action, and it is gated
// on the same capability `PUT /instances/{id}/autostart` is (§11.1a).
//
// Ungated, this was an unconditional corrective action issuing a polkit-denied
// D-Bus call on every pass for every instance whose enable state the daemon
// cannot change — an error loop with no terminal state. Skipped, the divergence
// is reported once instead, and the instances table renders the autostart
// column read-only with the manual command in a tooltip.
func (s *Supervisor) reconcileAutostart(ctx context.Context, in actInput) error {
	if !s.cfg.ManageUnitFiles {
		return nil
	}
	unit := unitName(in.inst)

	enabled, known := s.observedEnablement(ctx, unit)
	if !known {
		// No unit-file state is observable through the control channel (see
		// Enablement's doc comment). Applying the declared value once per
		// daemon start converges without a D-Bus call per instance per tick.
		s.mu.Lock()
		applied, seen := s.appliedAutostart[in.inst.ID]
		s.mu.Unlock()
		if seen && applied == in.inst.Autostart {
			return nil
		}
		enabled = !in.inst.Autostart
	}
	if enabled == in.inst.Autostart {
		return nil
	}

	var err error
	if in.inst.Autostart {
		err = s.control.Enable(ctx, []string{unit})
	} else {
		err = s.control.Disable(ctx, []string{unit})
	}
	if err != nil {
		s.log.Warn("supervisor: could not reconcile autostart",
			slog.String("instance", in.inst.Name), slog.String("error", err.Error()))
		return nil
	}

	s.mu.Lock()
	s.appliedAutostart[in.inst.ID] = in.inst.Autostart
	s.mu.Unlock()
	return nil
}

// observedEnablement asks the Enablement seam, when one is wired.
func (s *Supervisor) observedEnablement(ctx context.Context, unit string) (bool, bool) {
	if s.cfg.Enablement == nil {
		return false, false
	}
	enabled, err := s.cfg.Enablement.Enabled(ctx, unit)
	if err != nil {
		return false, false
	}
	return enabled, true
}

// unmanagedAutostart lists the instances whose enablement the daemon may not
// change, and the exact commands that would reconcile them by hand.
//
// §5.8 asks for this as a SINGLE `autostart_unmanaged` notification, refreshed
// rather than repeated. It is exposed as a method so the layer that owns
// notifications can render it without this package importing one.
func (s *Supervisor) UnmanagedAutostart(ctx context.Context) ([]string, error) {
	if s.cfg.ManageUnitFiles {
		return nil, nil
	}
	var out []string
	err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		live, err := s.st.Instances(ctx, tx, store.InstanceFilter{})
		if err != nil {
			return err
		}
		for _, inst := range live {
			if !inst.Autostart {
				continue
			}
			out = append(out, "sudo systemctl enable "+unitName(inst))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
