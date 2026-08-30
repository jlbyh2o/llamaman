package model

// Hugging Face cache, models and files (DESIGN section 2.6).

// CacheRootDetectedFrom is `hf_cache_roots.detected_from` (§2.6): which rule of
// the §7.2 six-rule chain found this hub directory. It is kept because the
// answer changes what the UI may say about the root — a root found through
// HF_HUB_CACHE is a hub directory with NO /hub suffix at all, and nothing may
// assume the suffix.
type CacheRootDetectedFrom string

const (
	DetectedFromHFHubCache   CacheRootDetectedFrom = "HF_HUB_CACHE"
	DetectedFromHFHome       CacheRootDetectedFrom = "HF_HOME"
	DetectedFromXDGCacheHome CacheRootDetectedFrom = "XDG_CACHE_HOME"
	DetectedFromDefault      CacheRootDetectedFrom = "default"
	DetectedFromLegacyEnv    CacheRootDetectedFrom = "legacy_env"
	DetectedFromDedicated    CacheRootDetectedFrom = "dedicated_user"
	DetectedFromManual       CacheRootDetectedFrom = "manual"
	DetectedFromSetting      CacheRootDetectedFrom = "setting"
)

// CacheRootDetectedFromValues lists the members of the
// `hf_cache_roots.detected_from` CHECK constraint, in order.
func CacheRootDetectedFromValues() []CacheRootDetectedFrom {
	return []CacheRootDetectedFrom{
		DetectedFromHFHubCache, DetectedFromHFHome, DetectedFromXDGCacheHome,
		DetectedFromDefault, DetectedFromLegacyEnv, DetectedFromDedicated,
		DetectedFromManual, DetectedFromSetting,
	}
}

// Valid reports whether d is a member of the CHECK constraint.
func (d CacheRootDetectedFrom) Valid() bool { return valid(d, CacheRootDetectedFromValues()) }

// ModelKind is `models.kind` (§2.6). `unknown` is a real member: a scan may find
// a GGUF it cannot classify yet, and refusing to record it would lose the file.
type ModelKind string

const (
	ModelText      ModelKind = "text"
	ModelEmbedding ModelKind = "embedding"
	ModelMmproj    ModelKind = "mmproj"
	ModelUnknown   ModelKind = "unknown"
)

// ModelKindValues lists the members of the `models.kind` CHECK constraint, in order.
func ModelKindValues() []ModelKind {
	return []ModelKind{ModelText, ModelEmbedding, ModelMmproj, ModelUnknown}
}

// Valid reports whether k is a member of the CHECK constraint.
func (k ModelKind) Valid() bool { return valid(k, ModelKindValues()) }

// ModelState is `models.state` (§2.6).
//
// Transitions:
//
//	planned → downloading → verifying → ready
//	downloading ⇄ incomplete                       (pause / resume)
//	ready → missing                                a scan found the files gone. NEVER silently
//	                                               deleted: a disk may be unplugged, and the row is
//	                                               how the user finds out which one
//	ready → corrupt                                verification failed
//	ready|missing|corrupt → deleting → deleted     under the in-use guard
//
// A scan may insert directly as `ready` with origin='scanned', which is the one
// entry point that skips the download states entirely.
type ModelState string

const (
	ModelPlanned     ModelState = "planned"
	ModelDownloading ModelState = "downloading"
	ModelIncomplete  ModelState = "incomplete"
	ModelVerifying   ModelState = "verifying"
	ModelReady       ModelState = "ready"
	ModelCorrupt     ModelState = "corrupt"
	ModelMissing     ModelState = "missing"
	ModelDeleting    ModelState = "deleting"
	ModelDeleted     ModelState = "deleted"
)

