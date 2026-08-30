package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The cache scan and its reconciliation (DESIGN sections 2.6, 3.7, 7.2).
//
// The whole scan is ONE job, so it survives a restart: boot triage returns a
// `cache_scan` job to `queued` (§2.3) and the walk starts again against the row
// it already has. The reconciliation itself is idempotent — every write is
// keyed by `UNIQUE(root_id, repo_id, revision, primary_file)` — which is what
// makes restarting it free rather than merely safe.
//
// Reconciliation, in order:
//
//  1. Walk the root. Every `snapshots/<commit>/` directory is a REVISION, and
//     the directory name IS `models.revision` (never `refs/main`).
//  2. Group the GGUFs in each snapshot into logical models and pair projectors.
//  3. Upsert `models` + `model_files` with `origin='scanned'`, `state='ready'`.
//  4. Rows previously present whose files vanished become `missing` — NEVER
//     deleted. A disk may have been unplugged, and the row is how the user
//     finds out which one.
//  5. Strays: GGUFs outside a snapshot, orphan blobs, broken links, unparsable
//     files.
//
// D69 runs through all of it: any write that moves `snapshot_dir`,
// `primary_file` or `state` recomputes `config_hash` for every non-deleted
// instance referencing that model, in the same transaction.

// JobRef is the `{job_id, subject}` receipt §3 gives for a long action. It is a
// struct rather than a bare id so a handler renders the same shape for every
// kind, and so a replayed idempotent request is distinguishable from a fresh one
// — D65 says a replay answers `200` with the same body where a new job answers
// `202`.
//
// An empty JobID means this binary was built without a job queue: the domain row
// exists and nothing is scheduled. That is reported rather than papered over
// with synchronous work, which would violate §3's long-action rule and would not
// survive a restart.
type JobRef struct {
	JobID    string
	Replayed bool
	// SubjectID is the domain row the job names: a `cache_scans.id` for a scan,
	// a `models.id` for a verify or a delete (§2.3a).
	SubjectID string
}

// ScanParams is what a `cache_scan` job carries in `params_json` — everything
// the worker needs to resume after a restart, since the leased row is all it
// gets.
type ScanParams struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
}

// RequestScan is `POST /api/v1/cache/scan`: the `cache_scans` row and its job in
// one transaction (§2.3a), so `idx_jobs_one_live_per_subject` makes a second
// live scan of the same scan row structurally impossible.
//
// rootID empty means the primary root, which is what the wizard's step and the
// post-download trigger want.
func (s *Service) RequestScan(ctx context.Context, rootID string,
	trigger model.CacheScanTrigger) (model.CacheScan, JobRef, error) {

	var (
		row model.CacheScan
		ref JobRef
	)
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		var (
			root model.CacheRoot
			err  error
		)
		if rootID == "" {
			root, err = s.store.PrimaryCacheRoot(ctx, tx)
		} else {
			root, err = s.store.CacheRoot(ctx, tx, rootID)
		}
		if err != nil {
			return err
		}

		now := s.now()
		row = model.CacheScan{
			ID: s.newID(now), RootID: root.ID, State: model.ScanQueued,
			Trigger: trigger, CreatedAt: now.UnixMilli(),
		}
		if err := s.store.InsertCacheScan(ctx, tx, row); err != nil {
			return err
		}
		ref, err = s.enqueue(ctx, tx, jobs.EnqueueParams{
			Kind:     model.JobCacheScan,
			DomainID: row.ID,
			Params:   ScanParams{RootID: root.ID, Path: root.Path},
		}, row.ID)
		return err
	})
	return row, ref, err
}

