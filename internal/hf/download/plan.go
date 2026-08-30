package download

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Expanding a request into a file set (DESIGN section 7.3).
//
// "A download is created for a LOGICAL model. The user picks a quant; the API
// expands it to every shard plus, when `include_mmproj`, the chosen mmproj —
// which becomes a SEPARATE `models` row of `kind='mmproj'` linked by
// `mmproj_model_id`, because it is separately reusable. `model_files` rows are
// created up front in `state='planned'` with `size_bytes` from `lfs.size`, so
// total bytes and ETA are exact from the first byte, and the queue REFUSES A
// PARTIAL SHARD SET."
//
// The refusal is the part worth being explicit about. A repository mid-upload
// legitimately advertises `-00003-of-00005` with two shards missing; queueing
// that produces a download that runs for an hour, succeeds, and leaves a model
// llama.cpp cannot load. Failing at the click costs a user one second and an
// intelligible message.

// Plan is one expanded download request: the weights, an optional projector, and
// the totals that make the ETA exact from the first byte.
type Plan struct {
	// Weights is the shard set the user picked.
	Weights hf.FileGroup
	// Mmproj is the projector to fetch alongside it, or nil.
	Mmproj *hf.FileGroup
	// TotalBytes is both groups' true sizes summed — what the disk guard checks
	// and what the progress bar divides by.
	TotalBytes int64
}

// Files returns every file the plan downloads, weights first.
func (p Plan) Files() []hf.TreeEntry {
	out := make([]hf.TreeEntry, 0, len(p.Weights.Files)+2)
	out = append(out, p.Weights.Files...)
	if p.Mmproj != nil {
		out = append(out, p.Mmproj.Files...)
	}
	return out
}

// PrimaryFile is `models.primary_file`: shard 1, or the single file. It is a
// file NAME relative to the snapshot directory, which is what section 2.6's
// column holds and what `UNIQUE(root_id, repo_id, revision, primary_file)`
// scopes a model's identity by.
//
// It is shard 1 rather than "the largest" or "the first alphabetically" because
// only shard 1 carries the metadata: section 7.3's "only shard 1 is needed to
// parse metadata" is the same fact that makes it the file llama.cpp is handed,
// since llama.cpp opens the first shard and finds the rest by name.
func PrimaryFile(g hf.FileGroup) string {
	if len(g.Files) == 0 {
		return ""
	}
	return g.Files[0].Path
}

// ExpandRequest turns the files a client asked for into a Plan, against a tree
// this daemon just fetched.
//
// requested may name any file of a shard set — a user who clicked "shard 3" gets
// the whole set, because a partial set is not a model. An empty list is an
// error rather than "everything": downloading every quantization of a repository
// is never what a click meant, and a repository can hold two hundred gigabytes
// of them.
func ExpandRequest(entries []hf.TreeEntry, requested []string, includeMmproj bool,
	mmprojFile string) (Plan, error) {

	if len(requested) == 0 {
		return Plan{}, model.Error{
			Code:    CodeNoFilesSelected,
			Message: "a download must name at least one file",
		}
	}
	for _, f := range requested {
		if err := hf.ValidateFilePath(f); err != nil {
			return Plan{}, model.Error{
				Code:    CodeFileNotInRepo,
				Message: err.Error(),
			}
		}
	}

	groups := hf.GroupTree(entries)
	byFile := map[string]int{}
	for i, g := range groups {
		for _, f := range g.Files {
			byFile[f.Path] = i
		}
	}

	// Every requested file must resolve to the SAME group. Two quantizations in
	// one download would be two logical models sharing one `downloads` row and
	// one progress bar, and section 2.7's `downloads.model_id` is singular for
	// exactly that reason.
	var (
		weights *hf.FileGroup
		mmproj  *hf.FileGroup
	)
	seen := map[int]bool{}
	for _, f := range requested {
		idx, ok := byFile[f]
		if !ok {
			return Plan{}, model.Error{
				Code:    CodeFileNotInRepo,
				Message: fmt.Sprintf("%s is not a GGUF file in this repository at this revision", f),
				Details: map[string]any{"file": f},
			}
		}
		seen[idx] = true
	}
	for idx := range seen {
		g := groups[idx]
		switch {
		case g.Mmproj && weights != nil:
			// The client named the projector explicitly alongside the weights.
			// That is the same request as `include_mmproj`, so it is honored
			// rather than refused.
			c := g
			mmproj = &c
		case g.Mmproj && len(seen) == 1:
			// A projector on its own is a legitimate download: section 7.3 says
			// it is separately reusable, and pairing it with an existing model
			// later is what `POST /models/{id}/pair-mmproj` is for.
			c := g
			weights = &c
		case weights != nil:
			return Plan{}, model.Error{
				Code: CodeMultipleQuants,
				Message: "a download names one quantization; " +
					"queue a second download for the other",
			}
		default:
			c := g
			weights = &c
		}
	}
	if weights == nil {
		// Only projectors were named, and more than one of them.
		return Plan{}, model.Error{
			Code:    CodeMultipleQuants,
			Message: "a download names one quantization",
		}
	}
	if !weights.Complete {
		return Plan{}, model.Error{
			Code: CodeShardSetIncomplete,
			Message: fmt.Sprintf(
				"%s declares %d shards and the repository holds %d at this revision",
				weights.Key, weights.ShardTotal, len(weights.Files)),
			Details: map[string]any{
				"key": weights.Key, "declared": weights.ShardTotal, "present": len(weights.Files),
			},
		}
	}

	if mmproj == nil && includeMmproj && !weights.Mmproj {
		picked, err := PickMmproj(groups, weights.QuantLabel, mmprojFile)
		if err != nil {
			return Plan{}, err
		}
		mmproj = picked
	}
	if mmproj != nil && !mmproj.Complete {
		return Plan{}, model.Error{
			Code:    CodeShardSetIncomplete,
			Message: fmt.Sprintf("the projector %s is missing shards", mmproj.Key),
		}
	}

	p := Plan{Weights: *weights, Mmproj: mmproj, TotalBytes: weights.TotalBytes}
	if mmproj != nil {
		p.TotalBytes += mmproj.TotalBytes
	}
	return p, nil
}

