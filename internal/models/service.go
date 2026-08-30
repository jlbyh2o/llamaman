package models

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The local model service (DESIGN sections 2.6, 3.7, 7.2, 7.2a).
//
// It owns the CATALOG: which models this host has, where they are, what their
// headers say, which instances are using them, and what removing one would cost.
// The filesystem half lives in internal/hf/cache; the SQL lives in
// internal/store (D49 invariant 1); this package is the guarded transitions
// between them.
//
// Two rules from the design shape nearly every method here:
//
//   - D69: a model's resolved path is a `config_hash` input. In the SAME
//     transaction that writes `snapshot_dir`, `primary_file` or `state`, this
//     service calls `instances.RecomputeConfigHash` for every non-deleted
//     instance referencing that model. Without it, "queue the download, then
//     configure the instance" ends with an instance whose stored hash describes
//     a path that never existed, and `restart_required` never fires.
//   - §7.2: deleting a model NEVER issues a SQL DELETE. The row moves
//     `deleting → deleted` and stays, so `instances.model_id`'s ON DELETE
//     RESTRICT is never exercised on that path and a soft-deleted instance keeps
//     a readable record of what it once pointed at. The cache-root DETACH of
//     §7.2a is the documented exception, and it has its own, wider guard.

// Store is the persistence this service needs. *store.Store satisfies it
// structurally (DESIGN section 1, invariant 1).
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	CacheRoots(ctx context.Context, tx store.Tx) ([]model.CacheRoot, error)
	CacheRoot(ctx context.Context, tx store.Tx, id string) (model.CacheRoot, error)
	CacheRootByPath(ctx context.Context, tx store.Tx, path string) (model.CacheRoot, error)
	PrimaryCacheRoot(ctx context.Context, tx store.Tx) (model.CacheRoot, error)
	InsertCacheRoot(ctx context.Context, tx store.Tx, r model.CacheRoot) error
	UpdateCacheRootFacts(ctx context.Context, tx store.Tx, r model.CacheRoot) (bool, error)
	SetPrimaryCacheRoot(ctx context.Context, tx store.Tx, id string) (bool, error)
	StampCacheRootScan(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)
	DeleteCacheRoot(ctx context.Context, tx store.Tx, id string) (bool, error)
	CacheRootInstanceRefs(ctx context.Context, tx store.Tx, rootID string) ([]model.InstanceRef, error)

	LocalModels(ctx context.Context, tx store.Tx, f store.ModelFilter) ([]model.LocalModel, error)
	LocalModel(ctx context.Context, tx store.Tx, id string) (model.LocalModel, error)
	LocalModelByIdentity(ctx context.Context, tx store.Tx, rootID, repoID, revision, primaryFile string) (model.LocalModel, error)
	InsertLocalModel(ctx context.Context, tx store.Tx, m model.LocalModel) error
	UpdateLocalModel(ctx context.Context, tx store.Tx, m model.LocalModel) (bool, error)
	SetLocalModelState(ctx context.Context, tx store.Tx, id string, state model.ModelState, at int64) (bool, error)
	SetLocalModelMmproj(ctx context.Context, tx store.Tx, id string, mmprojID *string, auto bool, at int64) (bool, error)
	ModelInstanceRefs(ctx context.Context, tx store.Tx, modelID string, includeDeleted bool) ([]model.InstanceRef, error)
	ModelUsers(ctx context.Context, tx store.Tx) (map[string][]model.InstanceRef, error)
	InstancesForModels(ctx context.Context, tx store.Tx, ids []string) ([]string, error)
	ModelDiskUsage(ctx context.Context, tx store.Tx) ([]store.RootUsage, error)
	MmprojCandidates(ctx context.Context, tx store.Tx, rootID, repoID, revision string) ([]model.LocalModel, error)

	ModelFiles(ctx context.Context, tx store.Tx, modelID string) ([]model.ModelFile, error)
	UpsertModelFile(ctx context.Context, tx store.Tx, f model.ModelFile) error
	SetModelFileState(ctx context.Context, tx store.Tx, id string, state model.ModelFileState, at int64) (bool, error)
	DeleteModelFilesNotIn(ctx context.Context, tx store.Tx, modelID string, keep []string) (int64, error)

	InsertCacheScan(ctx context.Context, tx store.Tx, c model.CacheScan) error
	CacheScan(ctx context.Context, tx store.Tx, id string) (model.CacheScan, error)
	CacheScans(ctx context.Context, tx store.Tx, rootID string, limit int) ([]model.CacheScan, error)
	UpdateCacheScanProgress(ctx context.Context, tx store.Tx, c model.CacheScan) (bool, error)
	SetCacheScanState(ctx context.Context, tx store.Tx, id string, state model.CacheScanState,
		startedAt, finishedAt *int64, errMessage *string) (bool, error)

	StrayFiles(ctx context.Context, tx store.Tx, rootID string, includeDismissed bool) ([]model.StrayFile, error)
	StrayFile(ctx context.Context, tx store.Tx, id string) (model.StrayFile, error)
	UpsertStrayFile(ctx context.Context, tx store.Tx, st model.StrayFile) error
	DeleteStrayFilesNotSeen(ctx context.Context, tx store.Tx, rootID string, before int64) (int64, error)
	DeleteStrayFile(ctx context.Context, tx store.Tx, id string) (bool, error)
	DismissStrayFile(ctx context.Context, tx store.Tx, id string, at int64) (bool, error)

	// PutSetting and SetRuntimeCachePaths are §7.2a's other two
	// representations. They are in THIS interface, rather than reached through
	// the settings cache, because SetPrimaryRoot writes all four in ONE
	// transaction — and a settings setter that opens its own would make that
	// promise untrue.
	PutSetting(ctx context.Context, tx store.Tx, v model.Setting) error
	SetRuntimeCachePaths(ctx context.Context, tx store.Tx, hubDir string, hfHome *string) (bool, error)
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// ConfigHashes is D69's other half: `instances.RecomputeConfigHash`.
// *instances.Service satisfies it, and the interface is declared here because
// the consumer owns it — importing internal/instances for one method signature
// would couple the catalog to the renderer for no gain.
type ConfigHashes interface {
	RecomputeConfigHash(ctx context.Context, tx store.Tx, ids ...string) error
}

