package download

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The download service (DESIGN sections 2.7, 3.8, 7.3, 7.4; D26, D27).
//
// Three layers sit over one download and section 2.3a fixes their relationship
// so they cannot drift: one `jobs` row carries scheduling, `downloads.state`
// carries domain state, `download_tasks` carry per-file state and are folded
// upward. This service writes all three in ONE transaction on the way in, and
// the worker moves all three together on the way through.
//
// # Why pause is a job state and not a flag
//
// `POST /downloads/{id}/pause` calls the queue's Pause, which writes
// `jobs.state='paused'` and RELEASES THE LEASE. The running worker's next
// heartbeat finds the lease gone, cuts its own context and unwinds; the queue
// then drops its outcome, because writing to a job you no longer own is the one
// thing a worker must never do. So the pause needs no separate signal, no shared
// flag and no cooperation from the transfer loop — the `.incomplete` files stand
// where they are, and Resume returns the job to `queued` for the same worker to
// pick up and continue from the byte it reached. That mechanism is why section
// 2.3's state table has `paused` in it at all.

// JobRef is the `{job_id, subject}` receipt section 3 gives for a long action,
// the same shape internal/models returns. Replayed distinguishes a D65 replay,
// which answers `200` with the same body, from a fresh job, which answers `202`.
type JobRef struct {
	JobID     string
	Replayed  bool
	SubjectID string
}

// Store is the persistence this service needs. *store.Store satisfies it
// structurally (DESIGN section 1, invariant 1: the consumer owns the interface).
type Store interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error

	PrimaryCacheRoot(ctx context.Context, tx store.Tx) (model.CacheRoot, error)

	LocalModel(ctx context.Context, tx store.Tx, id string) (model.LocalModel, error)
	LocalModelByIdentity(ctx context.Context, tx store.Tx,
		rootID, repoID, revision, primaryFile string) (model.LocalModel, error)
	InsertLocalModel(ctx context.Context, tx store.Tx, m model.LocalModel) error
	UpdateLocalModel(ctx context.Context, tx store.Tx, m model.LocalModel) (bool, error)
	SetLocalModelState(ctx context.Context, tx store.Tx, id string,
		state model.ModelState, at int64) (bool, error)
	SetLocalModelMmproj(ctx context.Context, tx store.Tx, id string,
		mmprojID *string, auto bool, at int64) (bool, error)

	ModelFiles(ctx context.Context, tx store.Tx, modelID string) ([]model.ModelFile, error)
	UpsertModelFile(ctx context.Context, tx store.Tx, f model.ModelFile) error
	SetModelFileState(ctx context.Context, tx store.Tx, id string,
		state model.ModelFileState, at int64) (bool, error)

	InsertDownload(ctx context.Context, tx store.Tx, d store.Download) error
	Download(ctx context.Context, tx store.Tx, id string) (store.Download, error)
	Downloads(ctx context.Context, tx store.Tx, f store.DownloadFilter) ([]store.Download, error)
	LiveDownloadForModel(ctx context.Context, tx store.Tx, modelID string) (store.Download, error)
	SetDownloadState(ctx context.Context, tx store.Tx, id string, state model.DownloadState,
		startedAt, finishedAt *int64, errorCode, errorMessage *string) (bool, error)
	UpdateDownloadProgress(ctx context.Context, tx store.Tx, d store.Download) (bool, error)
	SetDownloadPriority(ctx context.Context, tx store.Tx, id string, priority int) (bool, error)
	BumpDownloadAttempts(ctx context.Context, tx store.Tx, id string) (bool, error)

	InsertDownloadTask(ctx context.Context, tx store.Tx, t store.DownloadTask) error
	DownloadTasks(ctx context.Context, tx store.Tx, downloadID string) ([]store.DownloadTask, error)
	DownloadTaskViews(ctx context.Context, tx store.Tx, downloadID string) ([]store.DownloadTaskView, error)
	SetDownloadTaskState(ctx context.Context, tx store.Tx, id string,
		state model.DownloadTaskState, startedAt, finishedAt *int64, lastError *string) (bool, error)
	SetDownloadTasksState(ctx context.Context, tx store.Tx, downloadID string,
		state model.DownloadTaskState, at *int64) (int64, error)
	UpdateDownloadTaskTransfer(ctx context.Context, tx store.Tx, t store.DownloadTask) (bool, error)
	UpdateDownloadTaskProgress(ctx context.Context, tx store.Tx, id string, bytesDone int64) (bool, error)
	ClearDownloadTaskValidator(ctx context.Context, tx store.Tx, id string) (bool, error)
	BumpDownloadTaskAttempts(ctx context.Context, tx store.Tx, id string) (bool, error)
	KnownIncompletePaths(ctx context.Context, tx store.Tx) (map[string]struct{}, error)

	LiveJobForSubject(ctx context.Context, tx store.Tx,
		subjectType model.JobSubjectType, subjectID string) (model.Job, error)
	// Jobs is how Retry reaches a job that has already STOPPED: `failed` and
	// `canceled` are terminal, so LiveJobForSubject filters out exactly the rows
	// a retry is for.
	Jobs(ctx context.Context, tx store.Tx, f store.JobFilter) ([]model.Job, error)
	// LiveIdempotencyKey answers only "is this key inside its window", so the
	// `download_exists` guard can step aside for a D65 replay without
	// reimplementing D65 (see isReplay).
	LiveIdempotencyKey(ctx context.Context, tx store.Tx, key string, now int64) (model.IdempotencyKey, error)
	SetDownloadJobPriority(ctx context.Context, tx store.Tx, jobID string, priority int) (bool, error)
}

