package models

import (
	"testing"

	"github.com/jlbyh2o/llamaman/internal/gguf"
	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// GroupSnapshot and PairMmproj are pure, so they are tested as a table with
// hand-built inputs — no disk, no database, no clock.

// entry builds one scanned file. shape is optional; the two shard signals
// (file NAME and `split.*` METADATA) are set independently so a test can make
// them disagree, which is the case section 7.2 needs both for.
func entry(name string, size int64, shape *gguf.Shape) cache.FileEntry {
	f := cache.FileEntry{Name: name, SizeBytes: size, BytesOnDisk: size, IsGGUF: cache.IsGGUF(name), Shape: shape}
	f.Shard, f.ShardOK = cache.ParseShardName(name)
	return f
}

func shape(arch string, split, of int) *gguf.Shape {
	s := &gguf.Shape{Architecture: arch, BlockCount: 2}
	if of > 0 {
		s.SplitNo, s.SplitCount = split-1, of
	}
	return s
}

func TestGroupSnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		files []cache.FileEntry
		// want maps the expected primary file to its shard count.
		want map[string]int
	}{
		{
			name: "a single unsharded file",
			files: []cache.FileEntry{
				entry("Model-Q4_K_M.gguf", 100, shape("llama", 0, 0)),
			},
			want: map[string]int{"Model-Q4_K_M.gguf": 1},
		},
		{
			name: "shards group by the file name suffix",
			files: []cache.FileEntry{
				entry("Model-Q4_K_M-00001-of-00003.gguf", 100, nil),
				entry("Model-Q4_K_M-00002-of-00003.gguf", 100, nil),
				entry("Model-Q4_K_M-00003-of-00003.gguf", 100, nil),
			},
			want: map[string]int{"Model-Q4_K_M-00001-of-00003.gguf": 3},
		},
		{
			// Two quants of one repo+revision are two logical models. They are
			// separately deletable and separately referenceable by an instance.
			name: "two quants stay two models",
			files: []cache.FileEntry{
				entry("Model-Q4_K_M.gguf", 100, shape("llama", 0, 0)),
				entry("Model-Q8_0.gguf", 200, shape("llama", 0, 0)),
			},
			want: map[string]int{"Model-Q4_K_M.gguf": 1, "Model-Q8_0.gguf": 1},
		},
		{
			// Two sharded quants side by side: the base is what separates them,
			// and a rule keyed on the suffix alone would fuse all six files.
			name: "two sharded quants stay two models",
			files: []cache.FileEntry{
				entry("M-Q4_K_M-00001-of-00002.gguf", 100, nil),
				entry("M-Q4_K_M-00002-of-00002.gguf", 100, nil),
				entry("M-Q8_0-00001-of-00002.gguf", 200, nil),
				entry("M-Q8_0-00002-of-00002.gguf", 200, nil),
			},
			want: map[string]int{
				"M-Q4_K_M-00001-of-00002.gguf": 2,
				"M-Q8_0-00001-of-00002.gguf":   2,
			},
		},
		{
			// The header is the stronger signal: `split.count` is what the
			// loader itself reads, so a name claiming 2 and metadata claiming 3
			// resolves to 3 and the row records that one shard is missing.
			name: "split.count overrides the name's total",
			files: []cache.FileEntry{
				entry("M-00001-of-00002.gguf", 100, shape("llama", 1, 3)),
				entry("M-00002-of-00002.gguf", 100, shape("llama", 2, 3)),
			},
			want: map[string]int{"M-00001-of-00002.gguf": 3},
		},
		{
			// Non-GGUF files belong to the repository and are not models. They
			// are not strays either — huggingface_hub put them there.
			name: "non-GGUF files produce no group",
			files: []cache.FileEntry{
				entry("README.md", 10, nil),
				entry("config.json", 10, nil),
				entry("Model.gguf", 100, shape("llama", 0, 0)),
			},
			want: map[string]int{"Model.gguf": 1},
		},
		{
			name: "a subdirectory is part of the identity",
			files: []cache.FileEntry{
				entry("a/Model.gguf", 100, shape("llama", 0, 0)),
				entry("b/Model.gguf", 100, shape("llama", 0, 0)),
			},
			want: map[string]int{"a/Model.gguf": 1, "b/Model.gguf": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := GroupSnapshot(tc.files)
			if len(got) != len(tc.want) {
				var names []string
				for _, g := range got {
					names = append(names, g.PrimaryFile)
				}
				t.Fatalf("groups = %v, want %d", names, len(tc.want))
			}
			for _, g := range got {
				want, ok := tc.want[g.PrimaryFile]
				if !ok {
					t.Fatalf("unexpected group with primary file %q", g.PrimaryFile)
				}
				if g.ShardCount != want {
					t.Fatalf("%q shard count = %d, want %d", g.PrimaryFile, g.ShardCount, want)
				}
			}
		})
	}
}