// Queue is the job queue this service enqueues into. *jobs.Queue satisfies it.
//
// Every long action here is a job (§3's "long actions never block"): a scan, a
// verify and a delete all return `202` with a job id and report over SSE. The
// EnqueueTx form is what §2.3a requires — the job row and the domain row are
// written by the same transaction, so they cannot drift.
type Queue interface {
	EnqueueTx(ctx context.Context, tx store.Tx, p jobs.EnqueueParams) (jobs.EnqueueResult, error)
}

// SettingsCache is the read-through cache's invalidation half. SetPrimaryRoot
// writes `settings['hf.hub_dir']` through the store, inside its transaction, and
// then tells the cache to forget both keys — the ordering §3.4's "takes effect
// immediately" needs.
type SettingsCache interface {
	Invalidate(keys ...string)
}

// The settings keys §7.2a keeps in agreement with `hf_cache_roots`.
const (
	KeyHubDir = "hf.hub_dir"
	KeyHFHome = "hf.home"
)

// Config wires a Service.
type Config struct {
	Store  Store
	Events Events
	// Hashes is optional only in the sense that a binary built without the
	// instance service has nothing to recompute. When it is present, every
	// write that moves a resolved path calls it (D69).
	Hashes ConfigHashes
	// Queue is optional; without it the endpoints that start work answer 503
	// rather than doing the work synchronously. A scan that blocked an HTTP
	// request would violate §3's long-action rule and would not survive a
	// restart, which is the property the job row exists for.
	Queue Queue
	// Settings is the cache to invalidate after a SetPrimaryRoot commit.
	Settings SettingsCache
	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// StateDir is the resolved state directory (D72), which the detection
	// chain's rule 4 compares against `$HOME`.
	StateDir string
}