// Client is the Hub client this service needs. *hf.Client satisfies it.
//
// It is an interface so the worker can be driven against a stub in a unit test,
// and so the integration suite can point the real client at an httptest server
// without either of them reaching for a package-level variable.
type Client interface {
	Tree(ctx context.Context, repo, revision string) ([]hf.TreeEntry, error)
	Head(ctx context.Context, repo, revision, filePath string) (hf.FileMeta, error)
	Open(ctx context.Context, p hf.OpenParams) (*hf.Transfer, error)
	ResolveURL(repo, revision, filePath string) (string, error)
	Endpoint() string
}

// Events is the events/SSE seam. Append belongs inside the caller's write
// transaction; Publish runs only after it commits.
type Events interface {
	Append(ctx context.Context, tx store.Tx, ev model.Event) error
	Publish(ev model.Event)
}

// Queue is the job queue. *jobs.Queue satisfies it.
type Queue interface {
	EnqueueTx(ctx context.Context, tx store.Tx, p jobs.EnqueueParams) (jobs.EnqueueResult, error)
	Pause(ctx context.Context, id string, domain jobs.CommitFunc) error
	Resume(ctx context.Context, id string, domain jobs.CommitFunc) error
	Cancel(ctx context.Context, id string) (model.Job, error)
	Retry(ctx context.Context, id string) (model.Job, error)
}

// ConfigHashes is D69's other half. A model's resolved path is a `config_hash`
// input, and a download landing is exactly the moment one moves: a `planned`
// model becomes `ready` with a real `snapshot_dir`/`primary_file`, and every
// non-deleted instance referencing it must have its hash recomputed IN THE SAME
// TRANSACTION.
//
// Without it, "queue the download, configure the instance while it runs"
// (section 3.10a) ends with an instance whose stored hash describes a path that
// never existed, and `restart_required` never fires.
type ConfigHashes interface {
	RecomputeConfigHash(ctx context.Context, tx store.Tx, ids ...string) error
}

// Scans requests the `post_download` cache scan of section 2.6.
//
// The scan is what fills a downloaded model's GGUF geometry: this worker knows
// the bytes landed and where, and internal/models owns the mapping from a parsed
// header to the twenty columns section 2.6 holds. Duplicating that mapping here
// would be two implementations of one table, which is the drift bug section 7.2a
// spends a page preventing for the cache root.
type Scans interface {
	RequestScan(ctx context.Context, rootID string, trigger model.CacheScanTrigger) (model.CacheScan, error)
}

// Settings is the read-through settings cache. *settings.Cache satisfies it.
type Settings interface {
	GetInt(ctx context.Context, key string) (int64, error)
	GetBool(ctx context.Context, key string) (bool, error)
}

// The settings keys this package reads (section 3.4).
const (
	KeyConcurrency     = "hf.download_concurrency"
	KeyRateLimit       = "hf.rate_limit_bytes_sec"
	KeyVerifyChecksums = "hf.verify_checksums"
)

// DiskHeadroom is section 7.4's disk guard margin: free space must exceed
// `bytes_total - bytes_done + 1 GiB`.
//
// The gibibyte is not superstition. A filesystem at absolute zero free space
// cannot write the journal entry that would record the last extent, so a
// download sized to land exactly at full fails at 99.9% after an hour — and
// takes the daemon's own database, which lives on the same disk in the default
// topology, down with it.
const DiskHeadroom = 1 << 30

