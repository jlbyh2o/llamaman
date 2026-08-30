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
