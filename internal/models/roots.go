package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Cache roots (DESIGN sections 3.7, 7.2, 7.2a).
//
// "One cache location, four representations, ONE writer." The location is
// visible in `hf_cache_roots`, in `settings['hf.hub_dir']`, in the derived
// `settings['hf.home']` and in `runtime_info.hf_hub_dir`/`hf_home`, and they
// would drift in a week if they were four independent facts. They are not:
// SetPrimaryRoot below is the only thing that writes any of them, and
// `PATCH /settings {"hf.hub_dir"}`, `POST /cache/roots/{id}/promote` and the
// wizard's `hf` step all call it.

// RootView is one `hf_cache_roots` row as the API returns it.
type RootView struct {
	model.CacheRoot
	// HFHome is the courtesy projection: the hub directory minus a trailing
	// `/hub`, and EMPTY when there is no such suffix. Rule 1 of §7.2 routinely
	// produces a hub directory that has no `HF_HOME` at all, and the Storage
	// form renders this beneath the editable hub field only when one exists.
	HFHome string
	// Models and BytesOnDisk are this root's share of the catalog.
	Models      int64
	BytesOnDisk int64
}

// Roots is `GET /api/v1/cache/roots`.
func (s *Service) Roots(ctx context.Context) ([]RootView, error) {
	var out []RootView
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		roots, err := s.store.CacheRoots(ctx, tx)
		if err != nil {
			return err
		}
		usage, err := s.store.ModelDiskUsage(ctx, tx)
		if err != nil {
			return err
		}
		byID := make(map[string]store.RootUsage, len(usage))
		for _, u := range usage {
			byID[u.RootID] = u
		}
		out = make([]RootView, 0, len(roots))
		for _, r := range roots {
			v := RootView{CacheRoot: r}
			if home, ok := cache.HFHome(r.Path); ok {
				v.HFHome = home
			}
			if u, ok := byID[r.ID]; ok {
				v.Models, v.BytesOnDisk = u.Models, u.BytesOnDisk
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// AddRoot is `POST /api/v1/cache/roots`.
//
// A new root is NEVER primary (§3.7): it is scan-and-serve only until something
// promotes it, because "which disk did that 40 GB go to" is a question a user
// should never have to answer twice. The path is validated the same way
// SetPrimaryRoot validates one — writability, the F17 symlink probe, and the
// `ProtectSystem=full` prefix check — except that a non-writable directory is
// recorded as `writable=0` rather than refused, since a read-only library is a
// perfectly good thing to serve models out of.
func (s *Service) AddRoot(ctx context.Context, path string) (RootView, JobRef, error) {
	info, err := cache.Validate(path, cache.ValidateOptions{Create: false})
	if err != nil {
		return RootView{}, JobRef{}, rootError(err, path)
	}
	if !info.Exists {
		return RootView{}, JobRef{}, model.Error{Code: model.CodeSettingInvalid,
			Message: "no directory exists at that path",
			Details: map[string]any{"path": info.Path}}
	}

	var (
		view RootView
		ref  JobRef
	)
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if existing, err := s.store.CacheRootByPath(ctx, tx, info.Path); err == nil {
			view = RootView{CacheRoot: existing}
			return model.Error{Code: model.CodeSettingInvalid,
				Message: "that cache root is already registered",
				Details: map[string]any{"path": info.Path, "root_id": existing.ID}}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		now := s.now()
		from := model.DetectedFromManual
		row := model.CacheRoot{
			ID: s.newID(now), Path: info.Path, IsPrimary: false,
			Writable: info.Writable, SymlinksOK: info.SymlinksOK,
			DetectedFrom: &from, CreatedAt: now.UnixMilli(),
		}
		applyFSFacts(&row, info)
		if err := s.store.InsertCacheRoot(ctx, tx, row); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "cache_root_added", Message: "registered cache root " + row.Path,
		}); err != nil {
			return err
		}

		var err error
		if ref, err = s.enqueueScan(ctx, tx, row.ID, model.ScanTriggerManual); err != nil {
			return err
		}
		view = RootView{CacheRoot: row}
		if home, ok := cache.HFHome(row.Path); ok {
			view.HFHome = home
		}
		return nil
	})
	return view, ref, err
}