// Config wires a Service.
type Config struct {
	Store  Store
	Client Client
	Events Events
	Queue  Queue
	// Hashes is D69. A binary built without the instance service has nothing to
	// recompute, and nil is that binary.
	Hashes ConfigHashes
	// Scans is optional; without it a completed download records its files and
	// leaves the geometry for the next ordinary scan.
	Scans Scans
	// Settings is optional; without it the defaults of section 3.4's registry
	// apply.
	Settings Settings
	// Now supplies every instant this service stamps. Nil uses time.Now.
	Now func() time.Time
	// NewID mints row ids. Nil uses store.NewID.
	NewID func(time.Time) string
	// Logger receives one line per transfer and per failure. Nil uses
	// slog.Default.
	Logger *slog.Logger
	// Sleep is how a transfer retry waits. Nil uses a context-aware timer; a
	// test substitutes a no-op so a suite that exercises five resume attempts
	// does not spend fifteen seconds asleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Service is the download service.
type Service struct {
	store  Store
	client Client
	events Events
	queue  Queue
	hashes ConfigHashes
	scans  Scans
	config Settings
	now    func() time.Time
	newID  func(time.Time) string
	sleep  func(ctx context.Context, d time.Duration) error
	log    *slog.Logger

	// files is D26's file-level pool: at most `hf.download_concurrency` files in
	// flight across EVERY download, not per download. A sharded model still
	// progresses on several shards at once through it, and two queued downloads
	// cannot between them open twice the connections the setting allows.
	//
	// It is created lazily, on the first worker run, because the setting is read
	// from the database and this service is constructed before the settings
	// cache is loaded.
	filesOnce sync.Once
	files     chan struct{}
	// limit is the shared byte-rate limiter, nil when
	// `hf.rate_limit_bytes_sec` is 0.
	limit *rateLimiter

	// cancels tracks the deletion a cancel asked for, so a worker winding down
	// removes its own partial rather than a second goroutine racing the writer
	// for it.
	mu      sync.Mutex
	dropped map[string]bool
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("hf/download: a store is required")
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("hf/download: a Hub client is required")
	}
	s := &Service{
		store:   cfg.Store,
		client:  cfg.Client,
		events:  cfg.Events,
		queue:   cfg.Queue,
		hashes:  cfg.Hashes,
		scans:   cfg.Scans,
		config:  cfg.Settings,
		now:     cfg.Now,
		newID:   cfg.NewID,
		sleep:   cfg.Sleep,
		log:     cfg.Logger,
		dropped: map[string]bool{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sleep == nil {
		s.sleep = sleepCtx
	}
	if s.newID == nil {
		s.newID = store.NewID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// CreateRequest is the body of `POST /api/v1/downloads`.
type CreateRequest struct {
	RepoID string
	// Revision may be a branch, a tag or a commit sha; empty means `main`. What
	// is STORED is never this string: `models.revision` is the resolved commit
	// the Hub reported in `x-repo-commit`, because `main` names a different tree
	// next week and a model row has to keep meaning what it meant.
	Revision string
	// Files are paths inside the repository. Any file of a shard set expands to
	// the whole set (section 7.3).
	Files []string
	// IncludeMmproj adds the repository's projector when there is one and the
	// weights are not themselves a projector.
	IncludeMmproj bool
	// MmprojFile is an explicit projector choice, for a repository with several.
	MmprojFile string
	// Kind is `models.kind` for the weights row. Empty means `text`.
	Kind model.ModelKind
	// Priority is `downloads.priority` and `jobs.priority`; lower runs first.
	Priority int
	// Idempotency carries D65's replay window.
	Idempotency *jobs.Idempotency
}

// CreateResult is what `POST /api/v1/downloads` answers with.
type CreateResult struct {
	Download store.Download
	// ModelID is the weights row. It is in the response so the UI can open the
	// model page immediately — section 3.10a's "queue the download, configure
	// the instance while it runs" needs an id before a byte has moved.
	ModelID string
	// MmprojModelID is the projector row, empty when none was included.
	MmprojModelID string
	Job           JobRef
}

// Create is `POST /api/v1/downloads` (section 3.8).
//
// It answers `202 {"job_id","subject","model_id"}` — the one long-action shape
// of section 3 — with the `jobs`, `downloads`, `models` and `model_files` rows
// all written in ONE transaction (section 2.7), so `idx_jobs_one_live_per_subject`
// makes a second live job for the same download structurally impossible rather
// than merely unlikely.
//
// Everything before that transaction is network work and guards: resolve the
// tree, expand the shard set, resolve the commit, check the disk. None of it
// writes, so a refusal leaves nothing behind.
func (s *Service) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := hf.ValidateRepo(req.RepoID); err != nil {
		return CreateResult{}, model.Error{Code: CodeFileNotInRepo, Message: err.Error()}
	}

	entries, err := s.client.Tree(ctx, req.RepoID, req.Revision)
	if err != nil {
		return CreateResult{}, hubError(err)
	}
	plan, err := ExpandRequest(entries, req.Files, req.IncludeMmproj, req.MmprojFile)
	if err != nil {
		return CreateResult{}, err
	}

	// One HEAD resolves the commit — and, not incidentally, is the request that
	// discovers a gated repository. Section 7.1: metadata succeeds while
	// `resolve` returns 401/403, so a gate is invisible until this call, and
	// discovering it HERE rather than in the worker is what turns it into a
	// `403 hf_gated` a user sees on the click instead of a job that fails an
	// hour later.
	primary := PrimaryFile(plan.Weights)
	meta, err := s.client.Head(ctx, req.RepoID, req.Revision, primary)
	if err != nil {
		return CreateResult{}, hubError(err)
	}
	// `x-repo-commit` is read off the FINAL response after redirects — the CDN's
	// headers, or whatever host `settings['hf.endpoint']` names — and it becomes
	// a DIRECTORY NAME two lines below (`layout.SnapshotDir`, a plain
	// filepath.Join) that the transfer then creates and symlinks under. A header
	// of `../../../../..` would therefore write outside the hub root as the
	// service identity and store the escaped path in `models.snapshot_dir`. It
	// is checked here, at the one point where a server-supplied string becomes a
	// path, and checked against what section 2.6 says the column holds anyway: a
	// resolved 40-hex commit, never a branch name and never a path.
	commit := meta.Commit
	if commit != "" && (hf.ValidateRevision(commit) != nil || !looksLikeCommit(commit)) {
		return CreateResult{}, model.Error{
			Code: CodeHFUnreachable,
			Message: "the Hub reported a commit that is not a 40-character sha, " +
				"so it cannot be used as a cache directory name",
		}
	}
	if commit == "" {
		// An endpoint that does not report `x-repo-commit` leaves the revision
		// unresolved. Storing the branch name would violate section 2.6's
		// "resolved commit sha, never 'main'", so the request is refused with a
		// message that names the missing header rather than silently storing a
		// name that will mean something else next week.
		if req.Revision != "" && looksLikeCommit(req.Revision) {
			commit = req.Revision
		} else {
			return CreateResult{}, model.Error{
				Code:    CodeHFUnreachable,
				Message: "the Hub did not report which commit this revision resolves to",
			}
		}
	}

	var res CreateResult
	err = s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := s.store.PrimaryCacheRoot(ctx, tx)
		if errors.Is(err, store.ErrNotFound) {
			return model.Error{
				Code:    CodeNoCacheRoot,
				Message: "this daemon has no primary cache root yet",
			}
		}
		if err != nil {
			return err
		}

		// Section 7.4's disk guard, evaluated against the root the bytes will
		// actually land on. `bytes_done` is zero for a fresh download; a retry
		// re-enters through Retry, which does not pass here.
		if err := s.guardDisk(root.Path, plan.TotalBytes); err != nil {
			return err
		}

		layout := cache.NewLayout(root.Path)
		snapshotDir := layout.SnapshotDir(req.RepoID, commit)
		now := s.now()

		weights, err := s.upsertModel(ctx, tx, modelSpec{
			root: root.ID, repo: req.RepoID, revision: commit, refName: refNameFor(req.Revision, commit),
			snapshotDir: snapshotDir, group: plan.Weights, kind: kindFor(req.Kind, plan.Weights),
		}, now)
		if err != nil {
			return err
		}
		res.ModelID = weights.ID

		var mmproj *model.LocalModel
		if plan.Mmproj != nil {
			m, err := s.upsertModel(ctx, tx, modelSpec{
				root: root.ID, repo: req.RepoID, revision: commit, refName: refNameFor(req.Revision, commit),
				snapshotDir: snapshotDir, group: *plan.Mmproj, kind: model.ModelMmproj,
			}, now)
			if err != nil {
				return err
			}
			mmproj = &m
			res.MmprojModelID = m.ID
			// The pairing is automatic, so `mmproj_auto` stays true: section 7.2
			// says a later manual choice sets it false and a rescan must not
			// overrule one.
			if _, err := s.store.SetLocalModelMmproj(ctx, tx, weights.ID, &m.ID, true,
				now.UnixMilli()); err != nil {
				return err
			}
		}

		// The `409 download_exists` guard. It is phrased over the WEIGHTS model,
		// which is what `downloads.model_id` names, and it names the existing
		// row rather than merely refusing — a user who double-clicked Download
		// wants to be taken to the one that is running.
		//
		// It must NOT fire ahead of D65's replay window, and that ordering is
		// the whole subtlety here: a double-clicked Download sends the same
		// `Idempotency-Key` twice, and answering the second one `409` would turn
		// the exact case the window exists for into an error. So a request whose
		// key is a live hit skips this guard and is handed to the queue against
		// the EXISTING download, where D65's own rules apply unchanged — a
		// matching route and fingerprint replay the original job, and a
		// mismatched one is `422 idempotency_key_reused`. Re-deriving those
		// rules here would be a second implementation of the window.
		existing, existsErr := s.store.LiveDownloadForModel(ctx, tx, weights.ID)
		switch {
		case existsErr == nil:
			if !s.isReplay(ctx, tx, req.Idempotency, now) {
				return model.Error{
					Code:    model.CodeDownloadExists,
					Message: "this model is already being downloaded",
					Details: map[string]any{
						"download_id": existing.ID, "state": string(existing.State),
					},
				}
			}
			ref, err := s.enqueue(ctx, tx, jobs.EnqueueParams{
				Kind:        model.JobModelDownload,
				DomainID:    existing.ID,
				Priority:    existing.Priority,
				MaxAttempts: 3,
				Params: Params{
					DownloadID: existing.ID, RepoID: req.RepoID, Revision: commit,
					RootID: root.ID, HubDir: root.Path,
					RefName: refNameFor(req.Revision, commit),
				},
				Idempotency: req.Idempotency,
			}, existing.ID)
			if err != nil {
				return err
			}
			res.Job, res.Download = ref, existing
			if weights.MmprojModelID != nil {
				res.MmprojModelID = *weights.MmprojModelID
			}
			return nil
		case errors.Is(existsErr, store.ErrNotFound):
			// No live download; carry on and create one.
		default:
			return existsErr
		}

		dl := store.Download{
			ID:            s.newID(now),
			ModelID:       weights.ID,
			State:         model.DownloadQueued,
			Priority:      priorityOr(req.Priority),
			IncludeMmproj: plan.Mmproj != nil,
			BytesTotal:    plan.TotalBytes,
			CreatedAt:     now.UnixMilli(),
		}
		if err := s.store.InsertDownload(ctx, tx, dl); err != nil {
			return err
		}

		if err := s.insertTasks(ctx, tx, dl, req, weights, mmproj, plan, now); err != nil {
			return err
		}

		ref, err := s.enqueue(ctx, tx, jobs.EnqueueParams{
			Kind:        model.JobModelDownload,
			DomainID:    dl.ID,
			Priority:    dl.Priority,
			MaxAttempts: 3,
			Params: Params{
				DownloadID: dl.ID, RepoID: req.RepoID, Revision: commit,
				RootID: root.ID, HubDir: root.Path,
			},
			Idempotency: req.Idempotency,
		}, dl.ID)
		if err != nil {
			return err
		}
		res.Job = ref
		res.Download = dl

		return s.appendEvent(ctx, tx, model.Event{
			ID: s.newID(now), At: now.UnixMilli(), Level: model.LevelInfo,
			Category: model.CategoryDownload, Action: "created",
			SubjectType: strPtr(string(model.SubjectDownload)), SubjectID: strPtr(dl.ID),
			ToState: strPtr(string(model.DownloadQueued)), Actor: model.ActorAdmin,
			Message: fmt.Sprintf("queued %s (%s) from %s",
				path.Base(primary), humanBytes(plan.TotalBytes), req.RepoID),
		})
	})
	if err != nil {
		return CreateResult{}, err
	}
	return res, nil
}

// isReplay reports whether this request's `Idempotency-Key` is a live row in
// D65's window.
//
// It answers ONLY that question, deliberately. Whether the replay is a valid one
// — same route, same request fingerprint — is the queue's to decide, and it
// decides it a few lines later with the same key; asking it here as well would
// be a second implementation of the rule that decides whether a user sees their
// original download or a `422`.
func (s *Service) isReplay(ctx context.Context, tx store.Tx, id *jobs.Idempotency, now time.Time) bool {
	if id == nil || id.Key == "" || s.queue == nil {
		return false
	}
	_, err := s.store.LiveIdempotencyKey(ctx, tx, id.Key, now.UnixMilli())
	return err == nil
}

// modelSpec is one `models` row's identity plus the group it holds.
type modelSpec struct {
	root        string
	repo        string
	revision    string
	refName     string
	snapshotDir string
	group       hf.FileGroup
	kind        model.ModelKind
}

// upsertModel finds or creates the `models` row for one group, and its
// `model_files` rows in `state='planned'` with `size_bytes` from `lfs.size`.
//
// Reusing an existing row rather than refusing is deliberate: a download that
// failed halfway leaves a `models` row behind, and the retry has to land on the
// same identity or `UNIQUE(root_id, repo_id, revision, primary_file)` refuses the
// insert and the user sees a foreign-key error instead of a download.
func (s *Service) upsertModel(ctx context.Context, tx store.Tx, spec modelSpec,
	now time.Time) (model.LocalModel, error) {

	primary := PrimaryFile(spec.group)
	row, err := s.store.LocalModelByIdentity(ctx, tx, spec.root, spec.repo, spec.revision, primary)
	switch {
	case err == nil:
		// The row exists. A `ready` model is left exactly as it is — its files
		// are on disk and the blob short-circuit of section 7.2 will link them
		// without a byte of transfer — and anything else returns to `planned`,
		// which is what a fresh attempt is.
		if row.State != model.ModelReady {
			if _, err := s.store.SetLocalModelState(ctx, tx, row.ID, model.ModelPlanned,
				now.UnixMilli()); err != nil {
				return model.LocalModel{}, err
			}
			row.State = model.ModelPlanned
		}
	case errors.Is(err, store.ErrNotFound):
		var ref *string
		if spec.refName != "" {
			ref = &spec.refName
		}
		var quant *string
		if spec.group.QuantLabel != "" {
			q := spec.group.QuantLabel
			quant = &q
		}
		row = model.LocalModel{
			ID: s.newID(now), RootID: spec.root, RepoID: spec.repo, Revision: spec.revision,
			RefName: ref, QuantLabel: quant, Kind: spec.kind,
			State: model.ModelPlanned, Origin: model.OriginLlamaman,
			SnapshotDir: spec.snapshotDir, PrimaryFile: primary,
			ShardCount: len(spec.group.Files), TotalBytes: spec.group.TotalBytes,
			MmprojAuto: true,
			CreatedAt:  now.UnixMilli(), UpdatedAt: now.UnixMilli(),
		}
		if err := s.store.InsertLocalModel(ctx, tx, row); err != nil {
			return model.LocalModel{}, err
		}
		// D69: a new row's `snapshot_dir`, `primary_file` and `state` are all
		// being written, so every instance that already references it — which is
		// none, for a row this transaction just minted — would be recomputed
		// here. The call is made anyway rather than reasoned around, because the
		// rule is "in the same transaction that writes any of these", and a
		// future path that reaches this line with an existing reference must not
		// depend on someone re-deriving the argument.
		if err := s.recompute(ctx, tx, row.ID); err != nil {
			return model.LocalModel{}, err
		}
	default:
		return model.LocalModel{}, err
	}

	existing, err := s.store.ModelFiles(ctx, tx, row.ID)
	if err != nil {
		return model.LocalModel{}, err
	}
	byName := map[string]model.ModelFile{}
	for _, f := range existing {
		byName[f.Filename] = f
	}
	for _, e := range spec.group.Files {
		idx, total := ShardIndex(e.Path, spec.group.ShardTotal)
		f, ok := byName[e.Path]
		if !ok {
			f = model.ModelFile{
				ID: s.newID(now), ModelID: row.ID, Filename: e.Path,
				CreatedAt: now.UnixMilli(),
			}
		}
		f.ShardIndex, f.ShardTotal = idx, total
		// The true size, from `lfs.size`. Never the top-level `size`, which for
		// an LFS entry is the ~130-byte pointer (section 7.1).
		f.SizeBytes = e.Size
		if e.LFS && e.OID != "" {
			// For an LFS object the tree's `oid` IS the blob name, so a download
			// can be planned — the `.incomplete` path chosen, an already-present
			// blob recognized — without a single HEAD.
			oid := e.OID
			f.Etag = &oid
		}
		if f.State != model.FilePresent {
			f.State = model.FilePlanned
		}
		f.UpdatedAt = now.UnixMilli()
		if err := s.store.UpsertModelFile(ctx, tx, f); err != nil {
			return model.LocalModel{}, err
		}
		byName[e.Path] = f
	}
	row.ShardCount = len(spec.group.Files)
	row.TotalBytes = spec.group.TotalBytes
	return row, nil
}

// insertTasks writes one `download_tasks` row per file of both groups.
func (s *Service) insertTasks(ctx context.Context, tx store.Tx, dl store.Download,
	req CreateRequest, weights model.LocalModel, mmproj *model.LocalModel,
	plan Plan, now time.Time) error {

	add := func(m model.LocalModel, g hf.FileGroup) error {
		files, err := s.store.ModelFiles(ctx, tx, m.ID)
		if err != nil {
			return err
		}
		byName := map[string]model.ModelFile{}
		for _, f := range files {
			byName[f.Filename] = f
		}
		for _, e := range g.Files {
			f, ok := byName[e.Path]
			if !ok {
				return fmt.Errorf("hf/download: %s has no model_files row", e.Path)
			}
			// The resolve URL is stored against the RESOLVED COMMIT rather than
			// the branch the user typed. A download that resumes next week must
			// fetch the same bytes it started, and `main` would not.
			url, err := s.client.ResolveURL(req.RepoID, m.Revision, e.Path)
			if err != nil {
				return err
			}
			t := store.DownloadTask{
				ID: s.newID(now), DownloadID: dl.ID, ModelFileID: f.ID, URL: url,
				State: model.TaskQueued, BytesTotal: e.Size, Etag: f.Etag,
			}
			if err := s.store.InsertDownloadTask(ctx, tx, t); err != nil {
				return err
			}
		}
		return nil
	}

	if err := add(weights, plan.Weights); err != nil {
		return err
	}
	if mmproj != nil && plan.Mmproj != nil {
		return add(*mmproj, *plan.Mmproj)
	}
	return nil
}

// guardDisk is section 7.4's disk guard: free space on the target filesystem
// must exceed `bytes_total - bytes_done + 1 GiB`, else `409 insufficient_disk`
// CARRYING THE NUMBERS. A refusal that does not say how much is needed and how
// much is there is a refusal a user cannot act on.
func (s *Service) guardDisk(hub string, needed int64) error {
	info, err := cache.Validate(hub, cache.ValidateOptions{})
	if err != nil {
		return model.Error{
			Code:    CodeNoCacheRoot,
			Message: fmt.Sprintf("the cache root %s is not usable: %v", hub, err),
		}
	}
	if info.FreeBytes <= 0 {
		// statfs said nothing. Refusing on an unknown is worse than proceeding:
		// the transfer itself fails with ENOSPC and reports honestly, where a
		// refusal here would block every download on a filesystem this daemon
		// cannot measure.
		return nil
	}
	want := needed + DiskHeadroom
	if info.FreeBytes >= want {
		return nil
	}
	return model.Error{
		Code: CodeInsufficientDisk,
		Message: fmt.Sprintf("%s has %s free and this download needs %s",
			hub, humanBytes(info.FreeBytes), humanBytes(want)),
		Details: map[string]any{
			"path":         hub,
			"free_bytes":   info.FreeBytes,
			"needed_bytes": want,
			"total_bytes":  needed,
			"headroom":     int64(DiskHeadroom),
		},
	}
}

// -----------------------------------------------------------------------------
// Reads
// -----------------------------------------------------------------------------

// View is one download with its per-file progress — `GET /api/v1/downloads/{id}`.
type View struct {
	store.Download
	// RepoID and Revision come from the model row, so a list does not need one
	// request per download to say what is being downloaded.
	RepoID   string
	Revision string
	// PrimaryFile is the weights model's shard 1, which is the name a user
	// recognizes the download by.
	PrimaryFile string
	Tasks       []store.DownloadTaskView
}

// Get returns one download with its tasks.
func (s *Service) Get(ctx context.Context, id string) (View, error) {
	var v View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		d, err := s.store.Download(ctx, tx, id)
		if err != nil {
			return err
		}
		v.Download = d
		tasks, err := s.store.DownloadTaskViews(ctx, tx, id)
		if err != nil {
			return err
		}
		v.Tasks = tasks
		m, err := s.store.LocalModel(ctx, tx, d.ModelID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		v.RepoID, v.Revision, v.PrimaryFile = m.RepoID, m.Revision, m.PrimaryFile
		return nil
	})
	return v, err
}