// ModelStateValues lists the members of the `models.state` CHECK constraint, in order.
func ModelStateValues() []ModelState {
	return []ModelState{
		ModelPlanned, ModelDownloading, ModelIncomplete, ModelVerifying,
		ModelReady, ModelCorrupt, ModelMissing, ModelDeleting, ModelDeleted,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s ModelState) Valid() bool { return valid(s, ModelStateValues()) }

// ModelOrigin is `models.origin` (§2.6): did Llama Man download this model, or
// did a cache scan find it already on disk?
type ModelOrigin string

const (
	OriginLlamaman ModelOrigin = "llamaman"
	OriginScanned  ModelOrigin = "scanned"
)

// ModelOriginValues lists the members of the `models.origin` CHECK constraint, in order.
func ModelOriginValues() []ModelOrigin { return []ModelOrigin{OriginLlamaman, OriginScanned} }

// Valid reports whether o is a member of the CHECK constraint.
func (o ModelOrigin) Valid() bool { return valid(o, ModelOriginValues()) }

// ModelFileState is `model_files.state` (§2.6): the per-file half of a model's
// state, which a sharded model folds upward from.
type ModelFileState string

const (
	FilePlanned     ModelFileState = "planned"
	FileDownloading ModelFileState = "downloading"
	FilePaused      ModelFileState = "paused"
	FileVerifying   ModelFileState = "verifying"
	FilePresent     ModelFileState = "present"
	FileMissing     ModelFileState = "missing"
	FileCorrupt     ModelFileState = "corrupt"
)

// ModelFileStateValues lists the members of the `model_files.state` CHECK
// constraint, in order.
func ModelFileStateValues() []ModelFileState {
	return []ModelFileState{
		FilePlanned, FileDownloading, FilePaused, FileVerifying, FilePresent,
		FileMissing, FileCorrupt,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s ModelFileState) Valid() bool { return valid(s, ModelFileStateValues()) }

// CacheScanState is `cache_scans.state` (§2.6). Scans are deliberately NOT
// pausable, which is why `paused` is absent here and the §2.3a mapping table
// leaves that cell empty.
type CacheScanState string

const (
	ScanQueued    CacheScanState = "queued"
	ScanRunning   CacheScanState = "running"
	ScanSucceeded CacheScanState = "succeeded"
	ScanFailed    CacheScanState = "failed"
	ScanCanceled  CacheScanState = "canceled"
)

// CacheScanStateValues lists the members of the `cache_scans.state` CHECK
// constraint, in order.
func CacheScanStateValues() []CacheScanState {
	return []CacheScanState{ScanQueued, ScanRunning, ScanSucceeded, ScanFailed, ScanCanceled}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s CacheScanState) Valid() bool { return valid(s, CacheScanStateValues()) }

// CacheScanTrigger is `cache_scans.trigger` (§2.6). Documented by a comment
// rather than closed by a CHECK, so it is absent from ClosedEnums.
type CacheScanTrigger string

const (
	ScanTriggerBoot         CacheScanTrigger = "boot"
	ScanTriggerWizard       CacheScanTrigger = "wizard"
	ScanTriggerManual       CacheScanTrigger = "manual"
	ScanTriggerPostDownload CacheScanTrigger = "post_download"
)

// CacheScanTriggerValues lists the triggers the design names, in the order of
// the column's comment.
func CacheScanTriggerValues() []CacheScanTrigger {
	return []CacheScanTrigger{
		ScanTriggerBoot, ScanTriggerWizard, ScanTriggerManual, ScanTriggerPostDownload,
	}
}

// Valid reports whether t is one of the triggers the design names.
func (t CacheScanTrigger) Valid() bool { return valid(t, CacheScanTriggerValues()) }

// StrayReason is `stray_files.reason` (§2.6): why a file in a cache root is not
// part of any model. Documented by a comment rather than closed by a CHECK, so
// it is absent from ClosedEnums.
type StrayReason string

const (
	StrayOutsideSnapshot StrayReason = "outside_snapshot"
	StrayOrphanBlob      StrayReason = "orphan_blob"
	StrayBrokenSymlink   StrayReason = "broken_symlink"
	StrayUnparsable      StrayReason = "unparsable"
)

// StrayReasonValues lists the reasons the design names, in the order of the
// column's comment.
func StrayReasonValues() []StrayReason {
	return []StrayReason{
		StrayOutsideSnapshot, StrayOrphanBlob, StrayBrokenSymlink, StrayUnparsable,
	}
}

// Valid reports whether r is one of the reasons the design names.
func (r StrayReason) Valid() bool { return valid(r, StrayReasonValues()) }

// CacheRoot is one row of `hf_cache_roots` (§2.6): a hub DIRECTORY Llama Man
// knows about, and the four facts about it the filesystem is the authority for.
//
// `Path` is the hub directory itself and need NOT end in `/hub` — rule 1 of §7.2
// produces one that does not, and §7.2a makes `settings['hf.hub_dir']` the
// authoritative setting for exactly that reason. Nothing may append or strip the
// suffix on its own.
type CacheRoot struct {
	ID   string
	Path string
	// IsPrimary marks the ONE root that receives downloads. Every other root is
	// scan-and-serve: read, listed, and served to instances, never written to.
	IsPrimary bool
	// Writable and SymlinksOK are measured, not read off the mode (F17). A
	// `writable=0` root can never be promoted (`422 root_not_writable`).
	Writable   bool
	SymlinksOK bool
	// DetectedFrom is which rule of §7.2's six-rule chain found this directory,
	// or `manual`/`setting` for one a human named. NULL for a row written
	// before the column meant anything.
	DetectedFrom *CacheRootDetectedFrom
	FSType       *string
	TotalBytes   *int64
	FreeBytes    *int64
	LastScanAt   *int64
	CreatedAt    int64
}

// LocalModel is one row of `models` (§2.6): one LOGICAL model — repo + revision
// + quant, possibly spanning shards.
//
// The name is `LocalModel` rather than `Model` for two reasons that are both
// about reading code, not about taste: `model.Model` says nothing at a use site,
// and the service that owns these rows is `internal/models`, whose own view
// types would otherwise sit one letter away from the domain row. "Local" is also
// the word §3.7 uses for the endpoint group — these are the models on THIS
// disk, as opposed to the Hugging Face search results of §3.6.
//
// Pointer fields are the nullable columns, and every one of them is a fact that
// may not have been learned yet rather than a zero: `NLayer == nil` means the
// GGUF has not been parsed, and `NLayer == 0` would mean a model with no layers.
type LocalModel struct {
	ID       string
	RootID   string
	RepoID   string
	Revision string
	// RefName is a DISPLAY field only (§7.2): the branch a `refs/` entry points
	// at this revision with. NULL for a snapshot no ref names, which is shown by
	// its short sha.
	RefName    *string
	QuantLabel *string
	Kind       ModelKind
	State      ModelState
	Origin     ModelOrigin
	// SnapshotDir and PrimaryFile are the resolved path, and they are
	// `config_hash` inputs (D52): the models service recomputes every
	// referencing instance's hash in the same transaction that moves either
	// (D69).
	SnapshotDir string
	PrimaryFile string
	ShardCount  int
	// TotalBytes is the model's logical size — what the fit calculator reads.
	// BytesOnDisk is what it occupies, which is what a delete frees. They differ
	// for a sparse or partially downloaded file, and §2.6 keeps both columns for
	// that reason.
	TotalBytes    int64
	BytesOnDisk   int64
	MmprojModelID *string
	// MmprojAuto records that the pairing was the scan's rather than a human's.
	// Any manual choice sets it to false, and a later rescan must not overrule
	// one (§7.2).
	MmprojAuto bool

	GGUFParsedAt *int64
	Arch         *string
	NLayer       *int64
	NCtxTrain    *int64
	NEmbd        *int64
	NFF          *int64
	NHead        *int64
	// NHeadKVJSON is the scalar or per-layer array VERBATIM (D30). It is stored
	// as written because §8.3 indexes it per layer and an averaged scalar would
	// mis-size a model whose layers differ.
	NHeadKVJSON *string
	HeadDimK    *int64
	HeadDimV    *int64
	NVocab      *int64
	NExpert     *int64
	NExpertUsed *int64
	// SWAWindow is NULL when `{arch}.attention.sliding_window` is ABSENT, which
	// §8.3 reads as "no sliding-window attention at all". It is deliberately not
	// zero, which would mean a window of width zero.
	SWAWindow      *int64
	SWAPattern     *int64
	TokenizerModel *string
	FileType       *string
	HasVision      bool

	MetadataJSON      *string
	TensorSummaryJSON *string
	HFGGUFJSON        *string
	CardFetchedAt     *int64
	LastVerifiedAt    *int64

	CreatedAt int64
	UpdatedAt int64
}

// ModelFile is one row of `model_files` (§2.6): one file of a logical model,
// which for a sharded set is one shard.
type ModelFile struct {
	ID         string
	ModelID    string
	Filename   string
	ShardIndex int
	ShardTotal int
	// SizeBytes is `lfs.size` before a download and the stat afterwards. §7.1 is
	// emphatic that the LFS size is the true one: the plain `size` of an LFS
	// entry can be the ~130-byte pointer.
	SizeBytes int64
	// Etag is the BLOB NAME — `x-linked-etag`, de-quoted and `W/`-stripped, and
	// equal to the sha256 hex for an LFS object. It is never sent in a header
	// (§7.4); the HTTP validator is a different column on a different table.
	Etag             *string
	BlobPath         *string
	LinkPath         *string
	BytesOnDisk      int64
	State            ModelFileState
	ChecksumVerified bool
	CreatedAt        int64
	UpdatedAt        int64
}

// StrayFile is one row of `stray_files` (§2.6): something in a cache root that
// belongs to no model. It is a row rather than a log line because the user is
// the one who decides — a 40 GB orphan blob may be another tool's work in
// progress, and `dismissed_at` is how they say so.
type StrayFile struct {
	ID          string
	RootID      string
	Path        string
	SizeBytes   int64
	Reason      StrayReason
	FirstSeenAt int64
	LastSeenAt  int64
	DismissedAt *int64
}

// CacheScan is one row of `cache_scans` (§2.6): one walk of one root, with the
// counters §7.2 updates every 250 ms. It is a domain row paired with a
// `cache_scan` job (§2.3a), which is what makes a scan survive a restart.
type CacheScan struct {
	ID            string
	RootID        string
	State         CacheScanState
	Trigger       CacheScanTrigger
	DirsSeen      int64
	FilesSeen     int64
	ModelsFound   int64
	ModelsAdded   int64
	ModelsMissing int64
	StraysFound   int64
	BytesTotal    int64
	ErrorMessage  *string
	StartedAt     *int64
	FinishedAt    *int64
	CreatedAt     int64
}

// InstanceRef names an instance that references something, for the two guards
// that have to report WHICH ones: `409 model_in_use` on a model delete (§7.2)
// and `409 model_in_use` on a cache-root detach (§7.2a).
//
// DeletedAt is carried because the two guards read it differently, and the
// difference is the whole of §7.2a's warning. Deleting a MODEL never issues a
// SQL DELETE — the row moves `deleting → deleted` and stays — so a soft-deleted
// instance is not a blocker there. Detaching a ROOT does issue one, `models`
// cascades away, and `instances.model_id` is ON DELETE RESTRICT — so a guard
// phrased over non-deleted instances would pass and the transaction would then
// fail inside SQLite with a raw foreign-key violation instead of the documented
// 409.
type InstanceRef struct {
	ID   string
	Name string
	// Role is which column referenced it: `model`, `mmproj` or `draft`. The UI
	// says "used as the draft model by inference-1", which is a materially
	// different sentence from "used by inference-1".
	Role      string
	DeletedAt *int64
}

// Deleted reports whether this reference comes from a soft-deleted instance.
func (r InstanceRef) Deleted() bool { return r.DeletedAt != nil }