// Service is the local model service.
type Service struct {
	store    Store
	events   Events
	hashes   ConfigHashes
	queue    Queue
	settings SettingsCache
	now      func() time.Time
	newID    func(time.Time) string
	stateDir string
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("models: a store is required")
	}
	s := &Service{
		store:    cfg.Store,
		events:   cfg.Events,
		hashes:   cfg.Hashes,
		queue:    cfg.Queue,
		settings: cfg.Settings,
		now:      cfg.Now,
		newID:    cfg.NewID,
		stateDir: cfg.StateDir,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	return s, nil
}

// View is one catalog row as the API returns it.
type View struct {
	model.LocalModel
	// RootPath is the hub directory this model lives under, so the UI can say
	// which disk without a second request.
	RootPath string
	// InUseBy names the non-deleted instances referencing this model. It is the
	// `in_use_by` column of §3.7's list, and it is what turns "delete" into a
	// disabled button rather than a 409 the user has to read.
	InUseBy []model.InstanceRef
	// Mmproj is the paired projector row, when one is paired.
	Mmproj *model.LocalModel
}

// Detail is `GET /api/v1/models/{id}`: the row, its files, and the projector
// picker §7.2 owes the user when auto-pairing declined to guess.
type Detail struct {
	View
	Files            []model.ModelFile
	MmprojCandidates []model.LocalModel
}

// ListParams is `GET /api/v1/models`'s query.
type ListParams struct {
	State []model.ModelState
	Kind  []model.ModelKind
	Query string
	Sort  string
	// RootID narrows to one cache root.
	RootID string
	// IncludeDeleted admits `deleted` rows, which are history rather than
	// catalog (§7.2: the row survives a delete on purpose).
	IncludeDeleted bool
}

