package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The instance service (DESIGN sections 2.8, 3.10, 3.10a, 3.10c).
//
// Handlers never write domain state directly. Every mutation below performs its
// guarded transition inside ONE transaction, emits an `events` row in that same
// transaction, and publishes the SSE frame only after it commits — the seam
// DESIGN section 1 describes and internal/events.Recorder implements.
//
// The writer discipline of section 2.8 is what shapes this file:
//
//   - The `instance_status` row is inserted in the SAME transaction as the
//     `instances` row. It has three NOT NULL columns and no defaults for two of
//     them, so it cannot spring into existence lazily, and every reader may
//     therefore use an inner join.
//   - Deletion is SOFT (D68). The rows and the accounting survive; `name` and
//     both ports are reusable immediately; `?purge=true` is the explicit hard
//     delete.
//   - `config_hash` is stored, not computed on read, and has exactly three
//     writers: POST, PATCH and RecomputeConfigHash (D69).

// Store is the persistence this package needs. *store.Store satisfies it
// structurally — DESIGN section 1, invariant 1: only internal/store contains
// SQL, so every other package declares the repository interface it needs.
type Store interface {
	InsertInstance(ctx context.Context, tx store.Tx, i model.Instance) error
	Instance(ctx context.Context, tx store.Tx, id string) (model.Instance, error)
	InstanceByName(ctx context.Context, tx store.Tx, name string) (model.Instance, error)
	InstanceView(ctx context.Context, tx store.Tx, id string) (model.InstanceView, error)
	InstanceViews(ctx context.Context, tx store.Tx, f store.InstanceFilter) ([]model.InstanceView, error)
	UpdateInstanceConfig(ctx context.Context, tx store.Tx, i model.Instance) (bool, error)
	SetInstanceDesiredState(ctx context.Context, tx store.Tx, id string, desired model.DesiredState, at int64) (bool, error)
	StampPendingStart(ctx context.Context, tx store.Tx, id string, trigger model.PendingTrigger, overrideJSON *string, at int64) (bool, error)
	SetInstanceConfigHash(ctx context.Context, tx store.Tx, id, hash string, at int64) (bool, error)
	SoftDeleteInstance(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	PurgeInstance(ctx context.Context, tx store.Tx, id string) (bool, error)
	DeleteTokenInstances(ctx context.Context, tx store.Tx, instanceID string) error
	InstancePortHolders(ctx context.Context, tx store.Tx) ([]model.InstancePorts, error)

	InsertInstanceStatus(ctx context.Context, tx store.Tx, st model.InstanceStatus) error
	ClearCrashLoopLatch(ctx context.Context, tx store.Tx, instanceID string, now int64) (bool, error)

	InstanceStarts(ctx context.Context, tx store.Tx, instanceID string, limit int) ([]model.InstanceStart, error)

	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
}

// Settings is the typed settings this service reads: the internal-port pool and
// the two bind addresses of section 2.8's port rules.
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
	GetString(ctx context.Context, key string) (string, error)
}

// RuntimeFacts answers what `runtime_info` knows: the port the management walk
// actually landed on, which is excluded from every public port just as
// `ui.port_desired` is.
type RuntimeFacts interface {
	RuntimeInfo(ctx context.Context, tx store.Tx) (model.RuntimeInfo, error)
}

// ModelInfo is a `models` row as this service reads it: one resolved path for
// the renderer, and the two GGUF fields D34's draft check needs.
type ModelInfo struct {
	ID string
	// Path is the resolved primary file (shard 1 for a sharded set), or EMPTY
	// when the model has not been downloaded yet. An empty path is a supported
	// state, not an error: this design deliberately allows configuring an
	// instance against a model that is still `planned` or `downloading`.
	Path  string
	Kind  model.ModelKind
	State model.ModelState

	// Parsed is `gguf_parsed_at IS NOT NULL`. The two fields below exist only
	// after a parse.
	Parsed         bool
	TokenizerModel *string
	NVocab         *int64
}

// Meta projects the GGUF half for ValidateDraft.
func (m ModelInfo) Meta() ModelMeta {
	return ModelMeta{ID: m.ID, Parsed: m.Parsed, TokenizerModel: m.TokenizerModel, NVocab: m.NVocab}
}

// Resolver supplies the two things the renderers need that this package does
// not own: the models an instance references and the active llama.cpp build.
// internal/models and internal/llamacpp satisfy it; the interface is declared
// here because the consumer owns it.
type Resolver interface {
	// Models returns one entry per id that exists. A missing id is simply
	// absent from the map, which is how a reference to a deleted model is
	// reported without an error type.
	Models(ctx context.Context, tx store.Tx, ids []string) (map[string]ModelInfo, error)
	// ActiveRuntime returns the `is_active=1` version. ErrNoActiveRuntime means
	// no build is active yet, which is an ordinary state on a fresh install and
	// is not a reason to refuse an instance: llama.cpp activation recomputes
	// every instance's `config_hash` when it happens (D69).
	ActiveRuntime(ctx context.Context, tx store.Tx) (Runtime, error)
}

