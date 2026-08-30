package fit_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/gguf/ggufbuild"
)

// DESIGN section 8.7's golden-test rule, and section 15 repeats it: the fit
// suite runs against the recorded loads in `testdata/fit/` and asserts
// predictions within ±10% AND, non-negotiably, that a verdict never says "fits"
// for a load that actually OOM'd.
//
// The two halves are not the same test and must not be collapsed into one. The
// ±10% band is an ACCURACY claim — it is what makes the number on the screen
// worth reading — and it is allowed to be wrong by a tenth. The OOM rule is a
// SAFETY claim and has no band at all: a calculator that says "fits" for a load
// that died allocating has told a user to do the one thing this whole subsystem
// exists to stop them doing, and being within 10% while doing it is no defense.
//
// The corpus lives in a data file rather than in Go so that a row recorded from
// a real host — a `fit_observations` row, which carries exactly these figures —
// can be appended without touching this file. `testdata/fit/gencorpus.py`
// documents where the current rows come from and re-derives them from the design
// text.

// CorpusTolerance is section 8.7's ±10%.
const corpusTolerance = 0.10

type corpus struct {
	Note  string         `json:"note"`
	Loads []recordedFile `json:"loads"`
}

// recordedFile is one row of the corpus: what was launched, on what card, and
// the three buffer figures llama.cpp printed for it.
type recordedFile struct {
	Name      string `json:"name"`
	Fixture   string `json:"fixture"`
	NCtx      int    `json:"n_ctx"`
	NUbatch   int    `json:"n_ubatch"`
	NBatch    int    `json:"n_batch"`
	NParallel int    `json:"n_parallel"`
	TypeK     string `json:"type_k"`
	TypeV     string `json:"type_v"`
	FlashAttn string `json:"flash_attn"`

	FreeVRAMBytes uint64 `json:"free_vram_bytes"`
	RAMFreeBytes  uint64 `json:"ram_free_bytes"`

	Observed struct {
		WeightsBytes uint64 `json:"weights_bytes"`
		KVBytes      uint64 `json:"kv_bytes"`
		KVSWABytes   uint64 `json:"kv_swa_bytes"`
		ComputeBytes uint64 `json:"compute_bytes"`
	} `json:"observed"`

	// OOM marks a load that died allocating. It is `fit_observations.oom`.
	OOM bool `json:"oom"`
}

var corpusFixtures = map[string]func() *ggufbuild.Builder{
	"llama":  llamaFixture,
	"qwen3":  qwen3Fixture,
	"gemma3": gemma3Fixture,
	"moe":    moeFixture,
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	// internal/fit -> internal -> repo root -> testdata/fit.
	path := filepath.Join("..", "..", "testdata", "fit", "corpus.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the fit corpus: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse the fit corpus: %v", err)
	}
	return c
}

func (r recordedFile) request(t *testing.T) fit.Request {
	t.Helper()
	build, ok := corpusFixtures[r.Fixture]
	if !ok {
		t.Fatalf("the corpus names fixture %q, which this suite does not build", r.Fixture)
	}
	fa := fit.FlashAttnOff
	if r.FlashAttn == "on" {
		fa = fit.FlashAttnOn
	}
	return fit.Request{
		Model: shapeOf(parse(t, build())),
		Flags: fit.Flags{
			NCtx: r.NCtx, NUbatch: r.NUbatch, NBatch: r.NBatch, NParallel: r.NParallel,
			NGL: fit.NGLAll, FlashAttn: fa, TypeK: r.TypeK, TypeV: r.TypeV,
			SplitMode: fit.SplitLayer,
		},
		Devices: devices(r.FreeVRAMBytes),
		Host: fit.Host{
			RAMFreeBytes: r.RAMFreeBytes, RAMTotalBytes: 2 * r.RAMFreeBytes, RAMKnown: true,
		},
	}
}