// TestGroupSnapshotPrimaryIsShardOne: the primary file is what llama.cpp is
// handed, and pointing it at shard 3 loads nothing.
func TestGroupSnapshotPrimaryIsShardOne(t *testing.T) {
	t.Parallel()

	// Out of order on purpose: the walk sorts by name, but a producer may write
	// a set the sort does not order.
	got := GroupSnapshot([]cache.FileEntry{
		entry("M-00003-of-00003.gguf", 100, nil),
		entry("M-00001-of-00003.gguf", 100, nil),
		entry("M-00002-of-00003.gguf", 100, nil),
	})
	if len(got) != 1 {
		t.Fatalf("groups = %d, want 1", len(got))
	}
	if got[0].PrimaryFile != "M-00001-of-00003.gguf" {
		t.Fatalf("primary file = %q, want shard 1", got[0].PrimaryFile)
	}
	if got[0].TotalBytes != 300 {
		t.Fatalf("total bytes = %d, want the sum of every shard", got[0].TotalBytes)
	}
	if !got[0].Complete {
		t.Fatal("a full set was reported incomplete")
	}
}

func TestGroupSnapshotIncompleteAndBroken(t *testing.T) {
	t.Parallel()

	missing := GroupSnapshot([]cache.FileEntry{
		entry("M-00001-of-00003.gguf", 100, nil),
		entry("M-00002-of-00003.gguf", 100, nil),
	})
	if missing[0].Complete {
		t.Fatal("a set missing its third shard was reported complete")
	}
	if missing[0].ShardCount != 3 {
		t.Fatalf("shard count = %d, want the declared 3", missing[0].ShardCount)
	}

	broken := entry("M-00002-of-00002.gguf", 100, nil)
	broken.Broken = true
	withBroken := GroupSnapshot([]cache.FileEntry{
		entry("M-00001-of-00002.gguf", 100, nil),
		broken,
	})
	if withBroken[0].Complete {
		t.Fatal("a set with a broken link was reported complete")
	}
}

func TestPairMmproj(t *testing.T) {
	t.Parallel()

	proj := func(name, quant string) Group {
		return Group{PrimaryFile: name, QuantLabel: quant, Kind: model.ModelMmproj}
	}
	weights := func(quant string) Group {
		return Group{PrimaryFile: "model.gguf", QuantLabel: quant, Kind: model.ModelText}
	}

	cases := []struct {
		name       string
		weights    Group
		candidates []Group
		want       string // "" means "no pairing — show a picker"
	}{
		{
			name: "no candidates", weights: weights("Q4_K_M"), want: "",
		},
		{
			// One candidate is attached whatever its precision: there is
			// nothing to guess between.
			name: "exactly one candidate", weights: weights("Q4_K_M"),
			candidates: []Group{proj("mmproj-f32.gguf", "F32")},
			want:       "mmproj-f32.gguf",
		},
		{
			name: "a precision matching the weights wins", weights: weights("Q8_0"),
			candidates: []Group{
				proj("mmproj-f16.gguf", "F16"),
				proj("mmproj-q8.gguf", "Q8_0"),
			},
			want: "mmproj-q8.gguf",
		},
		{
			name: "f16 beats f32", weights: weights("Q4_K_M"),
			candidates: []Group{
				proj("mmproj-f32.gguf", "F32"),
				proj("mmproj-f16.gguf", "F16"),
			},
			want: "mmproj-f16.gguf",
		},
		{
			// A tie at the best tier is a PICKER, not a coin flip. Breaking it
			// by file name would be exactly the guess section 7.2 refuses.
			name: "two f16 projectors are a tie", weights: weights("Q4_K_M"),
			candidates: []Group{
				proj("mmproj-vision-f16.gguf", "F16"),
				proj("mmproj-audio-f16.gguf", "F16"),
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := PairMmproj(tc.weights, tc.candidates)
			if tc.want == "" {
				if ok {
					t.Fatalf("PairMmproj chose %q, want a picker", got.PrimaryFile)
				}
				return
			}
			if !ok {
				t.Fatalf("PairMmproj chose nothing, want %q", tc.want)
			}
			if got.PrimaryFile != tc.want {
				t.Fatalf("PairMmproj chose %q, want %q", got.PrimaryFile, tc.want)
			}
		})
	}
}
