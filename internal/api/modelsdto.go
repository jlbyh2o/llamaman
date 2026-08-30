package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
)

// The DTO conversions for DESIGN section 3.7. They live beside the routes and
// nowhere else: storage form in, wire form out, once.

func modelDTO(v models.View) ModelDTO {
	m := v.LocalModel
	d := ModelDTO{
		ID: m.ID, RootID: m.RootID, RootPath: v.RootPath, RepoID: m.RepoID,
		Revision: m.Revision, RefName: m.RefName, QuantLabel: m.QuantLabel,
		Kind: string(m.Kind), State: string(m.State), Origin: string(m.Origin),
		SnapshotDir: m.SnapshotDir, PrimaryFile: m.PrimaryFile,
		Path:       modelPath(m),
		ShardCount: m.ShardCount, TotalBytes: m.TotalBytes, BytesOnDisk: m.BytesOnDisk,
		MmprojModelID: m.MmprojModelID, MmprojAuto: m.MmprojAuto, HasVision: m.HasVision,
		Arch: m.Arch, NLayer: m.NLayer, NCtxTrain: m.NCtxTrain, NEmbd: m.NEmbd,
		NFF: m.NFF, NHead: m.NHead, NVocab: m.NVocab, NExpert: m.NExpert,
		NExpertUsed: m.NExpertUsed, HeadDimK: m.HeadDimK, HeadDimV: m.HeadDimV,
		SWAWindow: m.SWAWindow, SWAPattern: m.SWAPattern,
		TokenizerModel: m.TokenizerModel, FileType: m.FileType,
		NHeadKV:       rawJSON(m.NHeadKVJSON),
		TensorSummary: rawJSON(m.TensorSummaryJSON),
		GGUFParsedAt:  TimePtr(m.GGUFParsedAt), LastVerifiedAt: TimePtr(m.LastVerifiedAt),
		CreatedAt: Time(m.CreatedAt), UpdatedAt: Time(m.UpdatedAt),
		InUseBy: instanceRefDTOs(v.InUseBy),
	}
	return d
}

func modelDetailDTO(d models.Detail) ModelDetailDTO {
	out := ModelDetailDTO{Model: modelDTO(d.View)}
	out.Files = make([]ModelFileDTO, 0, len(d.Files))
	for _, f := range d.Files {
		out.Files = append(out.Files, ModelFileDTO{
			ID: f.ID, Filename: f.Filename, ShardIndex: f.ShardIndex, ShardTotal: f.ShardTotal,
			SizeBytes: f.SizeBytes, BytesOnDisk: f.BytesOnDisk, Etag: f.Etag,
			BlobPath: f.BlobPath, LinkPath: f.LinkPath, State: string(f.State),
			ChecksumVerified: f.ChecksumVerified,
		})
	}
	if d.Mmproj != nil {
		mm := modelDTO(models.View{LocalModel: *d.Mmproj, RootPath: d.RootPath})
		out.Mmproj = &mm
	}
	out.MmprojCandidates = make([]ModelDTO, 0, len(d.MmprojCandidates))
	for _, c := range d.MmprojCandidates {
		out.MmprojCandidates = append(out.MmprojCandidates,
			modelDTO(models.View{LocalModel: c, RootPath: d.RootPath}))
	}
	return out
}

func deletePreviewDTO(p models.DeletePlan) DeletePreviewDTO {
	return DeletePreviewDTO{
		Files: p.Files, Bytes: p.Bytes,
		BlobsSharedKept: p.BlobsSharedKept, SharedBytes: p.SharedBytes,
		RemovesRepoDir: p.RemovesRepoDir,
		InUseBy:        instanceRefDTOs(p.InUseBy),
	}
}

func cacheRootDTO(r models.RootView) CacheRootDTO {
	return CacheRootDTO{
		ID: r.ID, Path: r.Path, HFHome: r.HFHome,
		IsPrimary: r.IsPrimary, Writable: r.Writable, SymlinksOK: r.SymlinksOK,
		DetectedAs: enumString(r.DetectedFrom),
		FSType:     r.FSType, TotalBytes: r.TotalBytes, FreeBytes: r.FreeBytes,
		Models: r.Models, BytesOnDisk: r.BytesOnDisk,
		LastScanAt: TimePtr(r.LastScanAt), CreatedAt: Time(r.CreatedAt),
	}
}