// TestCorpusIsTheSizeTheRuleAsks guards the corpus itself. "~20 recorded real
// loads" is part of the contract, and a suite that silently ran over three rows
// would pass every assertion below while testing almost nothing.
func TestCorpusIsTheSizeTheRuleAsks(t *testing.T) {
	c := loadCorpus(t)
	if len(c.Loads) < 20 {
		t.Errorf("the corpus has %d loads; section 8.7 asks for about twenty", len(c.Loads))
	}
	var ooms int
	for _, l := range c.Loads {
		if l.OOM {
			ooms++
		}
	}
	if ooms == 0 {
		t.Error("the corpus contains no OOM row, so the one non-negotiable rule " +
			"in section 8.7 is asserted over nothing")
	}
}

// TestPredictionsAreWithinTenPercent is the accuracy half of the rule.
//
// Each term is checked separately rather than only in total, because a total can
// be right for the wrong reasons: an overestimated cache and an underestimated
// compute buffer cancel, and the configuration that separates them is the one
// the user is about to change.
func TestPredictionsAreWithinTenPercent(t *testing.T) {
	for _, load := range loadCorpus(t).Loads {
		t.Run(load.Name, func(t *testing.T) {
			rep := fit.Estimate(load.request(t))

			within(t, "weights", rep.WeightsBytes, load.Observed.WeightsBytes)
			within(t, "kv", rep.KVBytes, load.Observed.KVBytes)
			within(t, "kv_swa", rep.KVSWABytes, load.Observed.KVSWABytes)
			within(t, "compute", rep.ComputeBytes, load.Observed.ComputeBytes)
		})
	}
}

// within asserts one term against its recorded value. A recorded zero — a term
// this load has none of, such as the sliding-window cache on a model without one
// — must be predicted as exactly zero: there is no percentage of nothing, and
// "within 10% of 0" would silently accept any number at all.
func within(t *testing.T, term string, got, want uint64) {
	t.Helper()
	if want == 0 {
		if got != 0 {
			t.Errorf("%s = %d, want 0", term, got)
		}
		return
	}
	off := math.Abs(float64(got)-float64(want)) / float64(want)
	if off > corpusTolerance {
		t.Errorf("%s = %d, recorded %d — off by %.1f%%, above the ±%.0f%% section 8.7 allows",
			term, got, want, off*100, corpusTolerance*100)
	}
}

// TestNeverSaysFitsForARecordedOOM_Corpus is the safety half, run over the same
// rows.
//
// An OOM row's card is short of the REAL allocation — weights, cache, compute
// buffers and the backend context, with no margin — because that is what an OOM
// is. A row that merely ate into `fit.margin_mib` would make this rule trivial:
// the margin is our headroom, not llama.cpp's.
func TestNeverSaysFitsForARecordedOOM_Corpus(t *testing.T) {
	for _, load := range loadCorpus(t).Loads {
		if !load.OOM {
			continue
		}
		t.Run(load.Name, func(t *testing.T) {
			rep := fit.Estimate(load.request(t))
			if rep.Verdict == fit.VerdictFits {
				t.Fatalf("this load OOM'd and the calculator said `fits`: it assigned "+
					"%d bytes to a card with %d free", rep.RequiredVRAMBytes, load.FreeVRAMBytes)
			}
		})
	}
}

// TestSaysSomethingUsefulForARecordedSuccess is the other direction, and it is
// what stops the rule above from being satisfiable by answering `wont_run` to
// everything. A load that ran must not be reported as impossible.
func TestSaysSomethingUsefulForARecordedSuccess(t *testing.T) {
	for _, load := range loadCorpus(t).Loads {
		if load.OOM {
			continue
		}
		t.Run(load.Name, func(t *testing.T) {
			rep := fit.Estimate(load.request(t))
			if rep.Verdict != fit.VerdictFits {
				t.Errorf("this load ran on a card with %d free and the calculator said %q: %v",
					load.FreeVRAMBytes, rep.Verdict, rep.Notes)
			}
		})
	}
}