// enqueueScan is RequestScan's inner half, for the callers that are already
// inside a transaction — registering a root and promoting one both end with a
// scan of it (§7.2a).
func (s *Service) enqueueScan(ctx context.Context, tx store.Tx, rootID string,
	trigger model.CacheScanTrigger) (JobRef, error) {

	if s.queue == nil {
		return JobRef{}, nil
	}
	root, err := s.store.CacheRoot(ctx, tx, rootID)
	if err != nil {
		return JobRef{}, err
	}
	now := s.now()
	row := model.CacheScan{
		ID: s.newID(now), RootID: rootID, State: model.ScanQueued,
		Trigger: trigger, CreatedAt: now.UnixMilli(),
	}
	if err := s.store.InsertCacheScan(ctx, tx, row); err != nil {
		return JobRef{}, err
	}
	return s.enqueue(ctx, tx, jobs.EnqueueParams{
		Kind:     model.JobCacheScan,
		DomainID: row.ID,
		Params:   ScanParams{RootID: rootID, Path: root.Path},
	}, row.ID)
}

// enqueue is the one place this service talks to the queue. A nil queue is a
// binary built without one; the caller then has a domain row and no job, which
// is the honest state and not a silent synchronous fallback.
func (s *Service) enqueue(ctx context.Context, tx store.Tx, p jobs.EnqueueParams, subjectID string) (JobRef, error) {
	if s.queue == nil {
		return JobRef{SubjectID: subjectID}, nil
	}
	res, err := s.queue.EnqueueTx(ctx, tx, p)
	if err != nil {
		return JobRef{}, err
	}
	return JobRef{JobID: res.Job.ID, Replayed: res.Replayed, SubjectID: subjectID}, nil
}

// Scan returns one scan row — `GET /api/v1/cache/scans/{id}`.
func (s *Service) Scan(ctx context.Context, id string) (model.CacheScan, error) {
	var out model.CacheScan
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = s.store.CacheScan(ctx, tx, id)
		return err
	})
	return out, err
}

// ScanWorker runs `cache_scan` jobs. It is registered with the queue by the
// composition root.
type ScanWorker struct{ svc *Service }

// NewScanWorker builds the worker for `jobs.Queue.Register`.
func NewScanWorker(s *Service) *ScanWorker { return &ScanWorker{svc: s} }

// Kind is `cache_scan`.
func (w *ScanWorker) Kind() model.JobKind { return model.JobCacheScan }

// Run walks the root and reconciles the catalog against it.
//
// The domain row moves with the job row (§2.3a): `queued → running` when the
// walk starts, and `succeeded`/`failed`/`canceled` in the same transaction as
// the job's terminal state, which is what the returned Outcome carries.
func (w *ScanWorker) Run(ctx context.Context, t *jobs.Task) (jobs.Outcome, error) {
	s := w.svc
	scanID := t.Job().SubjectID

	var p ScanParams
	if raw := t.Job().ParamsJSON; raw != nil {
		if err := json.Unmarshal([]byte(*raw), &p); err != nil {
			return jobs.Failed("bad_params", "the scan job's parameters could not be decoded",
				w.commit(scanID, model.ScanFailed, "bad_params")), nil
		}
	}
	if p.RootID == "" {
		// A job whose params were lost — an older row, or a hand-written one.
		// The row itself still names the root, which is the authority.
		if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			row, err := s.store.CacheScan(ctx, tx, scanID)
			if err != nil {
				return err
			}
			p.RootID = row.RootID
			root, err := s.store.CacheRoot(ctx, tx, row.RootID)
			if err != nil {
				return err
			}
			p.Path = root.Path
			return nil
		}); err != nil {
			return jobs.Outcome{}, err
		}
	}

	started := s.now().UnixMilli()
	if err := t.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := s.store.SetCacheScanState(ctx, tx, scanID, model.ScanRunning, &started, nil, nil)
		return err
	}); err != nil {
		return jobs.Outcome{}, err
	}

	counters, err := s.runScan(ctx, t, scanID, p)
	switch {
	case errors.Is(err, context.Canceled):
		return jobs.Canceled(w.commitCounters(scanID, model.ScanCanceled, counters, nil)), nil
	case err != nil:
		msg := err.Error()
		return jobs.RetryableFailure("scan_failed", msg,
			w.commitCounters(scanID, model.ScanFailed, counters, &msg)), nil
	}
	return jobs.Succeeded(w.commitCounters(scanID, model.ScanSucceeded, counters, nil)), nil
}

