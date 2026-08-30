package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Boot reconciliation (§5.8), in the order the design fixes and for the reasons
// it gives. It runs once, before the loop starts, and the order is not
// negotiable: step 1 decides what boot this is and may rewrite every
// `desired_state`; step 2 replaces every observation the previous daemon left
// behind; step 3 closes the ledger rows that daemon abandoned; only then does
// step 4 let the reconciler act. Reversing any pair produces a visible bug —
// most sharply steps 1 and 4, where acting before the coupling has agreed with
// what systemd already did at boot gives a start-then-stop flap on every
// autostart instance.

// BootReconcile performs steps 1 through 3. Step 4 is Run's loop.
func (s *Supervisor) BootReconcile(ctx context.Context) error {
	now := s.now()

	changed, hostBoot, err := s.hostBootDecision(ctx, now)
	if err != nil {
		return err
	}

	// Step 2. Read every managed unit's properties once and write
	// `instance_status`, so a daemon restart never shows a stale "ready", and
	// step 3. Close any row left open by the previous daemon and synthesize the
	// rows that daemon's launchers could not write.
	//
	// Both are exactly what one reconcile pass does, which is why this is one
	// pass rather than a second implementation of the same table. The pass
	// takes no corrective action on an autostart instance, because step 1 has
	// already agreed with what systemd did at boot.
	if err := s.reconcileOpenRows(ctx, now); err != nil {
		return err
	}

	// Only when the host boot changed: the ONE relabel of D74.
	if changed {
		if err := s.relabelBootStarts(ctx, hostBoot, now); err != nil {
			return err
		}
	}

	return s.Reconcile(ctx)
}

// hostBootDecision is §5.8 boot reconciliation step 1, and it is the ONLY
// writer of `runtime_info.host_boot_id` and `host_boot_at` in the whole design.
//
// That exclusivity is the point rather than a style preference. §11.1 step 9
// performs the same read during the boot sequence — it needs the answer for
// logging and for `runtime_info` display — but it writes NOTHING. An earlier
// reading had step 9 record the new value before the supervisor started, which
// meant this comparison always saw equality, the D53 coupling never fired, and
// autostart was broken in both directions: exactly the failure D53 exists to
// prevent, produced by the mechanism meant to detect it.
//
// Two readers, one writer, and the writer is the one that acts on the answer.
func (s *Supervisor) hostBootDecision(ctx context.Context, now time.Time) (bool, HostBoot, error) {
	boot, err := s.host()
	if err != nil {
		// A host whose boot identity cannot be read is not a host to guess
		// about: leaving `desired_state` exactly as it was means an instance
		// that crashed while the daemon was down is still repaired, and no
		// instance is stopped on a hunch.
		s.log.Warn("supervisor: host boot identity unavailable; the autostart coupling is skipped",
			slog.String("error", err.Error()))
		return false, HostBoot{}, nil
	}

	var info model.RuntimeInfo
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		got, err := s.st.RuntimeInfo(ctx, tx)
		info = got
		return err
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, boot, fmt.Errorf("read runtime_info: %w", err)
	}

	same := info.HostBootID != nil && *info.HostBootID == boot.ID
	if same {
		// A daemon restart WITHIN one host boot. `desired_state` is left
		// exactly as it was, so D7 still holds: an instance that crashed while
		// the daemon was down is restarted when the daemon returns.
		return false, boot, nil
	}

	// The first daemon start of a new host boot. Apply the coupling, and only
	// THEN write the new identity — writing it first would make the very next
	// daemon start see equality and skip a coupling that had not happened.
	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		couplings, err := s.st.AutostartCouplings(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range couplings {
			want := model.DesiredStopped
			if c.Autostart {
				want = model.DesiredRunning
			}
			if c.DesiredState == want {
				continue
			}
			if _, err := s.st.SetInstanceDesiredState(ctx, tx, c.ID, want, now.UnixMilli()); err != nil {
				return err
			}
			// One events row per CHANGE, which is what makes the coupling
			// auditable: "autostart off actually means off" is a claim a user
			// can check against the log rather than take on faith.
			if err := s.appendEvent(ctx, tx,
				model.Instance{ID: c.ID, Name: c.Name}, now,
				"desired_state_coupled", model.LevelInfo,
				fmt.Sprintf("host boot: %s has autostart=%t, so desired_state is now %s",
					c.Name, c.Autostart, want),
				ptr(string(c.DesiredState)), ptr(string(want))); err != nil {
				return err
			}
		}
		return s.st.SetHostBoot(ctx, tx, boot.ID, boot.At.UnixMilli())
	}); err != nil {
		return false, boot, fmt.Errorf("apply the host-boot coupling: %w", err)
	}

	s.log.Info("supervisor: new host boot; the autostart coupling was applied",
		slog.String("host_boot_id", boot.ID))
	return true, boot, nil
}