// ErrNoActiveRuntime is what Resolver.ActiveRuntime returns when no llama.cpp
// build is active.
var ErrNoActiveRuntime = errors.New("instances: no llama.cpp version is active")

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Deactivator is what a delete needs from the layers this phase does not own:
// stop the unit, disable it, reset its failed state and close the gateway
// listener (section 3.10c step 1-2).
//
// It is optional, and its absence is not a failure. Two supported installs
// cannot make those calls at all — `--no-autostart-grant`, and
// `systemd_control='unavailable'` — so every call is best-effort and a skipped
// one raises the `unit_still_enabled` hint carrying the exact manual command
// instead of failing the delete. The safety net that makes this sound is
// already in place: an enabled unit for a deleted instance starts
// `instance-exec`, which finds `deleted_at` set and exits 64 without launching
// anything.
type Deactivator interface {
	// DeactivateInstance returns the hints for calls it could not make.
	DeactivateInstance(ctx context.Context, inst model.Instance) ([]string, error)
}

// Config wires a Service.
type Config struct {
	Store    Store
	Settings Settings
	Resolver Resolver
	Runtime  RuntimeFacts
	Events   Events
	// Deactivator is optional; nil means every delete reports the manual
	// disable command as a hint.
	Deactivator Deactivator
	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Probe is the advisory bind check. Nil uses LiveProbe; a test supplies its
	// own so the port rules can be exercised without binding.
	Probe Prober
}

// Service is the instance service.
type Service struct {
	store    Store
	settings Settings
	resolver Resolver
	runtime  RuntimeFacts
	events   Events
	deact    Deactivator
	now      func() time.Time
	newID    func(time.Time) string
	probe    Prober
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("instances: a store is required")
	}
	if cfg.Settings == nil {
		return nil, fmt.Errorf("instances: a settings source is required")
	}
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("instances: a resolver is required")
	}
	s := &Service{
		store:    cfg.Store,
		settings: cfg.Settings,
		resolver: cfg.Resolver,
		runtime:  cfg.Runtime,
		events:   cfg.Events,
		deact:    cfg.Deactivator,
		now:      cfg.Now,
		newID:    cfg.NewID,
		probe:    cfg.Probe,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	if s.probe == nil {
		s.probe = LiveProbe
	}
	return s, nil
}

// View is one instance as the API returns it: the joined row, its parsed
// FlagSet, and the four derived flags computed against the active build.
type View struct {
	model.InstanceView
	Flags   model.FlagSet
	Derived model.DerivedFlags
	// ActiveVersionID is the build the derived flags were computed against, so
	// a caller rendering `stale_version` can name the version to restart onto.
	ActiveVersionID string
	// Argv is the rendered command line, present only when everything it needs
	// is resolved — a model that is still downloading, or a host with no active
	// build, has no command line to show and showing a half-real one would be
	// worse than showing none.
	Argv []string
	// UnknownFlags is section 5.7's flag-churn guard against the active build's
	// help capture. Empty also means "the check was unavailable"; Warnings says
	// which.
	UnknownFlags []string
	// Starts is the recent ledger, on the detail view only (§3.10: "the last 5
	// starts with their outcomes").
	Starts   []model.InstanceStart
	Warnings []model.Warning
}

// DetailStarts is how many ledger rows `GET /instances/{id}` carries.
const DetailStarts = 5

// List is `GET /api/v1/instances`: config ⋈ status with the four derived flags.
// Soft-deleted instances are excluded unless includeDeleted (D68).
func (s *Service) List(ctx context.Context, includeDeleted bool) ([]View, error) {
	var out []View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{IncludeDeleted: includeDeleted})
		if err != nil {
			return err
		}
		active := s.activeRuntime(ctx, tx)
		out = make([]View, 0, len(rows))
		for _, row := range rows {
			v, err := s.viewOf(row, active)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// Get is `GET /api/v1/instances/{id}`: the detail, including the rendered argv
// and the last five starts.
func (s *Service) Get(ctx context.Context, id string) (View, error) {
	var out View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := s.store.InstanceView(ctx, tx, id)
		if err != nil {
			return err
		}
		active := s.activeRuntime(ctx, tx)
		v, err := s.viewOf(row, active)
		if err != nil {
			return err
		}
		if v.Starts, err = s.store.InstanceStarts(ctx, tx, id, DetailStarts); err != nil {
			return err
		}

		// The rendered argv, and the flag-churn warnings that go with it. Both
		// need the models resolved, so both are absent for an instance whose
		// download has not finished — which is a supported state, not an error.
		refs, err := s.resolveRefs(ctx, tx, row.Instance)
		if err != nil {
			return err
		}
		if refs.renderable() && active.ID != "" {
			argv, err := RenderArgv(row.Instance, v.Flags, refs.primary, refs.mmproj, refs.draft, active)
			if err != nil {
				return err
			}
			v.Argv = argv
			v.UnknownFlags = UnknownFlags(argv, active.Help)
			v.Warnings = append(v.Warnings, flagWarnings(v.Flags, active, v.UnknownFlags)...)
		}
		out = v
		return nil
	})
	return out, err
}