func cacheScanDTO(c model.CacheScan) CacheScanDTO {
	return CacheScanDTO{
		ID: c.ID, RootID: c.RootID, State: string(c.State), Trigger: string(c.Trigger),
		DirsSeen: c.DirsSeen, FilesSeen: c.FilesSeen, ModelsFound: c.ModelsFound,
		ModelsAdded: c.ModelsAdded, ModelsMissing: c.ModelsMissing,
		StraysFound: c.StraysFound, BytesTotal: c.BytesTotal,
		ErrorMessage: c.ErrorMessage,
		StartedAt:    TimePtr(c.StartedAt), FinishedAt: TimePtr(c.FinishedAt),
		CreatedAt: Time(c.CreatedAt),
	}
}

func strayDTO(st model.StrayFile) StrayFileDTO {
	return StrayFileDTO{
		ID: st.ID, RootID: st.RootID, Path: st.Path, SizeBytes: st.SizeBytes,
		Reason:      string(st.Reason),
		FirstSeenAt: Time(st.FirstSeenAt), LastSeenAt: Time(st.LastSeenAt),
		DismissedAt: TimePtr(st.DismissedAt),
	}
}

func instanceRefDTOs(refs []model.InstanceRef) []InstanceRefDTO {
	out := make([]InstanceRefDTO, 0, len(refs))
	for _, r := range refs {
		out = append(out, InstanceRefDTO{ID: r.ID, Name: r.Name, Role: r.Role, Deleted: r.Deleted()})
	}
	return out
}

// jobReceiptDTO renders section 3's long-action shape. A JobRef with no job id
// is a daemon built without a queue: the field is null rather than an empty
// string, so a client cannot mistake it for an id it could poll.
func jobReceiptDTO(ref models.JobRef, subjectType string) JobReceiptDTO {
	out := JobReceiptDTO{Subject: SubjectDTO{Type: subjectType, ID: ref.SubjectID}}
	if ref.JobID != "" {
		id := ref.JobID
		out.JobID = &id
	}
	return out
}

// optionalReceipt is the same, as a pointer, for a response whose job is a
// side effect rather than its subject — the scan queued by registering a root.
func optionalReceipt(ref models.JobRef, subjectType string) *JobReceiptDTO {
	if ref.JobID == "" && ref.SubjectID == "" {
		return nil
	}
	r := jobReceiptDTO(ref, subjectType)
	return &r
}

// modelPath is the resolved file llama.cpp would be handed. It is computed for
// the wire rather than stored, so it can never disagree with the two columns it
// is built from.
func modelPath(m model.LocalModel) string {
	if m.SnapshotDir == "" || m.PrimaryFile == "" {
		return ""
	}
	return filepath.Join(m.SnapshotDir, filepath.FromSlash(m.PrimaryFile))
}

// rawJSON hands a stored JSON column through to the wire without re-typing it.
//
// `n_head_kv_json` is a scalar OR an array (D30) and `tensor_summary_json` is an
// object; re-decoding either into a Go type and re-encoding it would force a
// choice the column deliberately does not make. A column that does not parse
// comes back null rather than failing the response — one malformed old row must
// not 500 a catalog listing.
func rawJSON(raw *string) any {
	if raw == nil {
		return nil
	}
	var out any
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}

// enumList reads a repeatable, comma-separated enum filter: `?state=ready,missing`
// and `?state=ready&state=missing` mean the same thing.
//
// An unrecognized value is passed through rather than dropped. The store's IN
// list simply will not match it, which is the honest answer for a filter naming
// a state that does not exist — dropping it would silently widen the query to
// "everything", which is the opposite of what the caller asked.
func enumList[T ~string](r *http.Request, name string) []T {
	var out []T
	for _, raw := range r.URL.Query()[name] {
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, T(part))
			}
		}
	}
	return out
}

// enumStrings widens a slice of string-kinded enum values for the route
// registry's `Enum` documentation field.
func enumStrings[T ~string](vs []T) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, string(v))
	}
	return out
}
