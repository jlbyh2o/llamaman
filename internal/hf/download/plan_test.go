package download

import (
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/hf"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Expansion and grouping (DESIGN section 7.3).

// tree is the file set every case below draws from: one single-file quant, a
// complete two-shard set, an incomplete three-shard set, and two projectors.
func tree() []hf.TreeEntry {
	return []hf.TreeEntry{
		{Path: "README.md", Size: 8342},
		{Path: "Model-Q4_K_M.gguf", Size: 4_920_736_256, OID: "aa", LFS: true},
		{Path: "Model-Q8_0-00001-of-00002.gguf", Size: 4_294_967_296, OID: "b1", LFS: true},
		{Path: "Model-Q8_0-00002-of-00002.gguf", Size: 4_294_967_296, OID: "b2", LFS: true},
		{Path: "Model-F16-00001-of-00003.gguf", Size: 5_000_000_000, OID: "c1", LFS: true},
		{Path: "Model-F16-00003-of-00003.gguf", Size: 5_000_000_000, OID: "c3", LFS: true},
		{Path: "mmproj-Model-f16.gguf", Size: 1_048_576, OID: "d1", LFS: true},
		{Path: "mmproj-Model-f32.gguf", Size: 2_097_152, OID: "d2", LFS: true},
	}
}

func codeOf(t *testing.T, err error) model.ErrorCode {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want a model.Error", err)
	}
	return me.Code
}

func TestExpandRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		entries       []hf.TreeEntry // nil uses tree()
		files         []string
		includeMmproj bool
		mmprojFile    string
		wantCode      model.ErrorCode
		check         func(t *testing.T, p Plan)
	}{
		{
			name:  "a single-file quant expands to itself",
			files: []string{"Model-Q4_K_M.gguf"},
			check: func(t *testing.T, p Plan) {
				if len(p.Weights.Files) != 1 || p.Weights.QuantLabel != "Q4_K_M" {
					t.Errorf("weights = %+v", p.Weights)
				}
				if p.Mmproj != nil {
					t.Error("no projector was asked for")
				}
				if p.TotalBytes != 4_920_736_256 {
					t.Errorf("total = %d", p.TotalBytes)
				}
			},
		},
		{
			// The rule section 7.3 states: "the user picks a quant; the API
			// expands it to every shard". Naming shard 2 must download shard 1
			// too, or the model cannot be loaded.
			name:  "naming any shard expands to the whole set",
			files: []string{"Model-Q8_0-00002-of-00002.gguf"},
			check: func(t *testing.T, p Plan) {
				if len(p.Weights.Files) != 2 {
					t.Fatalf("got %d files, want the whole shard set", len(p.Weights.Files))
				}
				if PrimaryFile(p.Weights) != "Model-Q8_0-00001-of-00002.gguf" {
					t.Errorf("primary_file = %q, want shard 1", PrimaryFile(p.Weights))
				}
				if p.TotalBytes != 2*4_294_967_296 {
					t.Errorf("total = %d, want both shards", p.TotalBytes)
				}
			},
		},
		{
			// "The queue refuses a partial shard set." A repository mid-upload
			// advertises `-00003-of-00003` with shard 2 missing; queueing it
			// buys an hour of downloading and a model llama.cpp cannot load.
			name:     "an incomplete shard set is refused",
			files:    []string{"Model-F16-00001-of-00003.gguf"},
			wantCode: CodeShardSetIncomplete,
		},
		{
			name:     "a file the repository does not hold is refused",
			files:    []string{"Model-Q2_K.gguf"},
			wantCode: CodeFileNotInRepo,
		},
		{
			name:     "an empty selection is never read as everything",
			files:    nil,
			wantCode: CodeNoFilesSelected,
		},
		{
			// `downloads.model_id` is singular: two logical models sharing one
			// progress bar is not a thing section 2.7's schema can represent.
			name:     "two quantizations in one download are refused",
			files:    []string{"Model-Q4_K_M.gguf", "Model-Q8_0-00001-of-00002.gguf"},
			wantCode: CodeMultipleQuants,
		},
		{
			// Section 7.2's preference chain resolves the ordinary f16/f32 pair
			// without a picker: f16 is the second preference and it is unique.
			name:          "the f16 preference resolves the ordinary projector pair",
			files:         []string{"Model-Q4_K_M.gguf"},
			includeMmproj: true,
			check: func(t *testing.T, p Plan) {
				if p.Mmproj == nil || p.Mmproj.Files[0].Path != "mmproj-Model-f16.gguf" {
					t.Errorf("mmproj = %+v, want the f16 projector", p.Mmproj)
				}
			},
		},
		{
			// Section 7.2's picker rather than a guess, for the case the
			// preference chain genuinely cannot separate. Guessing wrong costs a
			// multi-gigabyte download of the wrong projector.
			name: "projectors with no recognizable precision produce a picker",
			entries: []hf.TreeEntry{
				{Path: "Model-Q4_K_M.gguf", Size: 10, OID: "aa", LFS: true},
				{Path: "mmproj-Model-vision.gguf", Size: 5, OID: "d1", LFS: true},
				{Path: "mmproj-Model-audio.gguf", Size: 5, OID: "d2", LFS: true},
			},
			files:         []string{"Model-Q4_K_M.gguf"},
			includeMmproj: true,
			wantCode:      CodeMmprojAmbiguous,
		},
		{
			name:          "an explicit projector choice short-circuits the preference",
			files:         []string{"Model-Q4_K_M.gguf"},
			includeMmproj: true,
			mmprojFile:    "mmproj-Model-f32.gguf",
			check: func(t *testing.T, p Plan) {
				if p.Mmproj == nil || p.Mmproj.Files[0].Path != "mmproj-Model-f32.gguf" {
					t.Errorf("mmproj = %+v", p.Mmproj)
				}
				if p.TotalBytes != 4_920_736_256+2_097_152 {
					t.Errorf("total = %d, want the weights plus the projector", p.TotalBytes)
				}
			},
		},
		{
			// A projector on its own is a legitimate download: section 7.3 says
			// it is separately reusable, and pairing it later is what
			// `POST /models/{id}/pair-mmproj` is for.
			name:  "a projector alone is a download of its own",
			files: []string{"mmproj-Model-f16.gguf"},
			check: func(t *testing.T, p Plan) {
				if !p.Weights.Mmproj {
					t.Errorf("weights = %+v, want the projector as the subject", p.Weights)
				}
				if p.Mmproj != nil {
					t.Error("a projector download must not pair a second projector with itself")
				}
			},
		},
		{
			name:     "a path escape is refused at the boundary",
			files:    []string{"../../etc/passwd"},
			wantCode: CodeFileNotInRepo,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := tc.entries
			if entries == nil {
				entries = tree()
			}
			p, err := ExpandRequest(entries, tc.files, tc.includeMmproj, tc.mmprojFile)
			if tc.wantCode != "" {
				if err == nil {
					t.Fatalf("expected %s, got a plan: %+v", tc.wantCode, p)
				}
				if got := codeOf(t, err); got != tc.wantCode {
					t.Fatalf("code = %s, want %s (%v)", got, tc.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandRequest: %v", err)
			}
			tc.check(t, p)
		})
	}
}