// List is `GET /api/v1/downloads?state=active|all`, with per-task progress.
func (s *Service) List(ctx context.Context, f store.DownloadFilter) ([]View, error) {
	var out []View
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.store.Downloads(ctx, tx, f)
		if err != nil {
			return err
		}
		out = make([]View, 0, len(rows))
		for _, d := range rows {
			v := View{Download: d}
			tasks, err := s.store.DownloadTaskViews(ctx, tx, d.ID)
			if err != nil {
				return err
			}
			v.Tasks = tasks
			m, err := s.store.LocalModel(ctx, tx, d.ModelID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			v.RepoID, v.Revision, v.PrimaryFile = m.RepoID, m.Revision, m.PrimaryFile
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// -----------------------------------------------------------------------------
// The control verbs
// -----------------------------------------------------------------------------

// Pause is `POST /api/v1/downloads/{id}/pause`.
//
// It moves the `jobs` row and the `downloads` row together, which is what
// section 2.3a requires and what `jobs.state='paused'` exists for. The running
// worker needs no signal: the queue's Pause releases the lease, the worker's next
// heartbeat finds it gone and cuts its own context, and the `.incomplete` files
// stand exactly where they are for the resume to continue from.
func (s *Service) Pause(ctx context.Context, id string) error {
	return s.control(ctx, id, "paused", func(jobID string) error {
		return s.queue.Pause(ctx, jobID, s.commitState(id, model.DownloadPaused, model.TaskPaused, nil, nil))
	}, model.DownloadQueued, model.DownloadResolving, model.DownloadRunning)
}

// Resume is `POST /api/v1/downloads/{id}/resume`: the job returns to `queued`
// and the same worker picks it up and continues from the byte each file reached.
func (s *Service) Resume(ctx context.Context, id string) error {
	return s.control(ctx, id, "resumed", func(jobID string) error {
		return s.queue.Resume(ctx, jobID, s.commitState(id, model.DownloadQueued, model.TaskQueued, nil, nil))
	}, model.DownloadPaused)
}

// Retry is `POST /api/v1/downloads/{id}/retry`: a stopped download runs again
// now, resuming from whatever is on disk.
//
// It accepts the same three states the queue's Retry does, and for the same
// reason: `failed`, `canceled` and `interrupted` are the three ways a job stops
// without being finished with, and a partial file is a head start rather than
// something to discard.
func (s *Service) Retry(ctx context.Context, id string) error {
	var jobID string
	err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		d, err := s.store.Download(ctx, tx, id)
		if err != nil {
			return err
		}
		switch d.State {
		case model.DownloadFailed, model.DownloadCanceled:
		default:
			return model.Error{
				Code:    CodeDownloadNotPausable,
				Message: fmt.Sprintf("a download in state %q cannot be retried", d.State),
			}
		}
		// The job of a download that STOPPED is by definition not live, so
		// LiveJobForSubject cannot find it: `failed` and `canceled` are terminal
		// states and that query filters them out. Retry is the one control verb
		// that has to reach a finished row, which is why it looks the subject's
		// jobs up by history — newest first — rather than by liveness.
		rows, err := s.store.Jobs(ctx, tx, store.JobFilter{
			SubjectType: model.SubjectDownload, SubjectID: id, Limit: 1,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return model.Error{
				Code:    CodeDownloadNotPausable,
				Message: "this download has no job to retry",
			}
		}
		jobID = rows[0].ID
		if _, err := s.store.BumpDownloadAttempts(ctx, tx, id); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := s.queue.Retry(ctx, jobID); err != nil {
		return err
	}
	return s.publishState(ctx, id, "retried", model.DownloadQueued)
}

// Cancel is `POST /api/v1/downloads/{id}/cancel?keep_partial=true`.
//
// keepPartial defaults to true at the API layer, so a canceled download can be
// retried without re-transferring what already landed. When it is false, the
// deletion is recorded here and performed by the WORKER on its way out — a
// second goroutine unlinking a file the transfer loop is still writing is a race
// with no upside, and the worker is the one process that knows when its own
// handle is closed.
func (s *Service) Cancel(ctx context.Context, id string, keepPartial bool) error {
	if !keepPartial {
		s.mu.Lock()
		s.dropped[id] = true
		s.mu.Unlock()
	}

	var (
		jobID string
		live  bool
	)
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		d, err := s.store.Download(ctx, tx, id)
		if err != nil {
			return err
		}
		if isTerminalDownload(d.State) {
			return model.Error{
				Code:    CodeDownloadNotPausable,
				Message: fmt.Sprintf("a download in state %q is already finished", d.State),
			}
		}
		j, err := s.store.LiveJobForSubject(ctx, tx, model.SubjectDownload, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			jobID, live = j.ID, true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !live {
		// No job holds it. Close the rows here, which is exactly what the queue
		// would have done through the worker's DomainWriter.
		return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			return s.commitState(id, model.DownloadCanceled, model.TaskCanceled, nil, nil)(
				ctx, tx, model.JobCanceled)
		})
	}
	if _, err := s.queue.Cancel(ctx, jobID); err != nil {
		return err
	}
	return nil
}

// SetPriority is `PATCH /api/v1/downloads/{id} {"priority":10}`.
//
// It moves BOTH rows in one transaction. The pool leases on `jobs.priority`, and
// a reorder that touched only `downloads.priority` would change the list the
// user reads without changing the order the worker actually works through — the
// most confusing possible outcome for a control whose whole purpose is order.
func (s *Service) SetPriority(ctx context.Context, id string, priority int) error {
	return s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.store.Download(ctx, tx, id); err != nil {
			return err
		}
		if _, err := s.store.SetDownloadPriority(ctx, tx, id, priority); err != nil {
			return err
		}
		j, err := s.store.LiveJobForSubject(ctx, tx, model.SubjectDownload, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = s.store.SetDownloadJobPriority(ctx, tx, j.ID, priority)
		return err
	})
}

// control is the shared shape of Pause and Resume: check the state, find the
// live job, apply the queue's transition, emit the event.
func (s *Service) control(ctx context.Context, id, action string,
	apply func(jobID string) error, allowed ...model.DownloadState) error {

	if s.queue == nil {
		return model.Error{
			Code:    CodeDownloadNotPausable,
			Message: "this daemon was built without a job queue",
		}
	}
	var jobID string
	err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		d, err := s.store.Download(ctx, tx, id)
		if err != nil {
			return err
		}
		ok := false
		for _, st := range allowed {
			if d.State == st {
				ok = true
				break
			}
		}
		if !ok {
			return model.Error{
				Code:    CodeDownloadNotPausable,
				Message: fmt.Sprintf("a download in state %q cannot be %s", d.State, action),
			}
		}
		j, err := s.store.LiveJobForSubject(ctx, tx, model.SubjectDownload, id)
		if err != nil {
			return err
		}
		jobID = j.ID
		return nil
	})
	if err != nil {
		return err
	}
	if err := apply(jobID); err != nil {
		return err
	}
	state := model.DownloadPaused
	if action == "resumed" {
		state = model.DownloadQueued
	}
	return s.publishState(ctx, id, action, state)
}