// SetPrimaryRoot is §7.2a's single writer of all four representations.
//
// In ONE transaction it upserts the `hf_cache_roots` row for that hub directory
// EXACTLY AS GIVEN, moves `is_primary`, writes `settings['hf.hub_dir']` and the
// derived `settings['hf.home']`, refreshes `runtime_info.hf_hub_dir`/`hf_home`,
// and enqueues a scan of the new root. It emits one event and requires no
// restart: nothing about the change touches a unit file (D57) or the running
// listener set.
//
// Three things it deliberately does NOT do, each stated because the opposite
// would be a plausible reading:
//
//   - The old root is KEPT, as a non-primary root. Its models keep their
//     `root_id`, keep resolving to real files, and keep serving instances.
//     Relocating the cache is a statement about where NEW downloads go, not a
//     migration; nothing is moved, copied or deleted.
//   - `models.root_id` is never rewritten.
//   - The hub directory is stored verbatim. No `/hub` is appended and none is
//     stripped — rule 1 of §7.2 produces a hub directory with no such suffix,
//     and `settings['hf.home']` is the projection that is allowed to be empty.
func (s *Service) SetPrimaryRoot(ctx context.Context, hubDir string,
	from model.CacheRootDetectedFrom) (RootView, JobRef, error) {

	info, err := cache.Validate(hubDir, cache.ValidateOptions{Create: true, RequireWritable: true})
	if err != nil {
		return RootView{}, JobRef{}, rootError(err, hubDir)
	}

	var (
		view RootView
		ref  JobRef
	)
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		now := s.now()
		row, err := s.store.CacheRootByPath(ctx, tx, info.Path)
		switch {
		case err == nil:
			row.Writable, row.SymlinksOK = info.Writable, info.SymlinksOK
			applyFSFacts(&row, info)
			if _, err := s.store.UpdateCacheRootFacts(ctx, tx, row); err != nil {
				return err
			}
		case errors.Is(err, store.ErrNotFound):
			row = model.CacheRoot{
				ID: s.newID(now), Path: info.Path,
				Writable: info.Writable, SymlinksOK: info.SymlinksOK,
				DetectedFrom: &from, CreatedAt: now.UnixMilli(),
			}
			applyFSFacts(&row, info)
			if err := s.store.InsertCacheRoot(ctx, tx, row); err != nil {
				return err
			}
		default:
			return err
		}

		if _, err := s.store.SetPrimaryCacheRoot(ctx, tx, row.ID); err != nil {
			return err
		}
		row.IsPrimary = true

		if err := s.writeCacheSettings(ctx, tx, row.Path, now.UnixMilli()); err != nil {
			return err
		}

		if err := s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "cache_root_promoted", Message: "the primary cache root is now " + row.Path,
		}); err != nil {
			return err
		}
		if ref, err = s.enqueueScan(ctx, tx, row.ID, model.ScanTriggerManual); err != nil {
			return err
		}

		view = RootView{CacheRoot: row}
		if home, ok := cache.HFHome(row.Path); ok {
			view.HFHome = home
		}
		return nil
	})
	if err != nil {
		return RootView{}, JobRef{}, err
	}

	// AFTER the commit, and only then. The read-through cache must not be told
	// to forget a value the transaction could still roll back (§3.4's "takes
	// effect immediately" is about a committed change).
	if s.settings != nil {
		s.settings.Invalidate(KeyHubDir, KeyHFHome)
	}
	s.publish(model.Event{Level: model.LevelInfo, Category: model.CategoryModel,
		Actor: model.ActorAdmin, Action: "cache_root_promoted"})
	return view, ref, nil
}

