package instances

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Golden argv tests (DESIGN section 15).
//
// "internal/instances: argv rendering as byte-exact golden files — including one
// asserting that n_gpu_layers.mode=="auto" emits NO -ngl argument (D51) …
// RenderBenchArgv gets its own golden pair against the same FlagSet (D62)."
//
// The golden files are one argv element per line, so a diff names the argument
// that moved rather than the line that wrapped. Run with -update to rewrite
// them.

var update = flag.Bool("update", false, "rewrite the golden argv files")

const goldenDir = "testdata/argv"

func ptr[T any](v T) *T { return &v }

// cudaRuntime is a build that knows --fit, which is what makes `-ngl auto`
// render nothing at all.
func cudaRuntime() Runtime {
	return Runtime{
		ID:          "b10621-cuda-src",
		Dir:         "/var/lib/llamaman/versions/b10621-cuda-src",
		SupportsFit: true,
	}
}

func qwenModel() *ModelFile {
	return &ModelFile{
		ID: "m-qwen",
		Path: "/home/svc/.cache/huggingface/hub/models--bartowski--Qwen3-8B-GGUF/" +
			"snapshots/deadbeef/Qwen3-8B-Q4_K_M.gguf",
	}
}

func baseInstance() model.Instance {
	return model.Instance{
		ID:           "01JINSTANCE",
		Name:         "qwen",
		InternalPort: 21001,
		PublicPort:   8081,
		UnitName:     UnitName("qwen"),
	}
}

// docExampleFlags is the FlagSet behind the command line DESIGN section 5.7
// prints verbatim. The golden file for it is that command line, which is what
// makes this test a check on the document rather than only on the code.
func docExampleFlags() model.FlagSet {
	return model.FlagSet{
		CtxSize:         ptr(8192),
		NGpuLayers:      &model.NGpuLayers{Mode: model.NGLAll},
		BatchSize:       ptr(2048),
		UbatchSize:      ptr(512),
		Parallel:        ptr(4),
		FlashAttn:       ptr(model.FlashAttnOn),
		CacheTypeK:      ptr("q8_0"),
		CacheTypeV:      ptr("q8_0"),
		Alias:           ptr("qwen3-8b"),
		Jinja:           ptr(true),
		PropsEndpoint:   ptr(true),
		SlotsEndpoint:   ptr(true),
		MetricsEndpoint: ptr(true),
	}
}

// everythingFlags exercises every renderable field at once — the matrix case
// section 15 asks for, including cache types, a draft model, an mmproj and
// parallel slots.
func everythingFlags() model.FlagSet {
	return model.FlagSet{
		CtxSize:          ptr(32768),
		NGpuLayers:       &model.NGpuLayers{Mode: model.NGLCount, Count: ptr(37)},
		BatchSize:        ptr(4096),
		UbatchSize:       ptr(1024),
		Parallel:         ptr(8),
		Threads:          ptr(12),
		ThreadsBatch:     ptr(16),
		FlashAttn:        ptr(model.FlashAttnAuto),
		CacheTypeK:       ptr("q8_0"),
		CacheTypeV:       ptr("q4_0"),
		SplitMode:        ptr(model.SplitRow),
		TensorSplit:      []float64{0.6, 0.4},
		MainGPU:          ptr(1),
		DeviceFilter:     ptr("CUDA0,CUDA1"),
		DeviceUUIDs:      []string{"GPU-a1", "GPU-b2"},
		Mlock:            ptr(true),
		NoMmap:           ptr(true),
		ContBatching:     ptr(true),
		Embedding:        ptr(true),
		Pooling:          ptr("mean"),
		Rerank:           ptr(true),
		Alias:            ptr("qwen3-8b-big"),
		ChatTemplate:     ptr("chatml"),
		ChatTemplateFile: ptr("/etc/llamaman/tmpl.jinja"),
		Jinja:            ptr(true),
		RopeScaling:      ptr("yarn"),
		RopeFreqBase:     ptr(1000000.0),
		RopeFreqScale:    ptr(0.5),
		YarnExtFactor:    ptr(1.25),
		YarnAttnFactor:   ptr(0.75),
		NKeep:            ptr(256),
		NPredict:         ptr(512),
		DefragThold:      ptr(0.1),
		CacheReuse:       ptr(256),
		Numa:             ptr("distribute"),
		CPUMask:          ptr("0xff"),
		Prio:             ptr(2),
		SlotSavePath:     ptr("/var/lib/llamaman/slots"),
		Draft: &model.DraftFlags{
			NMax:       ptr(16),
			NMin:       ptr(0),
			PMin:       ptr(0.75),
			CtxSize:    ptr(4096),
			NGpuLayers: ptr(99),
		},
		PropsEndpoint:   ptr(true),
		SlotsEndpoint:   ptr(true),
		MetricsEndpoint: ptr(true),
		LogVerbosity:    ptr(1),
	}
}

