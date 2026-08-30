package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The two adapters the instance service needs from layers it does not own
// (DESIGN section 1: the consumer owns the interface, the composition root
// supplies the implementation).

// modelResolver satisfies instances.Resolver over the store.
//
// It is the composition root's rather than internal/models' deliberately.
// `instances.Resolver` asks two questions — which models does this instance
// reference, and which llama.cpp build is active — and both are one indexed
// read of a table the store already projects. A resolver that waited for the
// models service would leave the five instance endpoints of section 3.10
// answering 503 for a fact the database already holds, which is exactly the D43
// violation of documenting a response the binary cannot produce.
type modelResolver struct {
	st *store.Store
	// versionsDir is `<state_dir>/versions` (section 6.1). The active build's
	// directory is resolved from the row's `dir_name` rather than from the
	// `versions/active` symlink, so rendering an argv never depends on a
	// filesystem read — the renderer is pure, and section 5.7 says so.
	versionsDir string
}

// Models projects `models` rows into what the renderer and D34's draft check
// need. A missing id is absent from the map rather than an error.
func (r modelResolver) Models(ctx context.Context, tx store.Tx, ids []string) (
	map[string]instances.ModelInfo, error) {

	rows, err := r.st.ModelRefsByID(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]instances.ModelInfo, len(rows))
	for id, row := range rows {
		info := instances.ModelInfo{
			ID:             row.ID,
			Kind:           row.Kind,
			State:          row.State,
			Parsed:         row.Parsed,
			TokenizerModel: row.TokenizerModel,
			NVocab:         row.NVocab,
		}
		// A path is offered only for a model whose bytes are actually on disk.
		// Every other state — `planned`, `downloading`, `missing`, `corrupt` —
		// is a supported configuration this design deliberately allows an
		// instance to be created against, and an empty path is how the service
		// renders no argv and warns instead of printing a command line that
		// would fail at exec.
		if row.State == model.ModelReady {
			info.Path = filepath.Join(row.SnapshotDir, row.PrimaryFile)
		}
		out[id] = info
	}
	return out, nil
}

// ActiveRuntime returns the `is_active=1` build. No active build is an ordinary
// state on a fresh install and is reported as instances.ErrNoActiveRuntime, not
// as a failure: llama.cpp activation recomputes every instance's `config_hash`
// when it happens (D69).
func (r modelResolver) ActiveRuntime(ctx context.Context, tx store.Tx) (instances.Runtime, error) {
	active, err := r.st.ActiveVersion(ctx, tx)
	if errors.Is(err, store.ErrNotFound) {
		return instances.Runtime{}, instances.ErrNoActiveRuntime
	}
	if err != nil {
		return instances.Runtime{}, err
	}
	rt := instances.Runtime{
		ID:          active.ID,
		Dir:         filepath.Join(r.versionsDir, active.DirName),
		SupportsFit: active.SupportsFit,
	}
	if active.HelpJSON != nil {
		// A capture that will not parse leaves the flag-churn guard
		// unavailable, which is what a missing capture already means (section
		// 5.7). It is never a reason to refuse to render.
		_ = json.Unmarshal([]byte(*active.HelpJSON), &rt.Help)
	}
	return rt, nil
}

// deactivator satisfies instances.Deactivator: section 3.10c step 1's stop,
// disable and reset-failed, each individually gated and none of them a reason to
// fail a delete.
//
// Every call is best effort by design. Two supported installs cannot make them
// at all — `--no-autostart-grant` and `systemd_control='unavailable'` — and the
// safety net that makes this sound is already in place: an enabled unit for a
// deleted instance starts `instance-exec`, which finds `deleted_at` set and
// exits 64 without launching anything.
type deactivator struct {
	control         systemd.Controller
	manageUnitFiles bool
}

func (d deactivator) DeactivateInstance(ctx context.Context, inst model.Instance) ([]string, error) {
	unit := instances.UnitName(inst.Name)
	var hints []string

	if d.control == nil {
		// F10: all three calls are skipped, the row is soft-deleted anyway, and
		// the notification names what to do by hand (section 3.10c).
		if inst.Autostart {
			hints = append(hints, instances.DisableCommand(inst.Name))
		}
		return hints, nil
	}

	// The stop is asked for here; the supervisor is the only writer allowed to
	// close the open `instance_starts` row and does so from this very stop,
	// which is why the reconcile set carries the open-row term (section 5.8).
	//
	// A unit the manager does not know is the ordinary case rather than a
	// failure: an instance that has never been started has no loaded unit, and
	// a hint telling the user to stop something that is not running would be
	// noise on the commonest delete there is.
	if _, err := d.control.Stop(ctx, unit); err != nil && !errors.Is(err, systemd.ErrNoSuchUnit) {
		hints = append(hints, "sudo systemctl stop "+unit)
	}

	switch {
	case !d.manageUnitFiles:
		// `unit_still_enabled` with the manual command, rather than a polkit
		// denial the user has to interpret (section 11.1a).
		if inst.Autostart {
			hints = append(hints, instances.DisableCommand(inst.Name))
		}
	default:
		if err := d.control.Disable(ctx, []string{unit}); err != nil &&
			!errors.Is(err, systemd.ErrNoSuchUnit) {
			hints = append(hints, instances.DisableCommand(inst.Name))
		}
	}

	// A unit left in `failed` for an instance nobody can see again is noise in
	// `systemctl --failed` forever; clearing it is the third of section 3.10c's
	// three calls and its failure is not worth a hint.
	_ = d.control.ResetFailed(ctx, unit)
	return hints, nil
}