// PickMmproj implements section 7.2's auto-pairing preference, applied to a
// REMOTE tree rather than to a scanned snapshot: prefer a precision matching the
// weights, then `f16`, then `f32`.
//
// Several candidates with no preference between them produce a picker rather
// than a guess — the same rule the scan follows — which here means an error the
// API turns into "choose one", because guessing wrong costs the user a
// multi-gigabyte download of the wrong projector.
//
// explicit, when non-empty, is the user's own choice and short-circuits all of
// it.
func PickMmproj(groups []hf.FileGroup, weightsQuant, explicit string) (*hf.FileGroup, error) {
	var candidates []hf.FileGroup
	for _, g := range groups {
		if g.Mmproj {
			candidates = append(candidates, g)
		}
	}
	if explicit != "" {
		for _, g := range candidates {
			for _, f := range g.Files {
				if f.Path == explicit {
					c := g
					return &c, nil
				}
			}
		}
		return nil, model.Error{
			Code:    CodeFileNotInRepo,
			Message: fmt.Sprintf("%s is not a projector in this repository", explicit),
			Details: map[string]any{"file": explicit},
		}
	}
	switch len(candidates) {
	case 0:
		// No projector in the repository is not an error: `include_mmproj` is
		// on by default and most repositories are text-only.
		return nil, nil
	case 1:
		c := candidates[0]
		return &c, nil
	}

	for _, want := range []string{strings.ToUpper(weightsQuant), "F16", "F32"} {
		if want == "" {
			continue
		}
		var matches []hf.FileGroup
		for _, g := range candidates {
			if strings.EqualFold(g.QuantLabel, want) ||
				strings.Contains(strings.ToUpper(path.Base(g.Files[0].Path)), "-"+want) {
				matches = append(matches, g)
			}
		}
		if len(matches) == 1 {
			c := matches[0]
			return &c, nil
		}
	}

	names := make([]string, 0, len(candidates))
	for _, g := range candidates {
		names = append(names, g.Files[0].Path)
	}
	sort.Strings(names)
	return nil, model.Error{
		Code:    CodeMmprojAmbiguous,
		Message: "this repository has several projectors; choose one",
		Details: map[string]any{"candidates": names},
	}
}

// ShardIndex reports a file's place in its set, for the `model_files` row.
// A file whose name declares no shard is 1 of 1 — a set of one, which is what
// every downstream query treats an unsharded model as.
func ShardIndex(filename string, total int) (index, shardTotal int) {
	if s, ok := cache.ParseShardName(path.Base(filename)); ok {
		return s.Index, s.Total
	}
	if total <= 0 {
		total = 1
	}
	return 1, total
}