// commit moves the domain row with no counters — the parameter-decode failure,
// which never got as far as walking anything.
func (w *ScanWorker) commit(scanID string, state model.CacheScanState, reason string) jobs.CommitFunc {
	msg := reason
	return func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		finished := w.svc.now().UnixMilli()
		_, err := w.svc.store.SetCacheScanState(ctx, tx, scanID, state, nil, &finished, &msg)
		return err
	}
}

func (w *ScanWorker) commitCounters(scanID string, state model.CacheScanState,
	c model.CacheScan, errMessage *string) jobs.CommitFunc {

	return func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		c.ID = scanID
		if _, err := w.svc.store.UpdateCacheScanProgress(ctx, tx, c); err != nil {
			return err
		}
		finished := w.svc.now().UnixMilli()
		_, err := w.svc.store.SetCacheScanState(ctx, tx, scanID, state, nil, &finished, errMessage)
		return err
	}
}

// runScan is the walk plus the reconciliation. It returns the counters whatever
// happened, because a canceled or failed scan's partial numbers are still what
// the row should record.
func (s *Service) runScan(ctx context.Context, t *jobs.Task, scanID string, p ScanParams) (model.CacheScan, error) {
	counters := model.CacheScan{RootID: p.RootID}
	startedAt := s.now().UnixMilli()

	// Progress is written to the row every 250 ms and pushed as job progress at
	// the same rate, which is what §7.2's "counters update every 250 ms" means
	// on both sides of the wire.
	var lastWrite time.Time
	progress := func(pr cache.Progress) {
		counters.DirsSeen = int64(pr.DirsSeen)
		counters.FilesSeen = int64(pr.FilesSeen)
		counters.ModelsFound = int64(pr.ModelsSeen)
		counters.BytesTotal = pr.BytesTotal
		if now := s.now(); now.Sub(lastWrite) >= cache.ProgressEvery {
			lastWrite = now
			row := counters
			row.ID = scanID
			// Both writes are best-effort and their errors are deliberately
			// dropped: progress is a display fact, the terminal transition
			// writes the final counters anyway, and failing a scan of a
			// 300-model cache because one progress UPDATE lost a race would
			// trade the whole result for a number nobody reads twice.
			_ = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := s.store.UpdateCacheScanProgress(ctx, tx, row)
				return err
			})
			if t != nil {
				_ = t.SetProgress(ctx, pr)
			}
		}
	}

	res, err := cache.Scan(ctx, p.Path, cache.ScanOptions{Progress: progress, Now: s.now})
	counters.DirsSeen = int64(res.DirsSeen)
	counters.FilesSeen = int64(res.FilesSeen)
	counters.BytesTotal = res.BytesTotal
	counters.StraysFound = int64(len(res.Strays))
	if err != nil {
		return counters, err
	}

	added, found, err := s.reconcile(ctx, p.RootID, res, startedAt)
	counters.ModelsFound = found
	counters.ModelsAdded = added
	if err != nil {
		return counters, err
	}

	missing, err := s.markMissing(ctx, p.RootID, res, startedAt)
	counters.ModelsMissing = missing
	if err != nil {
		return counters, err
	}

	if err := s.recordStrays(ctx, p.RootID, res, startedAt); err != nil {
		return counters, err
	}

	// Refresh the root's own facts: free space moves under a scan more than
	// anything else does, and the storage view reads it from this row.
	if info, verr := cache.Validate(p.Path, cache.ValidateOptions{}); verr == nil && info.Exists {
		_ = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			row, err := s.store.CacheRoot(ctx, tx, p.RootID)
			if err != nil {
				return err
			}
			row.Writable, row.SymlinksOK = info.Writable, info.SymlinksOK
			applyFSFacts(&row, info)
			if _, err := s.store.UpdateCacheRootFacts(ctx, tx, row); err != nil {
				return err
			}
			_, err = s.store.StampCacheRootScan(ctx, tx, p.RootID, s.now().UnixMilli())
			return err
		})
	}
	return counters, nil
}