// TestPickMmprojPrefersAMatchingPrecision is section 7.2's auto-pairing rule
// applied to a remote tree: prefer a precision matching the weights, then f16,
// then f32.
func TestPickMmprojPrefersAMatchingPrecision(t *testing.T) {
	t.Parallel()

	groups := hf.GroupTree(tree())

	cases := []struct {
		weightsQuant string
		want         string
	}{
		{"F16", "mmproj-Model-f16.gguf"},
		{"F32", "mmproj-Model-f32.gguf"},
		// A quantization no projector matches falls through to the f16
		// preference, which is what the community publishes as the default.
		{"Q4_K_M", "mmproj-Model-f16.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.weightsQuant, func(t *testing.T) {
			t.Parallel()
			g, err := PickMmproj(groups, tc.weightsQuant, "")
			if err != nil {
				t.Fatalf("PickMmproj: %v", err)
			}
			if g == nil || g.Files[0].Path != tc.want {
				t.Errorf("picked %+v, want %s", g, tc.want)
			}
		})
	}

	// A repository with no projector is not an error: `include_mmproj` is on by
	// default and most repositories are text-only.
	only := hf.GroupTree([]hf.TreeEntry{{Path: "Model-Q4_K_M.gguf", Size: 10}})
	g, err := PickMmproj(only, "Q4_K_M", "")
	if err != nil || g != nil {
		t.Errorf("PickMmproj on a text-only repository = (%v, %v), want (nil, nil)", g, err)
	}

	// One projector needs no preference at all.
	single := hf.GroupTree([]hf.TreeEntry{
		{Path: "Model-Q4_K_M.gguf", Size: 10},
		{Path: "mmproj-Model-f16.gguf", Size: 5},
	})
	g, err = PickMmproj(single, "Q4_K_M", "")
	if err != nil || g == nil {
		t.Fatalf("PickMmproj with one candidate = (%v, %v)", g, err)
	}
}

func TestShardIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		filename  string
		total     int
		wantIndex int
		wantTotal int
	}{
		{"Model-Q8_0-00002-of-00005.gguf", 5, 2, 5},
		{"Model-Q4_K_M.gguf", 1, 1, 1},
		// An unsharded file in a group whose total was passed is still 1 of 1:
		// a set of one is what every downstream query treats it as.
		{"Model-Q4_K_M.gguf", 0, 1, 1},
		{"nested/Model-Q8_0-00001-of-00003.gguf", 3, 1, 3},
	}
	for _, tc := range cases {
		idx, total := ShardIndex(tc.filename, tc.total)
		if idx != tc.wantIndex || total != tc.wantTotal {
			t.Errorf("ShardIndex(%q, %d) = (%d, %d), want (%d, %d)",
				tc.filename, tc.total, idx, total, tc.wantIndex, tc.wantTotal)
		}
	}
}