// CreateParams is the body of `POST /api/v1/instances`. Nil pointers are
// "unset", which for a create means "use the default this design names" and for
// a PATCH means "leave it alone".
type CreateParams struct {
	Name        string
	DisplayName *string
	Description *string

	ModelID       string
	MmprojModelID *string
	DraftModelID  *string

	// PublicPort and InternalPort are auto-allocated when nil (§3.10).
	PublicPort   *int
	InternalPort *int

	AuthMode         *model.AuthMode
	Autostart        *bool
	RestartPolicy    *model.RestartPolicy
	RestartMax       *int
	RestartWindowSec *int

	Flags      model.FlagSet
	ExtraFlags string
}

// Create is `POST /api/v1/instances`.
//
// One transaction writes the config row, the status row and the event. The
// status row is not an afterthought: §2.8 makes creating it here the reason
// every reader may assume it exists.
func (s *Service) Create(ctx context.Context, p CreateParams) (View, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	if err := ValidateName(p.Name); err != nil {
		return View{}, err
	}
	if err := ValidateFlags(p.Flags); err != nil {
		return View{}, err
	}
	if _, err := ParseExtraFlags(p.ExtraFlags); err != nil {
		return View{}, err
	}
	if p.ModelID == "" {
		return View{}, model.Error{
			Code:    model.CodeModelMissing,
			Message: "an instance needs a model to serve",
		}
	}

	flagsJSON, err := json.Marshal(p.Flags)
	if err != nil {
		return View{}, fmt.Errorf("encode flags: %w", err)
	}

	var out View
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.InstanceByName(ctx, tx, p.Name); err == nil {
			return model.Error{
				Code:    model.CodeInstanceNameTaken,
				Message: fmt.Sprintf("another instance is already called %q", p.Name),
				Details: map[string]any{"name": p.Name},
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		inst := model.Instance{
			ID:               s.newID(now),
			Name:             p.Name,
			DisplayName:      p.DisplayName,
			Description:      p.Description,
			ModelID:          &p.ModelID,
			MmprojModelID:    p.MmprojModelID,
			DraftModelID:     p.DraftModelID,
			AuthMode:         valueOr(p.AuthMode, model.AuthToken),
			Autostart:        valueOr(p.Autostart, false),
			RestartPolicy:    valueOr(p.RestartPolicy, model.RestartOnFailure),
			RestartMax:       valueOr(p.RestartMax, DefaultRestartMax),
			RestartWindowSec: valueOr(p.RestartWindowSec, DefaultRestartWindowSec),
			FlagsJSON:        string(flagsJSON),
			ExtraFlags:       p.ExtraFlags,
			DesiredState:     model.DesiredStopped,
			DraftValidation:  model.DraftOK,
			UnitName:         UnitName(p.Name),
			Generation:       1,
			CreatedAt:        nowMS,
			UpdatedAt:        nowMS,
		}

		policy, holders, err := s.portContext(ctx, tx)
		if err != nil {
			return err
		}
		if inst.PublicPort, err = s.choosePort(PortPublic, p.PublicPort, policy, holders, ""); err != nil {
			return err
		}
		if inst.InternalPort, err = s.chooseInternalPort(p.InternalPort, policy, holders, "",
			inst.PublicPort); err != nil {
			return err
		}

		refs, warnings, err := s.resolveAndValidate(ctx, tx, inst)
		if err != nil {
			return err
		}
		inst.DraftValidation = refs.draftValidation

		active := s.activeRuntime(ctx, tx)
		if inst.ConfigHash, err = s.hash(inst, p.Flags, refs, active); err != nil {
			return err
		}

		if err := s.store.InsertInstance(ctx, tx, inst); err != nil {
			return err
		}
		if err := s.store.InsertInstanceStatus(ctx, tx, model.InstanceStatus{
			InstanceID:           inst.ID,
			State:                model.InstanceUnknown,
			LastChangeAt:         nowMS,
			GPUAttribution:       model.AttributionUnknown,
			RestartWindowResetAt: 0,
		}); err != nil {
			return err
		}
		if err := s.event(ctx, tx, now, inst, "instance_created", model.LevelInfo,
			fmt.Sprintf("instance %s created", inst.Name), nil, ptrTo(string(model.InstanceUnknown))); err != nil {
			return err
		}

		row, err := s.store.InstanceView(ctx, tx, inst.ID)
		if err != nil {
			return err
		}
		if out, err = s.viewOf(row, active); err != nil {
			return err
		}
		out.Warnings = warnings
		return nil
	})
	if err != nil {
		return View{}, err
	}
	s.publish(out.Instance, "instance_created", now)
	return out, nil
}