func TestRenderArgvGolden(t *testing.T) {
	mmproj := &ModelFile{ID: "m-mmproj", Path: "/cache/snapshots/deadbeef/mmproj-f16.gguf"}
	draft := &ModelFile{ID: "m-draft", Path: "/cache/snapshots/c0ffee/Qwen3-0.6B-Q8_0.gguf"}

	autoFlags := model.FlagSet{
		CtxSize:    ptr(8192),
		NGpuLayers: &model.NGpuLayers{Mode: model.NGLAuto},
	}

	tests := []struct {
		name    string
		golden  string
		inst    model.Instance
		flags   model.FlagSet
		mmproj  *ModelFile
		draft   *ModelFile
		runtime Runtime
	}{
		{
			// The command line DESIGN section 5.7 prints, byte for byte.
			name: "the section 5.7 example", golden: "doc_example",
			inst: baseInstance(), flags: docExampleFlags(), runtime: cudaRuntime(),
		},
		{
			// A configuration that says nothing about the offload behaves as
			// `auto` for the same reason `auto` does: pinning -ngl is what turns
			// --fit off.
			name: "minimal", golden: "minimal",
			inst: baseInstance(), flags: model.FlagSet{}, runtime: cudaRuntime(),
		},
		{
			// D51: `auto` emits NO -ngl argument at all.
			name: "ngl auto on a build with --fit", golden: "ngl_auto",
			inst: baseInstance(), flags: autoFlags, runtime: cudaRuntime(),
		},
		{
			// Section 5.7's second rule: this build predates --fit, so `auto`
			// behaves as `all`. The renderer reads the COLUMN, never a file.
			name: "ngl auto on a build without --fit", golden: "ngl_auto_no_fit",
			inst: baseInstance(), flags: autoFlags,
			runtime: Runtime{ID: "b9000-cpu-bin", Dir: "/v/b9000-cpu-bin", SupportsFit: false},
		},
		{
			name: "ngl none", golden: "ngl_none",
			inst: baseInstance(),
			flags: model.FlagSet{
				CtxSize:    ptr(2048),
				NGpuLayers: &model.NGpuLayers{Mode: model.NGLNone},
				Parallel:   ptr(1),
			},
			runtime: cudaRuntime(),
		},
		{
			// -nocb is the one tri-state that has to be said out loud, because
			// llama-server defaults continuous batching on.
			name: "continuous batching off", golden: "cont_batching_off",
			inst:  baseInstance(),
			flags: model.FlagSet{ContBatching: ptr(false)}, runtime: cudaRuntime(),
		},
		{
			name: "every field, mmproj and draft", golden: "everything",
			inst: func() model.Instance {
				i := baseInstance()
				i.ExtraFlags = `--log-colors --lora "/models/adapter one.gguf"`
				return i
			}(),
			flags: everythingFlags(), mmproj: mmproj, draft: draft, runtime: cudaRuntime(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderArgv(tt.inst, tt.flags, qwenModel(), tt.mmproj, tt.draft, tt.runtime)
			if err != nil {
				t.Fatalf("RenderArgv: %v", err)
			}
			assertGolden(t, tt.golden, got)
		})
	}
}