// List is `GET /api/v1/models`.
func (s *Service) List(ctx context.Context, p ListParams) ([]View, error) {
	var out []View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.LocalModels(ctx, tx, store.ModelFilter{
			RootID:         p.RootID,
			States:         p.State,
			Kinds:          p.Kind,
			Query:          p.Query,
			IncludeDeleted: p.IncludeDeleted,
			Sort:           p.Sort,
		})
		if err != nil {
			return err
		}
		users, err := s.store.ModelUsers(ctx, tx)
		if err != nil {
			return err
		}
		roots, err := s.rootPaths(ctx, tx)
		if err != nil {
			return err
		}

		byID := make(map[string]model.LocalModel, len(rows))
		for _, m := range rows {
			byID[m.ID] = m
		}
		out = make([]View, 0, len(rows))
		for _, m := range rows {
			v := View{LocalModel: m, RootPath: roots[m.RootID], InUseBy: users[m.ID]}
			if m.MmprojModelID != nil {
				if mm, ok := byID[*m.MmprojModelID]; ok {
					v.Mmproj = &mm
				}
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// Get is `GET /api/v1/models/{id}`.
func (s *Service) Get(ctx context.Context, id string) (Detail, error) {
	var out Detail
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		m, err := s.store.LocalModel(ctx, tx, id)
		if err != nil {
			return err
		}
		refs, err := s.store.ModelInstanceRefs(ctx, tx, id, false)
		if err != nil {
			return err
		}
		roots, err := s.rootPaths(ctx, tx)
		if err != nil {
			return err
		}
		out.View = View{LocalModel: m, RootPath: roots[m.RootID], InUseBy: refs}

		if out.Files, err = s.store.ModelFiles(ctx, tx, id); err != nil {
			return err
		}
		// The picker is offered for anything that is not itself a projector.
		// It is offered even when one is already paired, because changing a
		// wrong automatic pairing is exactly what the endpoint is for.
		if m.Kind != model.ModelMmproj {
			if out.MmprojCandidates, err = s.store.MmprojCandidates(ctx, tx,
				m.RootID, m.RepoID, m.Revision); err != nil {
				return err
			}
		}
		if m.MmprojModelID != nil {
			mm, err := s.store.LocalModel(ctx, tx, *m.MmprojModelID)
			if err == nil {
				out.Mmproj = &mm
			}
		}
		return nil
	})
	return out, err
}

// PairMmproj is `POST /api/v1/models/{id}/pair-mmproj`. It sets
// `mmproj_auto = 0`, which is what stops a later scan from overruling the
// choice; passing an empty id detaches the projector, likewise manually.
//
// The pairing is a `config_hash` input for every instance using this model —
// the rendered argv gains or loses `--mmproj` — so D69's recompute runs in the
// same transaction.
func (s *Service) PairMmproj(ctx context.Context, id, mmprojID string) (View, error) {
	var out View
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		m, err := s.store.LocalModel(ctx, tx, id)
		if err != nil {
			return err
		}
		var target *string
		if mmprojID != "" {
			mm, err := s.store.LocalModel(ctx, tx, mmprojID)
			if err != nil {
				return err
			}
			if mm.Kind != model.ModelMmproj {
				return model.Error{Code: model.CodeModelMissing,
					Message: "that model is not a multimodal projector",
					Details: map[string]any{"model_id": mmprojID, "kind": string(mm.Kind)}}
			}
			target = &mmprojID
		}

		now := s.now().UnixMilli()
		if _, err := s.store.SetLocalModelMmproj(ctx, tx, id, target, false, now); err != nil {
			return err
		}
		if err := s.recomputeFor(ctx, tx, id); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, model.Event{
			Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorAdmin,
			Action: "mmproj_paired", SubjectID: &id, Message: mmprojMessage(m, mmprojID),
		}); err != nil {
			return err
		}
		m.MmprojModelID = target
		m.MmprojAuto = false
		m.UpdatedAt = now
		out = View{LocalModel: m}
		return nil
	})
	if err == nil {
		s.publish(model.Event{Level: model.LevelInfo, Category: model.CategoryModel,
			Actor: model.ActorAdmin, Action: "mmproj_paired", SubjectID: &id})
	}
	return out, err
}

// Metadata is `GET /api/v1/models/{id}/metadata`: the full GGUF key/value map.
//
// It re-reads the file rather than serving `metadata_json`, and the reason is
// size. A scan of a 300-model cache that retained every tokenizer table would
// hold hundreds of megabytes of vocabulary in memory and then write it into the
// database, to answer a question about one model that a user asks once. The
// stored column is the FALLBACK, for a model whose disk is no longer there —
// which is the one case where re-reading cannot work and a cached answer is
// better than none.
func (s *Service) Metadata(ctx context.Context, id string) (map[string]any, error) {
	var m model.LocalModel
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		m, err = s.store.LocalModel(ctx, tx, id)
		return err
	}); err != nil {
		return nil, err
	}

	if kv, err := readMetadata(m); err == nil {
		return kv, nil
	}
	if m.MetadataJSON == nil {
		return nil, model.Error{Code: model.CodeModelMissing,
			Message: "the model's file is not readable and no metadata was recorded",
			Details: map[string]any{"model_id": id, "path": primaryPath(m)}}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*m.MetadataJSON), &out); err != nil {
		return nil, fmt.Errorf("models: decode stored metadata: %w", err)
	}
	return out, nil
}

// Usage is the per-root disk accounting `GET /api/v1/system/disk` renders beside
// the filesystem's own numbers.
type Usage struct {
	store.RootUsage
	// FreeBytes and TotalBytes come from the `hf_cache_roots` row, which the
	// scan refreshes. They are the FILESYSTEM's numbers and are deliberately
	// not derived from the catalog: the difference between "our models occupy
	// 400 GB" and "the disk has 20 GB left" is the whole point of showing both.
	TotalBytes *int64
	FreeBytes  *int64
	IsPrimary  bool
	Writable   bool
	LastScanAt *int64
}

