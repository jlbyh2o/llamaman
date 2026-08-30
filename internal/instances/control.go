package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The three guarded transitions behind DESIGN sections 3.10b and 3.10's
// remaining control verbs: safe start, reset failed, and the autostart toggle.
//
// They are here rather than in service.go because they share one property that
// the create/patch/delete triple does not: each writes into `instance_status`,
// which section 2.8 declares the SUPERVISOR's table. The exception list is
// exactly three columns — the crash-loop latch, the reconcile backoff and the
// restart window — and it exists for a single reason, stated in section 3.10b:
// they have to land synchronously with the request, because a Safe start whose
// backoff clears on a later supervisor pass is a button that appears to do
// nothing. Every other column here stays the supervisor's.

// SafeOverride is section 3.10b's transient patch: `-ngl 0 -c 2048`, one slot.
//
// It is a shallow patch over the parsed FlagSet — present keys replace, absent
// keys are untouched, `extra_flags` is dropped for the run — and it is consumed
// and CLEARED by `instance-exec` in its step-3 transaction. That is what "never
// persisted" has to mean for a system whose supervisor may restart the unit on
// its own: the override survives neither a crash nor a reboot, and the next
// start from any trigger is the saved configuration again.
func SafeOverride() map[string]any {
	return map[string]any{
		"n_gpu_layers": map[string]any{"mode": string(model.NGLNone)},
		"ctx_size":     2048,
		"parallel":     1,
	}
}

// SafeStart is `POST /api/v1/instances/{id}/safe-start` (D61, section 3.10b).
//
// One transaction does all six things section 3.10b step 1 enumerates:
// `desired_state='running'`, `pending_trigger='safe_start'`, the override blob,
// the cleared backoff, the `crash-looping → stopped` move if it was latched, and
// the restart window restarted at now. It does NOT call systemd — the supervisor
// reads `(desired, actual)` on its next pass, exactly as an ordinary start does.
func (s *Service) SafeStart(ctx context.Context, id string) (View, error) {
	blob, err := json.Marshal(SafeOverride())
	if err != nil {
		return View{}, fmt.Errorf("instances: encode the safe-start override: %w", err)
	}
	override := string(blob)

	now := s.now()
	nowMS := now.UnixMilli()

	var (
		out  View
		sink events.Sink
	)
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		inst, err := s.store.Instance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.Deleted() {
			return store.ErrNotFound
		}
		if _, err := s.store.SetInstanceDesiredState(ctx, tx, id,
			model.DesiredRunning, nowMS); err != nil {
			return err
		}
		if _, err := s.store.StampPendingStart(ctx, tx, id,
			model.TriggerSafeStart, &override, nowMS); err != nil {
			return err
		}
		// The narrow exception of section 2.8, and the reason this is not just
		// SetDesiredState with a different trigger: a safe start is a recovery
		// from a crash loop, and a crash loop is precisely the state whose
		// backoff would otherwise make the button do nothing for minutes.
		if _, err := s.store.ClearCrashLoopLatch(ctx, tx, id, nowMS); err != nil {
			return err
		}

		to := string(model.DesiredRunning)
		if err := s.event(ctx, tx, now, inst, "instance_safe_start", model.LevelInfo,
			fmt.Sprintf("instance %s is starting in safe mode (-ngl 0, ctx 2048)", inst.Name),
			ptrTo(string(inst.DesiredState)), &to, &sink); err != nil {
			return err
		}

		row, err := s.store.InstanceView(ctx, tx, id)
		if err != nil {
			return err
		}
		out, err = s.viewOf(row, s.activeRuntime(ctx, tx))
		return err
	})
	if err != nil {
		return View{}, err
	}
	s.publish(&sink)
	return out, nil
}

// ResetFailed is `POST /api/v1/instances/{id}/reset-failed` (D64).
//
// The three columns are the same narrow exception SafeStart writes, and the
// systemd `ResetFailed` that follows is the caller's: this package owns no
// systemd vocabulary (section 1, invariant 2), and the composition root makes
// that call after this transaction commits. The order matters — clearing the
// unit's failed state before the database agrees would let the supervisor's next
// pass see a healthy unit and a latched row.
func (s *Service) ResetFailed(ctx context.Context, id string) (View, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	var (
		out  View
		sink events.Sink
	)
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		inst, err := s.store.Instance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.Deleted() {
			return store.ErrNotFound
		}
		if _, err := s.store.ClearCrashLoopLatch(ctx, tx, id, nowMS); err != nil {
			return err
		}
		if err := s.event(ctx, tx, now, inst, "instance_reset_failed", model.LevelInfo,
			fmt.Sprintf("instance %s had its crash-loop window reset", inst.Name),
			nil, nil, &sink); err != nil {
			return err
		}

		row, err := s.store.InstanceView(ctx, tx, id)
		if err != nil {
			return err
		}
		out, err = s.viewOf(row, s.activeRuntime(ctx, tx))
		return err
	})
	if err != nil {
		return View{}, err
	}
	s.publish(&sink)
	return out, nil
}