// The defaults section 2.8's schema declares, restated here because a create
// that omits a field must produce the same row the column default would.
const (
	DefaultRestartMax       = 5
	DefaultRestartWindowSec = 600
)

// PatchParams is the body of `PATCH /api/v1/instances/{id}`: partial, plus the
// `generation` the client read.
//
// `autostart` and `desired_state` are deliberately absent. Autostart is
// `PUT /instances/{id}/autostart`, which only enables or disables the unit;
// desired state is the start/stop endpoints'. Folding either into a config
// PATCH would make editing an unrelated field silently change what happens at
// the next boot.
type PatchParams struct {
	Generation int64

	Name        *string
	DisplayName **string
	Description **string

	ModelID       *string
	MmprojModelID **string
	DraftModelID  **string

	PublicPort   *int
	InternalPort *int

	AuthMode         *model.AuthMode
	RestartPolicy    *model.RestartPolicy
	RestartMax       *int
	RestartWindowSec *int

	Flags      *model.FlagSet
	ExtraFlags *string
}

// Patch is `PATCH /api/v1/instances/{id}`.
//
// The generation guard is the whole reason this is one statement rather than a
// read followed by a write: `UpdateInstanceConfig` matches only while the
// generation is still the one the client read, so two concurrent edits cannot
// interleave into a row neither admin asked for.
func (s *Service) Patch(ctx context.Context, id string, p PatchParams) (View, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	var out View
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		inst, err := s.store.Instance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.Deleted() {
			return model.Error{
				Code:    model.CodeConflictGeneration,
				Message: "this instance has been deleted",
			}
		}
		if inst.Generation != p.Generation {
			return conflictGeneration(inst.Generation, p.Generation)
		}

		flags, err := model.ParseFlagSet([]byte(inst.FlagsJSON))
		if err != nil {
			return model.Error{Code: model.CodeBadFlags, Message: err.Error()}
		}

		if p.Name != nil && *p.Name != inst.Name {
			if err := ValidateName(*p.Name); err != nil {
				return err
			}
			if other, err := s.store.InstanceByName(ctx, tx, *p.Name); err == nil && other.ID != id {
				return model.Error{
					Code:    model.CodeInstanceNameTaken,
					Message: fmt.Sprintf("another instance is already called %q", *p.Name),
					Details: map[string]any{"name": *p.Name},
				}
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			inst.Name = *p.Name
			inst.UnitName = UnitName(*p.Name)
		}
		if p.DisplayName != nil {
			inst.DisplayName = *p.DisplayName
		}
		if p.Description != nil {
			inst.Description = *p.Description
		}
		if p.ModelID != nil {
			if *p.ModelID == "" {
				return model.Error{
					Code:    model.CodeModelMissing,
					Message: "an instance needs a model to serve",
				}
			}
			inst.ModelID = p.ModelID
		}
		if p.MmprojModelID != nil {
			inst.MmprojModelID = *p.MmprojModelID
		}
		if p.DraftModelID != nil {
			inst.DraftModelID = *p.DraftModelID
		}
		if p.AuthMode != nil {
			if !p.AuthMode.Valid() {
				return model.Error{
					Code:    model.CodeBadFlags,
					Message: fmt.Sprintf("auth_mode %q is not one of token, none", *p.AuthMode),
				}
			}
			inst.AuthMode = *p.AuthMode
		}
		if p.RestartPolicy != nil {
			if !p.RestartPolicy.Valid() {
				return model.Error{
					Code: model.CodeBadFlags,
					Message: fmt.Sprintf("restart_policy %q is not one of always, on-failure, never",
						*p.RestartPolicy),
				}
			}
			inst.RestartPolicy = *p.RestartPolicy
		}
		if p.RestartMax != nil {
			inst.RestartMax = *p.RestartMax
		}
		if p.RestartWindowSec != nil {
			inst.RestartWindowSec = *p.RestartWindowSec
		}
		if p.ExtraFlags != nil {
			if _, err := ParseExtraFlags(*p.ExtraFlags); err != nil {
				return err
			}
			inst.ExtraFlags = *p.ExtraFlags
		}
		if p.Flags != nil {
			if err := ValidateFlags(*p.Flags); err != nil {
				return err
			}
			flags = *p.Flags
			encoded, err := json.Marshal(flags)
			if err != nil {
				return fmt.Errorf("encode flags: %w", err)
			}
			inst.FlagsJSON = string(encoded)
		}

		if p.PublicPort != nil || p.InternalPort != nil {
			policy, holders, err := s.portContext(ctx, tx)
			if err != nil {
				return err
			}
			if p.PublicPort != nil {
				if err := ValidatePort(PortPublic, *p.PublicPort, policy, holders, id, s.probe); err != nil {
					return err
				}
				inst.PublicPort = *p.PublicPort
			}
			if p.InternalPort != nil {
				if err := ValidatePort(PortInternal, *p.InternalPort, policy, holders, id, s.probe); err != nil {
					return err
				}
				inst.InternalPort = *p.InternalPort
			}
			if inst.PublicPort == inst.InternalPort {
				return model.Error{
					Code:    model.CodePortUnavailable,
					Message: "the public and internal ports must differ",
					Details: map[string]any{
						"port":   inst.PublicPort,
						"reason": string(model.PortInUseByInstance),
					},
				}
			}
		}

		refs, warnings, err := s.resolveAndValidate(ctx, tx, inst)
		if err != nil {
			return err
		}
		inst.DraftValidation = refs.draftValidation

		active := s.activeRuntime(ctx, tx)
		if inst.ConfigHash, err = s.hash(inst, flags, refs, active); err != nil {
			return err
		}
		inst.UpdatedAt = nowMS

		changed, err := s.store.UpdateInstanceConfig(ctx, tx, inst)
		if err != nil {
			return err
		}
		if !changed {
			// The row moved between the read and the write: another admin
			// committed an edit inside this transaction's window. Re-read it so
			// the 409 names the generation that is actually current rather than
			// one this handler guessed at.
			current, readErr := s.store.Instance(ctx, tx, id)
			if readErr != nil {
				return readErr
			}
			return conflictGeneration(current.Generation, p.Generation)
		}
		if err := s.event(ctx, tx, now, inst, "instance_updated", model.LevelInfo,
			fmt.Sprintf("instance %s updated", inst.Name), nil, nil); err != nil {
			return err
		}

		row, err := s.store.InstanceView(ctx, tx, id)
		if err != nil {
			return err
		}
		if out, err = s.viewOf(row, active); err != nil {
			return err
		}
		out.Warnings = warnings
		return nil
	})
	if err != nil {
		return View{}, err
	}
	s.publish(out.Instance, "instance_updated", now)
	return out, nil
}