// TestRenderBenchArgvGolden is D62's other half: the SAME FlagSet through the
// other renderer, so the two mappings cannot drift apart silently.
func TestRenderBenchArgvGolden(t *testing.T) {
	point := BenchPoint{
		PromptLen:   ptr(512),
		GenLen:      ptr(128),
		Depth:       ptr(4096),
		Repetitions: ptr(3),
	}

	tests := []struct {
		name   string
		golden string
		flags  model.FlagSet
		point  BenchPoint
	}{
		{
			name: "the section 5.7 example FlagSet", golden: "bench_doc_example",
			flags: docExampleFlags(), point: point,
		},
		{
			name: "every field", golden: "bench_everything",
			flags: everythingFlags(),
			point: BenchPoint{PromptLen: ptr(512), GenLen: ptr(128), Repetitions: ptr(5),
				ExtraFlags: "--progress"},
		},
		{
			// Section 10.1: llama-bench has no --fit, so `auto` becomes 999
			// rather than "omit the flag" — visibly, through BenchNotes.
			name: "ngl auto becomes 999", golden: "bench_ngl_auto",
			flags: model.FlagSet{NGpuLayers: &model.NGpuLayers{Mode: model.NGLAuto}},
			point: BenchPoint{PromptLen: ptr(128)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderBenchArgv(tt.flags, qwenModel(), cudaRuntime(), tt.point)
			if err != nil {
				t.Fatalf("RenderBenchArgv: %v", err)
			}
			assertGolden(t, tt.golden, got)
		})
	}
}

// TestBenchArgvOmitsServerOnlyFlags is section 10.1's "one further test": the
// bench renderer emits none of the server's own vocabulary, whatever the
// FlagSet says.
func TestBenchArgvOmitsServerOnlyFlags(t *testing.T) {
	argv, err := RenderBenchArgv(everythingFlags(), qwenModel(), cudaRuntime(),
		BenchPoint{PromptLen: ptr(512)})
	if err != nil {
		t.Fatalf("RenderBenchArgv: %v", err)
	}

	forbidden := []string{
		"-c", "--host", "--port", "--alias", "--props", "--slots", "--metrics",
		"--no-webui", "-np", "-cb", "-nocb", "--embedding", "--pooling", "--reranking",
		"--jinja", "--chat-template", "--chat-template-file", "--keep", "--predict",
		"--defrag-thold", "--cache-reuse", "--slot-save-path", "--verbosity",
		"-md", "--draft-max", "--draft-min", "--draft-p-min", "-cd", "-ngld",
		"--rope-scaling", "--rope-freq-base", "--rope-freq-scale",
		"--yarn-ext-factor", "--yarn-attn-factor",
	}
	for _, bad := range forbidden {
		for _, tok := range argv {
			if tok == bad {
				t.Errorf("RenderBenchArgv emitted %q, which llama-bench does not accept "+
					"(section 10.1)", bad)
			}
		}
	}
}

// TestBenchFlashAttnIsTwoValued pins section 10.1's `-fa` mapping, including the
// `auto` substitution and the note that makes it visible in the results table.
func TestBenchFlashAttnIsTwoValued(t *testing.T) {
	tests := []struct {
		in       model.FlashAttn
		want     string
		wantNote bool
	}{
		{model.FlashAttnOn, "1", false},
		{model.FlashAttnOff, "0", false},
		{model.FlashAttnAuto, "1", true},
	}

	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			flags := model.FlagSet{FlashAttn: ptr(tt.in)}
			argv, err := RenderBenchArgv(flags, qwenModel(), cudaRuntime(), BenchPoint{})
			if err != nil {
				t.Fatalf("RenderBenchArgv: %v", err)
			}
			if got := valueAfter(argv, "-fa"); got != tt.want {
				t.Errorf("-fa = %q, want %q", got, tt.want)
			}
			notes := BenchNotes(flags)
			if hasNote := len(notes) > 0; hasNote != tt.wantNote {
				t.Errorf("BenchNotes = %v, want a note: %v", notes, tt.wantNote)
			}
		})
	}
}

// TestNoRenderPathEmitsCUDAVisibleDevices is D66 as an executable rule: --device
// is the only device selector, and setting CUDA_VISIBLE_DEVICES beside it would
// renumber the very devices the user picked.
func TestNoRenderPathEmitsCUDAVisibleDevices(t *testing.T) {
	inst := baseInstance()
	inst.ExtraFlags = "--verbose"

	server, err := RenderArgv(inst, everythingFlags(), qwenModel(), nil, nil, cudaRuntime())
	if err != nil {
		t.Fatalf("RenderArgv: %v", err)
	}
	bench, err := RenderBenchArgv(everythingFlags(), qwenModel(), cudaRuntime(), BenchPoint{})
	if err != nil {
		t.Fatalf("RenderBenchArgv: %v", err)
	}

	for _, argv := range [][]string{server, bench} {
		for _, tok := range argv {
			if strings.Contains(tok, "CUDA_VISIBLE_DEVICES") {
				t.Errorf("a renderer emitted %q; D66 forbids it outright", tok)
			}
		}
	}
	if got := valueAfter(server, "--device"); got != "CUDA0,CUDA1" {
		t.Errorf("--device = %q, want the user's own selection verbatim", got)
	}
}

