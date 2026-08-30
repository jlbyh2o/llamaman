package bench

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// The section 10.1 mapping table, exercised end to end: a Sweep expands into
// Points, each Point renders through instances.RenderBenchArgv, and the whole
// sweep's command lines are asserted byte-exactly against a golden file.
//
// The goldens beside internal/instances pin the mapping for ONE hand-built
// FlagSet. These pin it for the FlagSets EXPANSION produces, which is the half
// that could drift on its own: a sweep axis that wrote the wrong field, or an
// `auto` offload that reached argv without its 999 substitution, would leave
// those goldens untouched and every real benchmark wrong.
//
//	go test ./internal/bench -update

var update = flag.Bool("update", false, "rewrite the golden argv files")

const goldenDir = "testdata/argv"

// benchRuntime is a llamacpp_versions row as the renderer reads it, with the
// directory already resolved — the renderers never stat anything, so these are
// just strings.
func benchRuntime() instances.Runtime {
	return instances.Runtime{
		ID:          "b10621-cuda-src",
		Dir:         "/var/lib/llamaman/versions/b10621-cuda-src",
		SupportsFit: true,
	}
}

func benchModel() *instances.ModelFile {
	return &instances.ModelFile{
		ID: "01JQWEN",
		Path: "/home/svc/.cache/huggingface/hub/models--bartowski--Qwen3-8B-GGUF/" +
			"snapshots/deadbeef/Qwen3-8B-Q4_K_M.gguf",
	}
}

func TestSweepArgvGolden(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		sweep  Sweep
		reps   int
	}{
		{
			// Section 3.13's own POST body. Seventy-two command lines is a lot
			// of golden file, so this case takes the first test shape only —
			// which is still every offload, batch size, flash-attention setting
			// and K type the example names.
			name:   "section 3.13's example sweep, pp512 only",
			golden: "doc_example_sweep",
			sweep: func() Sweep {
				s := docExampleSweep()
				s.Tests = []Test{{PP: ptr(512)}}
				return s
			}(),
			reps: 3,
		},
		{
			// Every row of section 10.1 that comes from the FlagSet rather than
			// from the sweep, held fixed in the base while two axes vary. It is
			// the case that would catch a renamed flag or a dropped field.
			name:   "the whole 10.1 mapping table through a base FlagSet",
			golden: "mapping_table",
			sweep: Sweep{
				Base: &model.FlagSet{
					// Dropped by section 10.1, every one of them, and their
					// presence here is the point: the golden must show none.
					CtxSize:         ptr(32768),
					Alias:           ptr("qwen"),
					Parallel:        ptr(4),
					ContBatching:    ptr(true),
					Embedding:       ptr(false),
					Jinja:           ptr(true),
					NKeep:           ptr(64),
					NPredict:        ptr(256),
					DefragThold:     ptr(0.1),
					CacheReuse:      ptr(256),
					SlotSavePath:    ptr("/var/lib/llamaman/slots"),
					PropsEndpoint:   ptr(true),
					SlotsEndpoint:   ptr(true),
					MetricsEndpoint: ptr(true),
					LogVerbosity:    ptr(1),
					RopeScaling:     ptr("yarn"),
					RopeFreqBase:    ptr(1000000.0),
					Draft:           &model.DraftFlags{NMax: ptr(16)},

					// Mapped by section 10.1, every one of them.
					Threads:      ptr(12),
					ThreadsBatch: ptr(16),
					DeviceFilter: ptr("CUDA0,CUDA1"),
					MainGPU:      ptr(1),
					Mlock:        ptr(true),
					NoMmap:       ptr(true),
					Numa:         ptr("distribute"),
					CPUMask:      ptr("0xff"),
					Prio:         ptr(2),
					CacheTypeV:   ptr("q4_0"),
				},
				NGpuLayers: NGLAxis{
					{Mode: model.NGLNone},
					{Mode: model.NGLCount, Count: ptr(20)},
					{Mode: model.NGLAll},
					{Mode: model.NGLAuto},
				},
				NUbatch:     IntAxis{512},
				SplitMode:   StrAxis{"row"},
				TensorSplit: StrAxis{"0.6,0.4"},
				TypeK:       StrAxis{"q8_0"},
				Tests:       []Test{{PP: ptr(512), TG: ptr(128), Depth: ptr(4096)}},
				ExtraFlags:  "--progress",
			},
			reps: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			points, err := Expand(tt.sweep)
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}

			var lines []string
			for _, p := range points {
				argv, err := instances.RenderBenchArgv(p.Flags, benchModel(), benchRuntime(),
					p.BenchPoint(tt.reps, tt.sweep.ExtraFlags))
				if err != nil {
					t.Fatalf("RenderBenchArgv(point %d): %v", p.Ordinal, err)
				}
				lines = append(lines, p.Label+"\t"+strings.Join(argv, " "))
			}
			assertGoldenLines(t, tt.golden, lines)
		})
	}
}