// DeleteParams is the query half of `DELETE /api/v1/instances/{id}`.
type DeleteParams struct {
	// Purge is `?purge=true`: the explicit HARD delete, which cascades
	// `instance_status`, `instance_starts`, both usage tables, the denial
	// counters and `token_instances` away. That history is the one thing in
	// this system that cannot be recomputed, which is why the UI puts it behind
	// a second confirmation.
	Purge bool
	// KeepTokens is `?keep_tokens=true`: leave `token_instances` alone. The
	// default removes them, since a scope entry for an instance nobody can
	// reach is noise.
	KeepTokens bool
}

// DeleteResult is what the handler answers with.
type DeleteResult struct {
	Purged bool
	// Hints carries the manual commands for anything the daemon could not do
	// itself — the `unit_still_enabled` case of section 3.10c step 1.
	Hints []string
}

// Delete is `DELETE /api/v1/instances/{id}`: soft by default, hard on request
// (D68, section 3.10c).
//
// The order matters. `desired_state='stopped'` and the systemd calls come
// first, so the supervisor's own stop is what closes the open ledger row — this
// handler deliberately does NOT close it, which would make an API handler the
// third writer of a single-shot column and would race the supervisor for the
// same row.
func (s *Service) Delete(ctx context.Context, id string, p DeleteParams) (DeleteResult, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	var (
		inst   model.Instance
		result DeleteResult
	)
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		inst, err = s.store.Instance(ctx, tx, id)
		return err
	}); err != nil {
		return DeleteResult{}, err
	}

	// Deleting an already-deleted instance is a no-op rather than an error or a
	// second event: the row is already stopped, disabled and stamped, and a
	// retried request — a double click, a client replay — must not append a
	// second "deleted" line to its history. A PURGE still proceeds, because
	// discarding the history is a different operation from deleting the
	// instance.
	if inst.Deleted() && !p.Purge {
		return result, nil
	}

	// Step 1: stop, disable and reset — best effort, individually gated, and
	// never a reason to fail the delete.
	if s.deact != nil {
		hints, err := s.deact.DeactivateInstance(ctx, inst)
		if err != nil {
			hints = append(hints, DisableCommand(inst.Name))
		}
		result.Hints = hints
	} else if inst.Autostart {
		result.Hints = append(result.Hints, DisableCommand(inst.Name))
	}

	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if p.Purge {
			if _, err := s.store.PurgeInstance(ctx, tx, id); err != nil {
				return err
			}
			result.Purged = true
			// The event outlives the row it describes, which is the point: a
			// purge is the one operation whose subject cannot be looked up
			// afterwards.
			return s.event(ctx, tx, now, inst, "instance_purged", model.LevelWarn,
				fmt.Sprintf("instance %s purged with all of its history", inst.Name), nil, nil)
		}

		if _, err := s.store.SetInstanceDesiredState(ctx, tx, id, model.DesiredStopped, nowMS); err != nil {
			return err
		}
		if !p.KeepTokens {
			if err := s.store.DeleteTokenInstances(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := s.store.SoftDeleteInstance(ctx, tx, id, nowMS); err != nil {
			return err
		}
		return s.event(ctx, tx, now, inst, "instance_deleted", model.LevelInfo,
			fmt.Sprintf("instance %s deleted; its history and accounting are kept", inst.Name),
			nil, nil)
	})
	if err != nil {
		return DeleteResult{}, err
	}

	action := "instance_deleted"
	if result.Purged {
		action = "instance_purged"
	}
	s.publish(inst, action, now)
	return result, nil
}