// seen identifies one logical model within a root: repo + revision + primary
// file. It is the identity `UNIQUE(root_id, repo_id, revision, primary_file)`
// covers, which is what lets step 4 tell "gone from disk" from "never here".
type seen struct{ repoID, revision, primaryFile string }

// reconcile upserts one `models` row per group, and its `model_files` rows.
//
// Every snapshot is its own model row: N snapshot directories produce N distinct
// rows, each correctly labeled, each independently deletable and referenceable
// by an instance (§7.2).
func (s *Service) reconcile(ctx context.Context, rootID string, res cache.Result, at int64) (added, found int64, err error) {
	for _, repo := range res.Repos {
		for _, snap := range repo.Snapshots {
			groups := GroupSnapshot(snap.Files)
			if len(groups) == 0 {
				continue
			}
			found += int64(len(groups))

			// Projectors are written FIRST, because a weights row references
			// one through `mmproj_model_id` and a foreign key cannot point at a
			// row that does not exist yet.
			ids := make([]string, len(groups))
			for i, g := range groups {
				if g.Kind != model.ModelMmproj {
					continue
				}
				id, isNew, uerr := s.upsertGroup(ctx, rootID, repo, snap, g, at)
				if uerr != nil {
					return added, found, uerr
				}
				ids[i] = id
				if isNew {
					added++
				}
			}
			for i, g := range groups {
				if g.Kind == model.ModelMmproj {
					continue
				}
				id, isNew, uerr := s.upsertGroup(ctx, rootID, repo, snap, g, at)
				if uerr != nil {
					return added, found, uerr
				}
				ids[i] = id
				if isNew {
					added++
				}
			}

			if err := s.pairProjectors(ctx, groups, ids, at); err != nil {
				return added, found, err
			}
		}
	}
	return added, found, nil
}

// upsertGroup writes one logical model and its files, and reports whether the
// row was new.
//
// D69 lives here: the recompute runs whenever `snapshot_dir`, `primary_file` or
// `state` moved, in the same transaction as the write that moved it.
func (s *Service) upsertGroup(ctx context.Context, rootID string, repo cache.RepoEntry,
	snap cache.SnapshotEntry, g Group, at int64) (id string, isNew bool, err error) {

	var sink events.Sink
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		existing, ferr := s.store.LocalModelByIdentity(ctx, tx, rootID, repo.RepoID, snap.Revision, g.PrimaryFile)
		switch {
		case ferr == nil:
			id = existing.ID
		case errors.Is(ferr, store.ErrNotFound):
			isNew = true
			id = s.newID(s.now())
		default:
			return ferr
		}

		row := buildRow(id, rootID, repo, snap, g, at)
		if isNew {
			row.CreatedAt = at
			row.Origin = model.OriginScanned
			if err := s.store.InsertLocalModel(ctx, tx, row); err != nil {
				return err
			}
		} else {
			// A row this daemon downloaded keeps `origin='llamaman'` — the scan
			// is confirming it, not claiming it. The pairing columns are also
			// left alone, so a manual `pair-mmproj` survives every rescan.
			row.Origin = existing.Origin
			row.CreatedAt = existing.CreatedAt
			moved := existing.SnapshotDir != row.SnapshotDir ||
				existing.PrimaryFile != row.PrimaryFile ||
				existing.State != row.State
			if _, err := s.store.UpdateLocalModel(ctx, tx, row); err != nil {
				return err
			}
			if moved {
				if err := s.recomputeFor(ctx, tx, id); err != nil {
					return err
				}
			}
		}

		keep := make([]string, 0, len(g.Files))
		for _, f := range g.Files {
			keep = append(keep, f.Name)
			if err := s.store.UpsertModelFile(ctx, tx, buildFile(s, id, f, g, at)); err != nil {
				return err
			}
		}
		if _, err := s.store.DeleteModelFilesNotIn(ctx, tx, id, keep); err != nil {
			return err
		}

		if isNew {
			return s.appendEvent(ctx, tx, model.Event{
				Level: model.LevelInfo, Category: model.CategoryModel, Actor: model.ActorSystem,
				Action: "model_scanned", SubjectID: &id,
				ToState: statePtr(row.State),
				Message: "found " + repo.RepoID + " " + g.PrimaryFile + " in the cache",
			}, &sink)
		}
		return nil
	})
	if err != nil {
		return id, isNew, err
	}
	s.publish(&sink)
	return id, isNew, nil
}