// commitState is the domain half of a job transition (section 2.3a): the
// `downloads` row and every non-terminal `download_tasks` row move in the SAME
// transaction the queue writes `jobs.state` in.
func (s *Service) commitState(id string, dstate model.DownloadState, tstate model.DownloadTaskState,
	errorCode, errorMessage *string) jobs.CommitFunc {

	return func(ctx context.Context, tx store.Tx, _ model.JobState) error {
		now := s.now().UnixMilli()
		var finished *int64
		if isTerminalDownload(dstate) {
			finished = &now
		}
		if _, err := s.store.SetDownloadTasksState(ctx, tx, id, tstate, finished); err != nil {
			return err
		}
		// The tasks were moved; the download's state is then the fold of them
		// (section 2.7). `dstate` above is not written — it survives only as the
		// caller's statement of intent, which the fold either agrees with or
		// corrects.
		_, err := s.writeState(ctx, tx, id, stateWrite{
			ErrorCode: errorCode, ErrorMessage: errorMessage,
		})
		return err
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (s *Service) enqueue(ctx context.Context, tx store.Tx, p jobs.EnqueueParams,
	subjectID string) (JobRef, error) {

	if s.queue == nil {
		// A binary built without a queue has the domain rows and nothing
		// scheduled. That is the honest state, and it is reported rather than
		// papered over with synchronous work that would violate section 3's
		// long-action rule and would not survive a restart.
		return JobRef{SubjectID: subjectID}, nil
	}
	res, err := s.queue.EnqueueTx(ctx, tx, p)
	if err != nil {
		return JobRef{}, err
	}
	return JobRef{JobID: res.Job.ID, Replayed: res.Replayed, SubjectID: subjectID}, nil
}

func (s *Service) appendEvent(ctx context.Context, tx store.Tx, ev model.Event) error {
	if s.events == nil {
		return nil
	}
	return s.events.Append(ctx, tx, ev)
}

func (s *Service) publishState(ctx context.Context, id, action string, state model.DownloadState) error {
	if s.events == nil {
		return nil
	}
	now := s.now()
	ev := model.Event{
		ID: s.newID(now), At: now.UnixMilli(), Level: model.LevelInfo,
		Category: model.CategoryDownload, Action: action,
		SubjectType: strPtr(string(model.SubjectDownload)), SubjectID: strPtr(id),
		ToState: strPtr(string(state)), Actor: model.ActorAdmin,
		Message: "download " + action,
	}
	if err := s.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		return s.events.Append(ctx, tx, ev)
	}); err != nil {
		return err
	}
	s.events.Publish(ev)
	return nil
}

