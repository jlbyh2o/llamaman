package models

import (
	"path"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Shard grouping and mmproj pairing (DESIGN sections 7.2, 7.3).
//
// This file is PURE: it takes the files one snapshot directory holds and returns
// the logical models they make up. No database, no filesystem, no clock — which
// is what lets the two rules that are easiest to get subtly wrong be tested as a
// table.

// Group is one logical model within one snapshot: a `models` row and the
// `model_files` rows under it.
type Group struct {
	// PrimaryFile is `models.primary_file` — shard 1, or the single file. It is
	// what llama.cpp is handed: pointing it at the first shard is how a split
	// model is loaded, and pointing it at shard 3 loads nothing.
	PrimaryFile string
	// Files are the group's members in shard order.
	Files []cache.FileEntry
	// ShardCount is the set size. It is the DECLARED total when the files agree
	// with it, and the number actually found when they do not — a set missing
	// its third shard reports 3, so the row records that one is missing rather
	// than describing a two-shard model that does not exist.
	ShardCount int
	// ShardTotal is what the file names and `split.count` declared.
	ShardTotal int
	// Complete reports that every declared shard was found and none was broken.
	// An incomplete group is still a row — a half-downloaded model the user can
	// see and resume beats one the catalog pretends is not there.
	Complete bool

	TotalBytes  int64
	BytesOnDisk int64

	Kind       model.ModelKind
	QuantLabel string
	// Shape is the primary shard's parsed header, nil when it did not parse.
	// Only shard 1 is needed for metadata (§7.3), which is why the fit panel
	// becomes exact as soon as the first shard lands.
	Shape *gguf.Shape
}

// GroupSnapshot turns one snapshot directory's files into logical models.
//
// Grouping is by the `-NNNNN-of-NNNNN.gguf` FILE NAME suffix and by the
// `split.*` METADATA, and it has to be both. A producer that wrote the metadata
// but used a different naming convention, and one that used the convention
// without writing the metadata, are both in the wild; keying on either alone
// splits one model into five or fuses five into one.
//
// Non-GGUF files in the snapshot — a README, a tokenizer.json, a config.json —
// are not models and produce no group. They are not strays either: they belong
// to the repository and `huggingface_hub` put them there.
func GroupSnapshot(files []cache.FileEntry) []Group {
	type bucket struct {
		key      string
		members  []cache.FileEntry
		declared int
	}
	var (
		order   []string
		buckets = map[string]*bucket{}
	)

	for _, f := range files {
		if !f.IsGGUF {
			continue
		}
		key, declared := shardKey(f)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{key: key}
			buckets[key] = b
			order = append(order, key)
		}
		b.members = append(b.members, f)
		if declared > b.declared {
			b.declared = declared
		}
	}

	out := make([]Group, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		sort.SliceStable(b.members, func(i, j int) bool {
			return shardIndex(b.members[i]) < shardIndex(b.members[j])
		})

		g := Group{Files: b.members, ShardTotal: b.declared}
		if g.ShardTotal < 1 {
			g.ShardTotal = 1
		}
		g.ShardCount = max(g.ShardTotal, len(b.members))

		primary := b.members[0]
		g.PrimaryFile = primary.Name
		g.Shape = primary.Shape
		g.Kind = cache.Classify(primary.Name, primary.Shape)
		g.QuantLabel = cache.QuantLabel(path.Base(primary.Name), primary.Shape)

		g.Complete = len(b.members) == g.ShardTotal
		for _, f := range b.members {
			g.TotalBytes += f.SizeBytes
			g.BytesOnDisk += f.BytesOnDisk
			if f.Broken {
				g.Complete = false
			}
		}
		out = append(out, g)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].PrimaryFile < out[j].PrimaryFile })
	return out
}

// shardKey is the identity every shard of one set shares, and the declared set
// size the file claims. The key carries the file's DIRECTORY as well as its
// base, so two identically named quants in different subdirectories of one
// snapshot stay two models.
func shardKey(f cache.FileEntry) (key string, declared int) {
	dir := path.Dir(f.Name)
	base := strings.TrimSuffix(path.Base(f.Name), cache.GGUFExtension)

	if f.ShardOK {
		base = f.Shard.Base
		declared = f.Shard.Total
	}
	// The header is the stronger signal and overrides the name's count when the
	// two disagree — `split.count` is what the loader itself reads.
	if f.Shape != nil && f.Shape.SplitCount > 1 {
		declared = f.Shape.SplitCount
		if !f.ShardOK {
			// A producer that split the file without using the naming
			// convention. Nothing can be stripped safely — a set whose names
			// have nothing in common AND no suffix cannot be grouped by any
			// rule — so the base stays as it is and the files land in separate
			// groups, each honestly reporting the declared count it carries.
			// That is a worse catalog than grouping would give, and it is still
			// better than fusing two unrelated quants because both said
			// `split.count = 2`.
			_ = base
		}
	}
	return dir + "/" + base, declared
}

// shardIndex is a file's 1-based place in its set, preferring the header's
// `split.no` over the file name for the same reason shardKey does.
func shardIndex(f cache.FileEntry) int {
	if f.Shape != nil && f.Shape.SplitCount > 1 {
		return f.Shape.SplitNo + 1
	}
	if f.ShardOK {
		return f.Shard.Index
	}
	return 1
}

// PairMmproj chooses the projector for one weights group out of the candidates
// in the same repo+revision, and reports whether it chose one.
//
// Section 7.2's rule, and the reading this implements:
//
//	"an mmproj is attached when exactly one candidate exists, preferring a
//	 precision matching the weights, then f16, then f32. Several candidates
//	 produce a picker rather than a guess."
//
// One candidate is attached. Several are ranked by that preference, and a
// choice is made only when the BEST tier holds exactly one candidate. Two f16
// projectors are a tie, and breaking a tie by file name would be precisely the
// "guess" the rule refuses — the UI shows a picker instead, and any manual
// choice sets `mmproj_auto = 0` so no later scan overrules it.
func PairMmproj(weights Group, candidates []Group) (Group, bool) {
	switch len(candidates) {
	case 0:
		return Group{}, false
	case 1:
		return candidates[0], true
	}

	best := mmprojRank(weights.QuantLabel, candidates[0].QuantLabel)
	winners := []Group{candidates[0]}
	for _, c := range candidates[1:] {
		switch r := mmprojRank(weights.QuantLabel, c.QuantLabel); {
		case r < best:
			best, winners = r, []Group{c}
		case r == best:
			winners = append(winners, c)
		}
	}
	if len(winners) != 1 {
		return Group{}, false
	}
	return winners[0], true
}

// mmprojRank scores a candidate: 0 for a precision matching the weights, 1 for
// f16, 2 for f32, 3 for anything else. Lower wins.
func mmprojRank(weightsQuant, candidateQuant string) int {
	q := strings.ToUpper(candidateQuant)
	switch {
	case weightsQuant != "" && strings.EqualFold(weightsQuant, candidateQuant):
		return 0
	case q == "F16" || q == "BF16":
		return 1
	case q == "F32":
		return 2
	default:
		return 3
	}
}