// Usage aggregates the catalog per cache root.
func (s *Service) Usage(ctx context.Context) ([]Usage, error) {
	var out []Usage
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.ModelDiskUsage(ctx, tx)
		if err != nil {
			return err
		}
		roots, err := s.store.CacheRoots(ctx, tx)
		if err != nil {
			return err
		}
		byID := make(map[string]model.CacheRoot, len(roots))
		for _, r := range roots {
			byID[r.ID] = r
		}
		out = make([]Usage, 0, len(rows))
		for _, u := range rows {
			usage := Usage{RootUsage: u}
			if r, ok := byID[u.RootID]; ok {
				usage.TotalBytes, usage.FreeBytes = r.TotalBytes, r.FreeBytes
				usage.IsPrimary, usage.Writable, usage.LastScanAt = r.IsPrimary, r.Writable, r.LastScanAt
			}
			out = append(out, usage)
		}
		return nil
	})
	return out, err
}

// recomputeFor is D69, in one call: every non-deleted instance referencing this
// model gets its `config_hash` recomputed, in the caller's transaction.
//
// It is a no-op when no instance service is wired, which is the state of a
// binary built without one — and correct, because there is then nothing whose
// hash could go stale.
func (s *Service) recomputeFor(ctx context.Context, tx store.Tx, modelIDs ...string) error {
	if s.hashes == nil {
		return nil
	}
	ids, err := s.store.InstancesForModels(ctx, tx, modelIDs)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return s.hashes.RecomputeConfigHash(ctx, tx, ids...)
}

// appendEvent writes an `events` row inside the caller's transaction, filling in
// the id, the instant and the subject type this service always uses.
func (s *Service) appendEvent(ctx context.Context, tx store.Tx, ev model.Event) error {
	if s.events == nil {
		return nil
	}
	now := s.now()
	if ev.ID == "" {
		ev.ID = s.newID(now)
	}
	if ev.At == 0 {
		ev.At = now.UnixMilli()
	}
	if ev.SubjectType == nil && ev.SubjectID != nil {
		st := string(model.SubjectModel)
		ev.SubjectType = &st
	}
	return s.events.Append(ctx, tx, ev)
}

// publish pushes the SSE frame AFTER the transaction commits. Publishing inside
// one would announce a change a rollback could still undo.
func (s *Service) publish(ev model.Event) {
	if s.events == nil {
		return
	}
	if ev.ID == "" {
		ev.ID = s.newID(s.now())
	}
	if ev.At == 0 {
		ev.At = s.now().UnixMilli()
	}
	if ev.SubjectType == nil && ev.SubjectID != nil {
		st := string(model.SubjectModel)
		ev.SubjectType = &st
	}
	s.events.Publish(ev)
}

func (s *Service) rootPaths(ctx context.Context, tx store.Tx) (map[string]string, error) {
	roots, err := s.store.CacheRoots(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(roots))
	for _, r := range roots {
		out[r.ID] = r.Path
	}
	return out, nil
}

// primaryPath is the resolved file llama.cpp would be handed: the snapshot
// directory joined with the primary file. It is built here rather than stored
// as a third column so the two halves can never disagree.
func primaryPath(m model.LocalModel) string {
	if m.SnapshotDir == "" || m.PrimaryFile == "" {
		return ""
	}
	return filepath.Join(m.SnapshotDir, filepath.FromSlash(m.PrimaryFile))
}

func mmprojMessage(m model.LocalModel, mmprojID string) string {
	if mmprojID == "" {
		return "detached the multimodal projector from " + m.RepoID
	}
	return "paired a multimodal projector with " + m.RepoID
}