// TestSweepArgvOmitsServerOnlyFlags is section 10.1's "one further test", asked
// of the EXPANDED points rather than of one hand-built FlagSet: no point of any
// sweep may carry a flag llama-bench does not accept, however the base was
// written.
func TestSweepArgvOmitsServerOnlyFlags(t *testing.T) {
	t.Parallel()

	base := model.FlagSet{
		CtxSize: ptr(32768), Alias: ptr("qwen"), Parallel: ptr(4),
		ContBatching: ptr(true), Embedding: ptr(true), Pooling: ptr("mean"),
		Rerank: ptr(true), Jinja: ptr(true), ChatTemplate: ptr("chatml"),
		NKeep: ptr(64), NPredict: ptr(256), DefragThold: ptr(0.1),
		CacheReuse: ptr(256), SlotSavePath: ptr("/tmp/slots"),
		PropsEndpoint: ptr(true), SlotsEndpoint: ptr(true), MetricsEndpoint: ptr(true),
		LogVerbosity: ptr(1), RopeScaling: ptr("yarn"), RopeFreqBase: ptr(1e6),
		YarnExtFactor: ptr(1.0), Draft: &model.DraftFlags{NMax: ptr(16), CtxSize: ptr(2048)},
	}
	points, err := Expand(Sweep{
		Base:      &base,
		NBatch:    IntAxis{512, 2048},
		FlashAttn: BoolAxis{true, false},
		Tests:     []Test{{PP: ptr(512)}, {TG: ptr(128)}},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	forbidden := []string{
		"-c", "--host", "--port", "--alias", "--props", "--slots", "--metrics",
		"--no-webui", "-np", "-cb", "-nocb", "--embedding", "--pooling", "--reranking",
		"--jinja", "--chat-template", "--keep", "--predict", "--defrag-thold",
		"--cache-reuse", "--slot-save-path", "--verbosity", "-md", "--draft-max",
		"-cd", "-ngld", "--rope-scaling", "--rope-freq-base", "--yarn-ext-factor",
	}
	for _, p := range points {
		argv, err := instances.RenderBenchArgv(p.Flags, benchModel(), benchRuntime(),
			p.BenchPoint(3, ""))
		if err != nil {
			t.Fatalf("RenderBenchArgv(point %d): %v", p.Ordinal, err)
		}
		for _, tok := range argv {
			for _, bad := range forbidden {
				if tok == bad {
					t.Errorf("point %d (%s) emitted %q, which llama-bench does not accept "+
						"(section 10.1)", p.Ordinal, p.Label, bad)
				}
			}
			if strings.Contains(tok, "CUDA_VISIBLE_DEVICES") {
				t.Errorf("point %d emitted %q; D66 forbids it outright", p.Ordinal, tok)
			}
		}
	}
}

// TestBenchExtraFlagsForbidden: `bench.extra_flags` is llama-bench's own escape
// hatch and is validated against ITS forbidden list — the model, the output
// format and the repetition count all come from the sweep, and a run whose `-o`
// is not `json` cannot be parsed at all.
func TestBenchExtraFlagsForbidden(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"-m /other/model.gguf", "-o md", "-r 99"} {
		t.Run(bad, func(t *testing.T) {
			points, err := Expand(Sweep{ExtraFlags: bad})
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			_, err = instances.RenderBenchArgv(points[0].Flags, benchModel(), benchRuntime(),
				points[0].BenchPoint(3, bad))
			if err == nil {
				t.Fatalf("RenderBenchArgv accepted bench.extra_flags %q", bad)
			}
			var me model.Error
			if !errorAs(err, &me) || me.Code != model.CodeExtraFlagForbidden {
				t.Fatalf("got %v, want %s", err, model.CodeExtraFlagForbidden)
			}
		})
	}
}

// errorAs is errors.As without the import, kept local so this file's imports
// stay the ones a reader of a golden test expects.
func errorAs(err error, target *model.Error) bool {
	if me, ok := err.(model.Error); ok {
		*target = me
		return true
	}
	return false
}

func assertGoldenLines(t *testing.T, name string, lines []string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".txt")
	want := strings.Join(lines, "\n") + "\n"

	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden file: %v (regenerate: go test ./internal/bench -update)", err)
	}
	gotLines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if diff := cmp.Diff(gotLines, lines); diff != "" {
		t.Errorf("argv is not the golden file (-golden +got):\n%s", diff)
	}
}