// TestDeviceUUIDsAreNeverRendered: the UUIDs are provenance for `device_filter`,
// compared by the supervisor against the live map (F22), and are not a flag.
func TestDeviceUUIDsAreNeverRendered(t *testing.T) {
	flags := model.FlagSet{
		DeviceFilter: ptr("CUDA1"),
		DeviceUUIDs:  []string{"GPU-a1b2c3", "GPU-d4e5f6"},
	}
	argv, err := RenderArgv(baseInstance(), flags, qwenModel(), nil, nil, cudaRuntime())
	if err != nil {
		t.Fatalf("RenderArgv: %v", err)
	}
	for _, tok := range argv {
		if strings.HasPrefix(tok, "GPU-") {
			t.Errorf("argv carries the GPU uuid %q; only the CUDA<n> labels are rendered", tok)
		}
	}
}

// TestRenderArgvNeedsAModel: a row with no resolved primary file cannot be
// rendered, and the code says which condition it is — the same one the launcher
// exits 72 for.
func TestRenderArgvNeedsAModel(t *testing.T) {
	_, err := RenderArgv(baseInstance(), model.FlagSet{}, nil, nil, nil, cudaRuntime())
	var me model.Error
	if !asModelError(err, &me) || me.Code != model.CodeModelMissing {
		t.Fatalf("RenderArgv with no model = %v, want %s", err, model.CodeModelMissing)
	}
}

// TestUnknownFlags is section 5.7's flag-churn guard, including the case the
// design calls out: a build with NO help capture makes the check unavailable
// rather than universally failing.
func TestUnknownFlags(t *testing.T) {
	argv := []string{"/v/bin/llama-server", "-m", "/m.gguf", "--jinja", "--brand-new-flag", "-c", "4096"}

	tests := []struct {
		name string
		help model.HelpFlags
		want []string
	}{
		{
			name: "a capture that knows every flag",
			help: model.HelpFlags{"-m", "--jinja", "-c", "--brand-new-flag"},
			want: nil,
		},
		{
			name: "a capture that does not know one",
			help: model.HelpFlags{"-m", "--jinja", "-c"},
			want: []string{"--brand-new-flag"},
		},
		{
			name: "no capture at all: the check is unavailable, not failing",
			help: nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, UnknownFlags(argv, tt.help)); diff != "" {
				t.Errorf("UnknownFlags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBenchIgnoredFlagsAreLoud: section 10.1 requires every dropped field to be
// reported, so "why is my benchmark not measuring my 32k context" is answered
// before the run.
func TestBenchIgnoredFlagsAreLoud(t *testing.T) {
	ignored := BenchIgnoredFlags(everythingFlags(), "--verbose")

	byField := map[string]string{}
	for _, ig := range ignored {
		byField[ig.Field] = ig.Reason
	}
	for _, want := range []string{
		"ctx_size", "alias", "parallel", "cont_batching", "jinja", "draft", "extra_flags",
	} {
		if _, ok := byField[want]; !ok {
			t.Errorf("BenchIgnoredFlags did not report %q as dropped", want)
		}
	}
	if got := byField["ctx_size"]; !strings.Contains(got, "-p/-n/-d") {
		t.Errorf("the ctx_size reason should name where context comes from instead, got %q", got)
	}
}

// valueAfter returns the argument following a flag, or "" when the flag is
// absent.
func valueAfter(argv []string, flag string) string {
	for i, tok := range argv {
		if tok == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func asModelError(err error, out *model.Error) bool {
	me, ok := err.(model.Error)
	if ok {
		*out = me
	}
	return ok
}

// assertGolden compares argv against testdata/argv/<name>.txt, one element per
// line.
func assertGolden(t *testing.T, name string, argv []string) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".txt")
	want := strings.Join(argv, "\n") + "\n"

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
		t.Fatalf("reading the golden file: %v (regenerate: go test ./internal/instances -update)", err)
	}
	if diff := cmp.Diff(strings.Split(strings.TrimSuffix(string(got), "\n"), "\n"), argv); diff != "" {
		t.Errorf("argv is not the golden file (-golden +got):\n%s", diff)
	}
}