// PromoteRoot is `POST /api/v1/cache/roots/{id}/promote`: the same single write
// path, reached by id rather than by path.
//
// A `writable=0` root is refused with `422 root_not_writable` — it can be read
// and served forever, but it can never be the root downloads go to.
func (s *Service) PromoteRoot(ctx context.Context, id string) (RootView, JobRef, error) {
	var path string
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		r, err := s.store.CacheRoot(ctx, tx, id)
		if err != nil {
			return err
		}
		if !r.Writable {
			return model.Error{Code: model.CodeRootNotWritable,
				Message: "that cache root is not writable, so it cannot receive downloads",
				Details: map[string]any{"root_id": id, "path": r.Path}}
		}
		path = r.Path
		return nil
	}); err != nil {
		return RootView{}, JobRef{}, err
	}
	return s.SetPrimaryRoot(ctx, path, model.DetectedFromSetting)
}

// DetachRoot is `DELETE /api/v1/cache/roots/{id}`: the row is deleted, its
// `models`/`model_files`/`stray_files` cascade away, and NO FILE IS TOUCHED.
//
// This is THE ONE operation in the design that issues a SQL DELETE against
// `models`, and that is why its guard counts SOFT-DELETED instances too.
// `models.root_id` is ON DELETE CASCADE, so the rows go; `instances.model_id` is
// ON DELETE RESTRICT; and under D68 a soft-deleted instance keeps its row AND
// its `model_id`, deliberately, so the history stays readable. A guard phrased
// over non-deleted instances would therefore PASS and the transaction would then
// fail inside SQLite with a raw foreign-key violation instead of the documented
// 409 — which is exactly the bug this reading exists to prevent.
func (s *Service) DetachRoot(ctx context.Context, id string) error {
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		r, err := s.store.CacheRoot(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.IsPrimary {
			return model.Error{Code: model.CodeRootIsPrimary,
				Message: "the primary cache root cannot be detached; promote another root first",
				Details: map[string]any{"root_id": id, "path": r.Path}}
		}

		refs, err := s.store.CacheRootInstanceRefs(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return model.Error{Code: model.CodeModelInUse,
				Message: "instances still reference models on that cache root",
				Details: map[string]any{
					"root_id":   id,
					"path":      r.Path,
					"instances": refDetails(refs),
					"remedy": "purge the soft-deleted instances (DELETE /instances/{id}?purge=true) " +
						"or point the live ones at another model",
				}}
		}

		if _, err := s.store.DeleteCacheRoot(ctx, tx, id); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "cache_root_detached",
			Message: "detached cache root " + r.Path +
				" — its catalog rows were removed and no file was touched",
		})
	})
	if err == nil {
		s.publish(model.Event{Level: model.LevelInfo, Category: model.CategoryModel,
			Actor: model.ActorAdmin, Action: "cache_root_detached"})
	}
	return err
}

// DetectRoots runs §7.2's six-rule chain and registers what it found, ONCE.
//
// It is a no-op the moment a primary root exists: the chain "is evaluated top to
// bottom exactly once", and re-running it on a later boot would fight with a
// user who moved the cache in the UI — the environment variable that lost is
// still exported, and the next boot would promote it back.
//
// Every OTHER candidate that names an existing directory holding at least one
// `models--*` becomes a non-primary scan-and-serve root, so a user who once used
// `HF_HOME` and later moved to `HF_HUB_CACHE` sees both libraries on the first
// boot rather than half of one.
func (s *Service) DetectRoots(ctx context.Context) (Detection, error) {
	var out Detection
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.PrimaryCacheRoot(ctx, tx)
		if err == nil {
			out.AlreadyResolved = true
			return nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		return out, err
	}
	if out.AlreadyResolved {
		return out, nil
	}

	found := cache.Detect(cache.Env{StateDir: s.stateDir})
	out.Chain = found
	if found.Primary.Path == "" {
		// No rule named anything, which on a Unix host means `$HOME` is unset
		// and no variable was exported. There is nothing to register and
		// nothing to guess; the wizard's cache step asks.
		return out, nil
	}

	view, ref, err := s.SetPrimaryRoot(ctx, found.Primary.Path, found.Primary.From)
	if err != nil {
		return out, err
	}
	out.Primary, out.PrimaryScan = view, ref

	for _, other := range found.Others {
		v, r, err := s.AddRoot(ctx, other.Path)
		if err != nil {
			// A candidate that cannot be registered is not a boot failure: the
			// primary is already in place and the daemon is usable. It is
			// recorded and reported.
			out.Skipped = append(out.Skipped, DetectionSkip{Path: other.Path, Reason: err.Error()})
			continue
		}
		// The rule that FOUND it is more informative than `manual`, which is
		// what AddRoot stamps for a path a human typed.
		v.DetectedFrom = &other.From
		out.Others = append(out.Others, v)
		out.OtherScans = append(out.OtherScans, r)
	}
	return out, nil
}