// reconcileOpenRows is boot reconciliation step 3's first half: close any
// `instance_starts` row left open by the previous daemon.
//
// A unit that is STILL ACTIVE leaves its row open — the process the row
// describes is still running, and closing it would invent an end for a run that
// has not had one. A unit that is gone closes it `failed` with
// `error_code='daemon_restarted'`. Soft-deleted instances are included, which
// is exactly why the reconcile set carries the open-row term (§3.10c).
func (s *Supervisor) reconcileOpenRows(ctx context.Context, now time.Time) error {
	var ids []string
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		got, err := s.st.InstancesWithOpenStarts(ctx, tx)
		ids = got
		return err
	}); err != nil {
		return fmt.Errorf("list instances with open starts: %w", err)
	}

	for _, id := range ids {
		var inst model.Instance
		if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			got, err := s.st.Instance(ctx, tx, id)
			inst = got
			return err
		}); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return fmt.Errorf("load instance %s: %w", id, err)
		}

		obs := s.observe(ctx, inst, model.InstanceStatus{})
		if obs.unit == unitActive || obs.unit == unitActivating || obs.unit == unitUnknown {
			// Still running, still coming up, or unobservable. The first two
			// keep their row for the ordinary reason; the third keeps it
			// because a gap in observation is not evidence a run ended.
			continue
		}

		reason := ErrDaemonRestarted
		if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			closure := store.StartClosure{
				Outcome:      model.OutcomeFailed,
				ErrorCode:    &reason,
				ErrorMessage: ptr("the daemon restarted while this run was in flight"),
				EndedAt:      now.UnixMilli(),
			}
			if !obs.props.ExecMainExitTimestamp.IsZero() {
				closure.EndedAt = obs.props.ExecMainExitTimestamp.UnixMilli()
			}
			if obs.props.ExecMainStatus != 0 {
				closure.ExitCode = ptr(int64(obs.props.ExecMainStatus))
			}
			_, err := s.st.CloseOpenInstanceStart(ctx, tx, inst.ID, closure)
			return err
		}); err != nil {
			return fmt.Errorf("close the abandoned start row of %s: %w", inst.Name, err)
		}
	}
	return nil
}

// relabelBootStarts is D74's one relabel, and it runs ONLY on the first daemon
// start of a new host boot.
//
// The remaining honest-but-misleading case is a start systemd performed at boot
// through `llamaman-instances.target`, before the daemon was up: nobody stamped
// it, so the launcher recorded `external`, and yet the user did ask for it by
// enabling autostart. Within one host boot no relabel ever happens, so a
// hand-run start is a hand-run start forever. A hand-run start that genuinely
// happened in the boot window before the daemon came up is still
// indistinguishable from an autostart, and the ledger says `autostart`; that
// ambiguity is inherent — nothing observed it — and is a few seconds wide
// rather than days.
func (s *Supervisor) relabelBootStarts(ctx context.Context, boot HostBoot, now time.Time) error {
	var bootAt int64
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		info, err := s.st.RuntimeInfo(ctx, tx)
		if err != nil {
			return err
		}
		if info.BootAt != nil {
			bootAt = *info.BootAt
		}
		return nil
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("read runtime_info for the relabel window: %w", err)
	}
	if bootAt == 0 {
		// No daemon start instant recorded yet: this daemon IS the upper bound.
		bootAt = now.UnixMilli()
	}

	hostBootAt := boot.At.UnixMilli()
	if hostBootAt >= bootAt {
		// An empty or inverted window relabels nothing, which is the correct
		// answer rather than an error: a daemon that started before the host
		// boot instant it just read is a clock that moved, not a ledger to
		// rewrite.
		return nil
	}

	var n int64
	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		got, err := s.st.RelabelBootStarts(ctx, tx, hostBootAt, bootAt)
		n = got
		return err
	}); err != nil {
		return fmt.Errorf("relabel boot starts: %w", err)
	}
	if n > 0 {
		s.log.Info("supervisor: boot starts relabeled as autostart", slog.Int64("rows", n))
	}
	return nil
}