// SetDesiredState is the desired-state API the supervisor reconciles against:
// `POST /instances/{id}/start`, `/stop` and `/restart` all land here.
//
// It writes the DESIRED axis and — for a start — stamps the trigger the daemon
// is about to start for, in one transaction. It does NOT call systemd: the
// supervisor owns that, reads `(desired, actual)` on its next pass and takes at
// most one corrective action. Splitting it this way is what makes an instance
// that crashed while the daemon was down get restarted when the daemon returns.
//
// The two response hints section 2.8 attaches to these endpoints —
// `will_start_at_boot` when stopping an instance with `autostart=1`, and
// `start_now` when enabling autostart on a stopped one — belong to the handlers
// that expose start/stop and `PUT …/autostart`, and arrive with them. Both are
// derivable from the returned View (`autostart` beside `desired_state`), which
// is why nothing is lost by leaving them to the layer that renders a response.
func (s *Service) SetDesiredState(ctx context.Context, id string,
	desired model.DesiredState, trigger model.PendingTrigger) (View, error) {
	now := s.now()
	nowMS := now.UnixMilli()

	if !desired.Valid() {
		return View{}, model.Error{
			Code:    model.CodeBadFlags,
			Message: fmt.Sprintf("desired_state %q is not one of running, stopped", desired),
		}
	}

	var out View
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		inst, err := s.store.Instance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.Deleted() {
			return store.ErrNotFound
		}
		if _, err := s.store.SetInstanceDesiredState(ctx, tx, id, desired, nowMS); err != nil {
			return err
		}
		if desired == model.DesiredRunning {
			if !trigger.Valid() {
				trigger = model.TriggerUser
			}
			if _, err := s.store.StampPendingStart(ctx, tx, id, trigger, nil, nowMS); err != nil {
				return err
			}
		}
		to := string(desired)
		if err := s.event(ctx, tx, now, inst, "instance_desired_state", model.LevelInfo,
			fmt.Sprintf("instance %s is wanted %s", inst.Name, desired),
			ptrTo(string(inst.DesiredState)), &to); err != nil {
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
	s.publish(out.Instance, "instance_desired_state", now)
	return out, nil
}

// RecomputeConfigHash is D69's ONE method: re-render argv for each instance,
// recompute the hash, and write it.
//
// It takes the CALLER's transaction, because both of its callers need the write
// to land in a transaction they already own — llama.cpp activation recomputes
// for every non-deleted instance in the same transaction that sets `is_active`
// (§6.6 step 3), and the models service does it in the transaction that changes
// a model's resolved path. An empty id list means every non-deleted instance.
//
// It touches neither `generation` nor `applied_config_hash`: nobody edited a
// configuration, and leaving the applied hash alone is exactly what makes
// `restart_required` light up for every running instance the moment a new build
// is activated.
func (s *Service) RecomputeConfigHash(ctx context.Context, tx store.Tx, ids ...string) error {
	nowMS := s.now().UnixMilli()

	rows, err := s.store.InstanceViews(ctx, tx, store.InstanceFilter{IDs: ids})
	if err != nil {
		return err
	}
	active := s.activeRuntime(ctx, tx)

	for _, row := range rows {
		flags, err := model.ParseFlagSet([]byte(row.FlagsJSON))
		if err != nil {
			// A row whose flags no longer parse cannot be re-rendered, and a
			// version activation must not fail because one instance carries a
			// bad configuration: its hash simply stays where it was, and the
			// launcher will refuse it with exit 65 when someone starts it.
			continue
		}
		refs, err := s.resolveRefs(ctx, tx, row.Instance)
		if err != nil {
			return err
		}
		hash, err := s.hash(row.Instance, flags, refs, active)
		if err != nil {
			continue
		}
		if hash == row.ConfigHash {
			continue
		}
		if _, err := s.store.SetInstanceConfigHash(ctx, tx, row.ID, hash, nowMS); err != nil {
			return err
		}
	}
	return nil
}

// SuggestPort is `GET /api/v1/ports/suggest`.
func (s *Service) SuggestPort(ctx context.Context, kind PortKind) (int, error) {
	if !kind.Valid() {
		return 0, model.Error{
			Code:    model.CodePortUnavailable,
			Message: fmt.Sprintf("kind %q is not one of public, internal", kind),
		}
	}
	var port int
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		policy, holders, err := s.portContext(ctx, tx)
		if err != nil {
			return err
		}
		port, err = SuggestPort(kind, policy, holders, s.probe)
		return err
	})
	return port, err
}

// modelRefs is what one instance's three model references resolve to.
type modelRefs struct {
	primary, mmproj, draft *ModelFile
	draftValidation        model.DraftValidation
}