// SetAutostart writes `instances.autostart` and NOTHING else.
//
// It never starts or stops, which is why it is not part of the config PATCH
// either: folding autostart into an edit would let a change to an unrelated
// field silently change what happens at the next boot. The unit-file enable or
// disable is the caller's, for the same reason the systemd ResetFailed above is,
// and it is best-effort: a host that withheld the `manage-unit-files` grant
// still records the user's intent, and the response carries the manual command.
//
// It deliberately does not bump `generation`: nobody edited a configuration, and
// `config_hash` does not include autostart — a unit that starts at boot renders
// the same argv as one that does not.
func (s *Service) SetAutostart(ctx context.Context, id string, enabled bool) (View, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	var (
		out  View
		sink events.Sink
	)
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		inst, err := s.store.Instance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.Deleted() {
			return store.ErrNotFound
		}
		if _, err := s.store.SetInstanceAutostart(ctx, tx, id, enabled, nowMS); err != nil {
			return err
		}

		from, to := boolWord(inst.Autostart), boolWord(enabled)
		if err := s.event(ctx, tx, now, inst, "instance_autostart", model.LevelInfo,
			fmt.Sprintf("instance %s will %sstart at boot", inst.Name,
				map[bool]string{true: "", false: "not "}[enabled]),
			&from, &to, &sink); err != nil {
			return err
		}

		row, err := s.store.InstanceView(ctx, tx, id)
		if err != nil {
			return err
		}
		out, err = s.viewOf(row, s.activeRuntime(ctx, tx))
		return err
	})
	if err != nil {
		return View{}, err
	}
	s.publish(&sink)
	return out, nil
}

func boolWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// DryRunParams is `POST /api/v1/instances/validate`'s input (section 3.10a).
type DryRunParams struct {
	// InstanceID scopes the check to an existing row's identity, so an EDIT is
	// not told its own ports are taken. Empty is a create.
	InstanceID string

	ModelID       string
	MmprojModelID *string
	DraftModelID  *string

	Flags      model.FlagSet
	ExtraFlags string
}

// DryRun is the dry run behind the form's argv preview and its draft check.
//
// It NEVER refuses. Section 3.10a is explicit: this endpoint "returns the same
// three-valued result as `{"draft_validation":"ok"|"deferred"|"mismatch"}` plus
// the warning, and never a 422 — it is a dry run". A form that could not render
// a mismatch would be a form that shows nothing at the exact moment the user
// needs to be told what is wrong, so every refusal the save path raises comes
// back here as a warning or as an empty argv with a reason beside it.
func (s *Service) DryRun(ctx context.Context, p DryRunParams) (View, error) {
	inst := model.Instance{
		ID:         p.InstanceID,
		Name:       "preview",
		ExtraFlags: p.ExtraFlags,
	}
	if p.ModelID != "" {
		inst.ModelID = &p.ModelID
	}
	inst.MmprojModelID = p.MmprojModelID
	inst.DraftModelID = p.DraftModelID

	var out View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if p.InstanceID != "" {
			// An edit keeps the row's identity — name and both ports — so that
			// nothing here reports the instance's own values as conflicts.
			if row, err := s.store.Instance(ctx, tx, p.InstanceID); err == nil {
				inst.Name = row.Name
				inst.PublicPort, inst.InternalPort = row.PublicPort, row.InternalPort
			}
		}

		refs, warnings, err := s.resolveAndValidate(ctx, tx, inst)
		if err != nil {
			// A resolution error is reported as a warning rather than raised:
			// the commonest one is "this model has not been downloaded yet",
			// which is a supported state the form must be able to show.
			var me model.Error
			if !errors.As(err, &me) {
				return err
			}
			warnings = append(warnings, model.Warning{
				Code: model.WarnDraftVocabUnverified, Message: me.Message,
			})
			refs = modelRefs{draftValidation: model.DraftDeferred}
		}

		active := s.activeRuntime(ctx, tx)
		out = View{
			InstanceView: model.InstanceView{Instance: inst},
			Flags:        p.Flags,
			Warnings:     warnings,
		}
		out.DraftValidation = refs.draftValidation
		if out.DraftValidation == "" {
			out.DraftValidation = model.DraftDeferred
		}
		out.ActiveVersionID = active.ID

		if !refs.renderable() || active.ID == "" {
			// No command line to show. Section 5.7's rule, and the same one
			// Get follows: a half-real argv is worse than none, because a user
			// will copy it.
			return nil
		}
		argv, err := RenderArgv(inst, p.Flags, refs.primary, refs.mmproj, refs.draft, active)
		if err != nil {
			var me model.Error
			if errors.As(err, &me) {
				out.Warnings = append(out.Warnings,
					model.Warning{Code: model.WarnUnknownFlags, Message: me.Message})
				return nil
			}
			return err
		}
		out.Argv = argv
		out.UnknownFlags = UnknownFlags(argv, active.Help)
		out.Warnings = append(out.Warnings, flagWarnings(p.Flags, active, out.UnknownFlags)...)
		return nil
	})
	return out, err
}