// pairProjectors applies §7.2's auto-pairing across one snapshot's groups.
//
// A row whose `mmproj_auto` is already false is skipped: that flag is a human's
// decision and a rescan does not get to overrule it.
func (s *Service) pairProjectors(ctx context.Context, groups []Group, ids []string, at int64) error {
	var candidates []Group
	candidateIDs := map[string]string{}
	for i, g := range groups {
		if g.Kind == model.ModelMmproj && ids[i] != "" {
			candidates = append(candidates, g)
			candidateIDs[g.PrimaryFile] = ids[i]
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for i, g := range groups {
			if g.Kind == model.ModelMmproj || ids[i] == "" {
				continue
			}
			row, err := s.store.LocalModel(ctx, tx, ids[i])
			if err != nil {
				return err
			}
			if !row.MmprojAuto {
				continue
			}
			chosen, ok := PairMmproj(g, candidates)
			if !ok {
				// Several candidates and no clear winner: the UI shows a picker
				// rather than a guess. An existing automatic pairing is left in
				// place, because un-pairing a working instance on a rescan
				// would be a change nobody asked for.
				continue
			}
			target := candidateIDs[chosen.PrimaryFile]
			if row.MmprojModelID != nil && *row.MmprojModelID == target {
				continue
			}
			if _, err := s.store.SetLocalModelMmproj(ctx, tx, ids[i], &target, true, at); err != nil {
				return err
			}
			// The projector is part of the rendered argv, so pairing one moves
			// every referencing instance's config hash (D69).
			if err := s.recomputeFor(ctx, tx, ids[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// markMissing is step 4: a row previously found under this root whose primary
// file the walk no longer saw becomes `missing`.
//
// NEVER deleted. A disk may have been unplugged, an external tool may have moved
// the cache, and the row is how the user finds out which model went — and what
// to plug back in. A model that returns is moved back to `ready` by the next
// scan, through the ordinary upsert path.
func (s *Service) markMissing(ctx context.Context, rootID string, res cache.Result, at int64) (int64, error) {
	// A group the walk saw is PRESENT, complete or not. An incomplete shard set
	// is a model that is on the disk and missing a member — the upsert above
	// already recorded it as `incomplete` — and treating it as absent here
	// would move it straight back to `missing` on the same pass, so a
	// half-downloaded model could never be shown as one.
	present := map[seen]struct{}{}
	for _, repo := range res.Repos {
		for _, snap := range repo.Snapshots {
			for _, g := range GroupSnapshot(snap.Files) {
				present[seen{repo.RepoID, snap.Revision, g.PrimaryFile}] = struct{}{}
			}
		}
	}

	var (
		n    int64
		sink events.Sink
	)
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.LocalModels(ctx, tx, store.ModelFilter{
			RootID: rootID,
			States: []model.ModelState{model.ModelReady, model.ModelIncomplete},
		})
		if err != nil {
			return err
		}
		for _, m := range rows {
			if _, ok := present[seen{m.RepoID, m.Revision, m.PrimaryFile}]; ok {
				continue
			}
			if _, err := s.store.SetLocalModelState(ctx, tx, m.ID, model.ModelMissing, at); err != nil {
				return err
			}
			// `state` moved, so D69 applies: the launcher's preflight and the
			// stored config hash both describe a path that is no longer there.
			if err := s.recomputeFor(ctx, tx, m.ID); err != nil {
				return err
			}
			from, to := string(m.State), string(model.ModelMissing)
			id := m.ID
			if err := s.appendEvent(ctx, tx, model.Event{
				Level: model.LevelWarn, Category: model.CategoryModel, Actor: model.ActorSystem,
				Action: "model_missing", SubjectID: &id, FromState: &from, ToState: &to,
				Message: m.RepoID + " " + m.PrimaryFile + " is no longer on disk",
			}, &sink); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	s.publish(&sink)
	return n, nil
}

// recordStrays upserts what the walk found and removes the rows whose files are
// gone. `last_seen_at` is the discriminator: anything not touched by this pass
// was cleaned up by us, by another tool, or by the user, and the row is the
// record of a problem that no longer exists.
func (s *Service) recordStrays(ctx context.Context, rootID string, res cache.Result, at int64) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, st := range res.Strays {
			if err := s.store.UpsertStrayFile(ctx, tx, model.StrayFile{
				ID: s.newID(s.now()), RootID: rootID, Path: st.Path,
				SizeBytes: st.SizeBytes, Reason: st.Reason,
				FirstSeenAt: at, LastSeenAt: at,
			}); err != nil {
				return err
			}
		}
		_, err := s.store.DeleteStrayFilesNotSeen(ctx, tx, rootID, at)
		return err
	})
}

// buildRow projects one group into a `models` row.
func buildRow(id, rootID string, repo cache.RepoEntry, snap cache.SnapshotEntry,
	g Group, at int64) model.LocalModel {

	row := model.LocalModel{
		ID: id, RootID: rootID, RepoID: repo.RepoID, Revision: snap.Revision,
		Kind: g.Kind, Origin: model.OriginScanned, State: model.ModelReady,
		SnapshotDir: snap.Dir, PrimaryFile: g.PrimaryFile,
		ShardCount: g.ShardCount, TotalBytes: g.TotalBytes, BytesOnDisk: g.BytesOnDisk,
		MmprojAuto: true, LastVerifiedAt: &at, CreatedAt: at, UpdatedAt: at,
	}
	if !g.Complete {
		// A shard set missing a member, or one whose link is broken.
		//
		// `incomplete` is §2.6's paused-download state, and using it for a
		// partial set found on disk is the closest honest fit: the file set is
		// not all here, a download can resume into exactly this row, and the
		// alternatives are worse in both directions. `ready` would let an
		// instance try to load a set llama.cpp cannot open; `corrupt` would
		// claim a verification failed that never ran; and omitting the row
		// entirely would hide gigabytes the user needs to see to clean up.
		row.State = model.ModelIncomplete
	}
	if len(snap.RefNames) > 0 {
		ref := strings.Join(snap.RefNames, ", ")
		row.RefName = &ref
	}
	if g.QuantLabel != "" {
		q := g.QuantLabel
		row.QuantLabel = &q
		row.FileType = &q
	}
	applyShape(&row, g.Shape, at)
	return row
}

// applyShape copies the GGUF geometry §2.6's columns hold.
//
// Absence is carried honestly throughout: a nil pointer means the key was not in
// the file, which for the sliding-window pair IS the semantics (§8.3 — a model
// with no `sliding_window` key has no sliding-window attention at all, whatever
// any pattern key says) and which for the rest is what makes
// `gguf_parsed_at IS NULL` distinguishable from "parsed, and the answer is 0".
func applyShape(row *model.LocalModel, sh *gguf.Shape, at int64) {
	if sh == nil {
		return
	}
	row.GGUFParsedAt = &at
	setStr(&row.Arch, sh.Architecture)
	setInt(&row.NLayer, sh.BlockCount)
	setInt(&row.NCtxTrain, sh.ContextLength)
	setInt(&row.NEmbd, sh.EmbeddingLength)
	setInt(&row.NFF, sh.FeedForwardLength)
	setInt(&row.NHead, sh.HeadCount)
	setInt(&row.HeadDimK, sh.KeyLength)
	setInt(&row.HeadDimV, sh.ValueLength)
	setInt(&row.NVocab, sh.VocabSize)
	setInt(&row.NExpert, sh.ExpertCount)
	setInt(&row.NExpertUsed, sh.ExpertUsedCount)
	setStr(&row.TokenizerModel, sh.TokenizerModel)
	row.HasVision = sh.HasVision

	if sh.SlidingWindow != nil {
		v := int64(*sh.SlidingWindow)
		row.SWAWindow = &v
	}
	if sh.SlidingWindowPattern != nil {
		v := int64(*sh.SlidingWindowPattern)
		row.SWAPattern = &v
	}

	// `n_head_kv_json` is the key VERBATIM (D30): an array when the file gave
	// one, a scalar when it gave a scalar. §8.3 indexes it per layer either way,
	// and storing a broadcast array for a scalar file would lose the fact that
	// the producer wrote one number.
	if len(sh.HeadCountKV) > 0 {
		var (
			b   []byte
			err error
		)
		if sh.HeadCountKVPerLayer {
			b, err = json.Marshal(sh.HeadCountKV)
		} else {
			b, err = json.Marshal(sh.HeadCountKV[0])
		}
		if err == nil {
			v := string(b)
			row.NHeadKVJSON = &v
		}
	}
	if b, err := json.Marshal(sh.Sizes); err == nil {
		v := string(b)
		row.TensorSummaryJSON = &v
	}
}

// buildFile projects one scanned file into a `model_files` row.
//
// `state` is `present` for a file that is there and `missing` for a broken link.
// `checksum_verified` stays false: a scan stats, it does not hash. Section 3.7's
// `POST /models/{id}/verify` is what sets it, because re-reading 40 GB to
// confirm a digest is a job a user asks for, not something a boot scan does to
// every model on the disk.
func buildFile(s *Service, modelID string, f cache.FileEntry, g Group, at int64) model.ModelFile {
	row := model.ModelFile{
		ID: s.newID(s.now()), ModelID: modelID, Filename: f.Name,
		ShardIndex: shardIndex(f), ShardTotal: g.ShardTotal,
		SizeBytes: f.SizeBytes, BytesOnDisk: f.BytesOnDisk,
		State: model.FilePresent, CreatedAt: at, UpdatedAt: at,
	}
	if f.Broken {
		row.State = model.FileMissing
	}
	if f.Etag != "" {
		etag := f.Etag
		row.Etag = &etag
	}
	if f.BlobPath != "" {
		blob := f.BlobPath
		row.BlobPath = &blob
	}
	link := f.Path
	row.LinkPath = &link
	return row
}

// readMetadata re-parses a model's primary file for the full key/value map.
func readMetadata(m model.LocalModel) (map[string]any, error) {
	path := filepath.Join(m.SnapshotDir, filepath.FromSlash(m.PrimaryFile))
	if path == "" {
		return nil, fmt.Errorf("models: the model has no resolved path")
	}
	f, err := gguf.ParseFile(path)
	if err != nil {
		return nil, err
	}
	pairs := f.KV.All()
	out := make(map[string]any, len(pairs))
	keys := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		out[kv.Key] = kv.Value.Any()
		keys = append(keys, kv.Key)
	}
	sort.Strings(keys)
	return out, nil
}

func setStr(dst **string, v string) {
	if v == "" {
		return
	}
	s := v
	*dst = &s
}

func setInt(dst **int64, v int) {
	if v == 0 {
		return
	}
	n := int64(v)
	*dst = &n
}

func statePtr(s model.ModelState) *string {
	v := string(s)
	return &v
}