// renderable reports whether every referenced model has a resolved file, which
// is what RenderArgv needs. An instance configured against a model that is
// still downloading is not renderable YET, and that is a supported state.
func (r modelRefs) renderable() bool {
	return r.primary != nil && r.primary.Path != ""
}

func (s *Service) resolveRefs(ctx context.Context, tx store.Tx, inst model.Instance) (modelRefs, error) {
	ids := make([]string, 0, 3)
	for _, id := range []*string{inst.ModelID, inst.MmprojModelID, inst.DraftModelID} {
		if id != nil && *id != "" {
			ids = append(ids, *id)
		}
	}
	refs := modelRefs{draftValidation: inst.DraftValidation}
	if len(ids) == 0 {
		return refs, nil
	}

	found, err := s.resolver.Models(ctx, tx, ids)
	if err != nil {
		return modelRefs{}, err
	}
	pick := func(id *string) *ModelFile {
		if id == nil || *id == "" {
			return nil
		}
		info, ok := found[*id]
		if !ok {
			return nil
		}
		return &ModelFile{ID: info.ID, Path: info.Path}
	}
	refs.primary = pick(inst.ModelID)
	refs.mmproj = pick(inst.MmprojModelID)
	refs.draft = pick(inst.DraftModelID)
	return refs, nil
}

// resolveAndValidate resolves the model references, refuses one that names no
// row, and runs D34's three-valued draft check.
func (s *Service) resolveAndValidate(ctx context.Context, tx store.Tx, inst model.Instance) (modelRefs, []model.Warning, error) {
	ids := make([]string, 0, 3)
	for _, id := range []*string{inst.ModelID, inst.MmprojModelID, inst.DraftModelID} {
		if id != nil && *id != "" {
			ids = append(ids, *id)
		}
	}
	found, err := s.resolver.Models(ctx, tx, ids)
	if err != nil {
		return modelRefs{}, nil, err
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			return modelRefs{}, nil, model.Error{
				Code:    model.CodeModelMissing,
				Message: fmt.Sprintf("no model %s exists", id),
				Details: map[string]any{"model_id": id},
			}
		}
	}

	refs, err := s.resolveRefs(ctx, tx, inst)
	if err != nil {
		return modelRefs{}, nil, err
	}

	var (
		pair     DraftPair
		warnings []model.Warning
	)
	if inst.ModelID != nil {
		pair.Primary = found[*inst.ModelID].Meta()
	}
	if inst.DraftModelID != nil && *inst.DraftModelID != "" {
		pair.Draft = found[*inst.DraftModelID].Meta()
	}
	validation, warn, err := ValidateDraft(pair)
	if err != nil {
		return modelRefs{}, nil, err
	}
	if warn != nil {
		warnings = append(warnings, *warn)
	}
	refs.draftValidation = validation
	return refs, warnings, nil
}

// hash computes `config_hash` for a configuration that may not be renderable
// yet.
//
// A model that has not finished downloading has no path, and RenderArgv
// refuses without one — but "queue the download, configure the instance while
// it runs" is a flow this design calls out explicitly, so the save cannot fail.
// The unresolved reference is therefore hashed under a deterministic
// PLACEHOLDER derived from its model id. Two different configurations still
// hash differently, and the moment the download completes the models service
// recomputes the row through this same method (D69) — which is precisely the
// event that turns the placeholder into the real path.
func (s *Service) hash(inst model.Instance, flags model.FlagSet, refs modelRefs, active Runtime) (string, error) {
	primary, mmproj, draft := refs.primary, refs.mmproj, refs.draft
	if primary == nil && inst.ModelID != nil {
		primary = &ModelFile{ID: *inst.ModelID}
	}
	primary = withPlaceholder(primary)
	mmproj = withPlaceholder(mmproj)
	draft = withPlaceholder(draft)

	return ConfigHashFor(inst, flags, primary, mmproj, draft, active)
}

// withPlaceholder gives an unresolved model reference a stable stand-in path.
func withPlaceholder(m *ModelFile) *ModelFile {
	if m == nil || m.Path != "" {
		return m
	}
	return &ModelFile{ID: m.ID, Path: "model-pending:" + m.ID}
}

// viewOf assembles the API's view of one joined row.
func (s *Service) viewOf(row model.InstanceView, active Runtime) (View, error) {
	flags, err := model.ParseFlagSet([]byte(row.FlagsJSON))
	if err != nil {
		return View{}, model.Error{Code: model.CodeBadFlags, Message: err.Error()}
	}
	return View{
		InstanceView:    row,
		Flags:           flags,
		Derived:         row.Derived(active.ID),
		ActiveVersionID: active.ID,
	}, nil
}

// activeRuntime resolves the active build, treating "none active" as the zero
// Runtime rather than as an error: an instance may legitimately be created
// before llama.cpp is installed, and activation recomputes every hash when it
// happens (D69).
func (s *Service) activeRuntime(ctx context.Context, tx store.Tx) Runtime {
	rt, err := s.resolver.ActiveRuntime(ctx, tx)
	if err != nil {
		return Runtime{}
	}
	return rt
}