// Detection is what DetectRoots did.
type Detection struct {
	// AlreadyResolved reports that a primary root existed and the chain was not
	// run at all.
	AlreadyResolved bool
	// Chain is the raw six-rule result, which the wizard shows so the user can
	// confirm or override the winner.
	Chain cache.Detection

	Primary     RootView
	PrimaryScan JobRef
	Others      []RootView
	OtherScans  []JobRef
	Skipped     []DetectionSkip
}

// DetectionSkip is a candidate the chain named and this daemon could not
// register.
type DetectionSkip struct {
	Path   string
	Reason string
}

// writeCacheSettings writes the two settings rows and the two runtime_info
// columns, inside the caller's transaction.
//
// `hf.home` is written as the empty string, not omitted, when the hub directory
// has no `/hub` suffix. An omitted row would let the registry default (also "")
// answer, which happens to be the same value today — but the row is what makes
// the projection EXPLICIT, so a later default cannot silently become the answer
// to a question §7.2a says has exactly one.
func (s *Service) writeCacheSettings(ctx context.Context, tx store.Tx, hubDir string, at int64) error {
	hubJSON, err := json.Marshal(hubDir)
	if err != nil {
		return fmt.Errorf("models: encode hub dir: %w", err)
	}
	home, _ := cache.HFHome(hubDir)
	homeJSON, err := json.Marshal(home)
	if err != nil {
		return fmt.Errorf("models: encode hf home: %w", err)
	}

	for _, row := range []model.Setting{
		{Key: KeyHubDir, Value: string(hubJSON), UpdatedAt: at, UpdatedBy: model.UpdatedByAdmin},
		{Key: KeyHFHome, Value: string(homeJSON), UpdatedAt: at, UpdatedBy: model.UpdatedByAdmin},
	} {
		if err := s.store.PutSetting(ctx, tx, row); err != nil {
			return err
		}
	}

	var homePtr *string
	if home != "" {
		homePtr = &home
	}
	_, err = s.store.SetRuntimeCachePaths(ctx, tx, hubDir, homePtr)
	return err
}

func applyFSFacts(row *model.CacheRoot, info cache.RootInfo) {
	if info.FSType != "" {
		fs := info.FSType
		row.FSType = &fs
	}
	total, free := info.TotalBytes, info.FreeBytes
	row.TotalBytes, row.FreeBytes = &total, &free
}

// rootError maps internal/hf/cache's refusals onto §3.7's documented codes. The
// mapping lives here rather than in the API layer because the codes are domain
// vocabulary — `422 root_path_protected` is a statement about this product's
// unit hardening, not about HTTP.
func rootError(err error, path string) error {
	switch {
	case errors.Is(err, cache.ErrRootProtected):
		return model.Error{Code: model.CodeRootPathProtected,
			Message: "that path is under a directory the service unit mounts read-only",
			Details: map[string]any{"path": path, "protected_prefixes": cache.ProtectedPrefixes}}
	case errors.Is(err, cache.ErrRootNotWritable):
		return model.Error{Code: model.CodeRootNotWritable,
			Message: "that directory is not writable by this service identity",
			Details: map[string]any{"path": path}}
	case errors.Is(err, cache.ErrRootNotAbsolute), errors.Is(err, cache.ErrRootNotDirectory):
		return model.Error{Code: model.CodeSettingInvalid,
			Message: err.Error(), Details: map[string]any{"path": path}}
	default:
		return err
	}
}

func refDetails(refs []model.InstanceRef) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		out = append(out, map[string]any{
			"id": r.ID, "name": r.Name, "role": r.Role, "deleted": r.Deleted(),
		})
	}
	return out
}