// recompute is D69's call. A nil ConfigHashes is a binary built without the
// instance service, where there is nothing to recompute.
func (s *Service) recompute(ctx context.Context, tx store.Tx, modelIDs ...string) error {
	if s.hashes == nil {
		return nil
	}
	return s.hashes.RecomputeConfigHash(ctx, tx, modelIDs...)
}

// hubError maps a Hub failure onto the code section 3.6 answers with. It is one
// function because four call sites need the same mapping and a fifth that
// re-derived it would be the one that told a user their token was wrong when the
// repository was merely gated.
// ClassifyHubError is hubError exported, for the section 3.6 metadata endpoints.
//
// They classify a Hub failure exactly as `POST /downloads` does, because a gated
// repository must not read as `hf_gated` on the download button and as something
// else on the search result the user clicked to get there. One vocabulary, one
// function.
func ClassifyHubError(err error) error { return hubError(err) }

func hubError(err error) error {
	if err == nil {
		return nil
	}
	if g, ok := hf.IsGated(err); ok {
		return model.Error{
			Code:    CodeHFGated,
			Message: fmt.Sprintf("%s is gated; access must be granted on the Hub", g.Repo),
			Details: map[string]any{"repo": g.Repo, "request_url": g.RequestURL},
		}
	}
	if p, ok := hf.IsPrivate(err); ok {
		msg := "sign in with a Hugging Face token to access this repository"
		if p.HaveToken {
			msg = "the stored Hugging Face token cannot see this repository"
		}
		return model.Error{
			Code:    CodeHFPrivate,
			Message: msg,
			Details: map[string]any{"repo": p.Repo},
		}
	}
	if errors.Is(err, hf.ErrNotFound) {
		return model.Error{Code: CodeFileNotInRepo, Message: "no such repository, revision or file"}
	}
	if errors.Is(err, hf.ErrTokenInvalid) {
		return model.Error{
			Code:    CodeHFPrivate,
			Message: "the stored Hugging Face token was rejected; sign in again",
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return model.Error{Code: CodeHFUnreachable, Message: "the Hugging Face Hub could not be reached"}
}

func isTerminalDownload(s model.DownloadState) bool {
	switch s {
	case model.DownloadSucceeded, model.DownloadFailed, model.DownloadCanceled:
		return true
	}
	return false
}

func kindFor(requested model.ModelKind, g hf.FileGroup) model.ModelKind {
	if g.Mmproj {
		return model.ModelMmproj
	}
	if requested != "" && requested.Valid() {
		return requested
	}
	return model.ModelText
}

// refNameFor is `models.ref_name`, a DISPLAY field (section 7.2): the branch a
// user asked for, kept only when it is not itself the commit. A snapshot no ref
// names shows its short sha instead, so an empty string here is correct rather
// than missing.
func refNameFor(requested, commit string) string {
	if requested == "" {
		return "main"
	}
	if requested == commit {
		return ""
	}
	return requested
}

func looksLikeCommit(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for i := 0; i < len(rev); i++ {
		c := rev[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func priorityOr(p int) int {
	if p <= 0 {
		return 100
	}
	return p
}

func strPtr(s string) *string { return &s }

// humanBytes renders a byte count for a MESSAGE — never for a field. Section 3
// is explicit that byte counts on the wire are plain JSON numbers and formatting
// is the UI's job; this is the one exception, because an error message that says
// "40265318400 bytes" is a message nobody reads.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