// portContext reads the two settings and the one runtime fact section 2.8's
// port rules are evaluated against, plus every live port claim.
func (s *Service) portContext(ctx context.Context, tx store.Tx) (PortPolicy, []PortHolder, error) {
	desired, err := s.settings.GetInt(ctx, "ui.port_desired")
	if err != nil {
		return PortPolicy{}, nil, err
	}
	minPort, err := s.settings.GetInt(ctx, "instances.internal_port_min")
	if err != nil {
		return PortPolicy{}, nil, err
	}
	maxPort, err := s.settings.GetInt(ctx, "instances.internal_port_max")
	if err != nil {
		return PortPolicy{}, nil, err
	}
	bind, err := s.settings.GetString(ctx, "gateway.bind")
	if err != nil {
		return PortPolicy{}, nil, err
	}

	policy := PortPolicy{
		UIPortDesired: int(desired),
		InternalMin:   int(minPort),
		InternalMax:   int(maxPort),
		GatewayBind:   bind,
	}
	if s.runtime != nil {
		info, err := s.runtime.RuntimeInfo(ctx, tx)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return PortPolicy{}, nil, err
		}
		if info.UIPort != nil {
			policy.UIPort = int(*info.UIPort)
		}
	}

	holders, err := s.store.InstancePortHolders(ctx, tx)
	if err != nil {
		return PortPolicy{}, nil, err
	}
	return policy, holders, nil
}

// choosePort validates a requested port, or allocates one when it was omitted.
func (s *Service) choosePort(kind PortKind, requested *int, policy PortPolicy,
	holders []PortHolder, excludeID string) (int, error) {
	if requested != nil {
		if err := ValidatePort(kind, *requested, policy, holders, excludeID, s.probe); err != nil {
			return 0, err
		}
		return *requested, nil
	}
	return SuggestPort(kind, policy, holders, s.probe)
}

// chooseInternalPort is choosePort plus the one rule that only applies to a
// fresh pair: the two ports must differ. The pools normally keep them apart, so
// this is a guard against a policy whose pools overlap rather than an
// expectation.
func (s *Service) chooseInternalPort(requested *int, policy PortPolicy,
	holders []PortHolder, excludeID string, publicPort int) (int, error) {
	port, err := s.choosePort(PortInternal, requested, policy, holders, excludeID)
	if err != nil {
		return 0, err
	}
	if port == publicPort {
		return 0, model.Error{
			Code:    model.CodePortUnavailable,
			Message: "the public and internal ports must differ",
			Details: map[string]any{"port": port, "reason": string(model.PortInUseByInstance)},
		}
	}
	return port, nil
}

// flagWarnings turns section 5.7's two build-capability rules into the warnings
// the instance form shows. Neither is ever an error: llama.cpp ships ~10
// nightlies a day, and a hard failure would make the tool brittle by design.
func flagWarnings(flags model.FlagSet, active Runtime, unknown []string) []model.Warning {
	var out []model.Warning
	if len(unknown) > 0 {
		out = append(out, model.Warning{
			Code:    model.WarnUnknownFlags,
			Message: "this build's --help does not advertise every flag this configuration renders",
			Details: map[string]any{"flags": unknown},
		})
	} else if !active.Help.Available() {
		out = append(out, model.Warning{
			Code:    model.WarnFlagCheckUnavailable,
			Message: "flag check unavailable for this build",
		})
	}
	if !active.SupportsFit && flags.NGpuLayers != nil && flags.NGpuLayers.Mode == model.NGLAuto {
		out = append(out, model.Warning{
			Code:    model.WarnNGLAutoWithoutFit,
			Message: "this build predates --fit; auto behaves as all",
		})
	}
	return out
}

// event appends one `events` row inside the caller's transaction.
func (s *Service) event(ctx context.Context, tx store.Tx, now time.Time, inst model.Instance,
	action string, level model.EventLevel, message string, from, to *string) error {
	if s.events == nil {
		return nil
	}
	return s.events.Append(ctx, tx, s.newEvent(now, inst, action, level, message, from, to))
}

// publish fans the frame out AFTER the transaction has committed. A subscriber
// told about a row that then rolled back would have been told something that
// did not happen.
func (s *Service) publish(inst model.Instance, action string, now time.Time) {
	if s.events == nil {
		return
	}
	s.events.Publish(s.newEvent(now, inst, action, model.LevelInfo, "", nil, nil))
}

func (s *Service) newEvent(now time.Time, inst model.Instance, action string,
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
		Actor:       model.ActorAdmin,
		Message:     message,
	}
}

func conflictGeneration(current, sent int64) error {
	return model.Error{
		Code: model.CodeConflictGeneration,
		Message: "this instance was edited by someone else; reload the form and reapply " +
			"your change",
		Details: map[string]any{"generation": current, "sent": sent},
	}
}

func valueOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

func ptrTo[T any](v T) *T { return &v }
