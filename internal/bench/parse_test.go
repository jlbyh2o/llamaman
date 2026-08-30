package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// llama-bench JSON parsing against recorded outputs, which is the other half of
// DESIGN section 15's unit test for this package.
//
// The fixtures are hand-written from the field list section 10 names —
// `n_prompt`, `n_gen`, `n_depth`, `avg_ts`, `stddev_ts`, `avg_ns`, `stddev_ns`,
// `samples_ns` — surrounded by the two dozen fields llama-bench actually emits
// and this schema does not model. Those extra fields are the point of the
// `raw_json` column, and one of the assertions below is that they survive.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseOutput(t *testing.T) {
	t.Parallel()

	type want struct {
		kind     model.BenchTestKind
		nPrompt  int
		nGen     int
		nDepth   int
		avgTS    float64
		stddevTS float64
		avgNS    int64
		stddevNS int64
		samples  int
	}

	tests := []struct {
		name    string
		fixture string
		want    []want
	}{
		{
			name:    "one pp and one tg, with stddev and three samples",
			fixture: "llama-bench-pp-tg.json",
			want: []want{
				{
					kind: model.TestPP, nPrompt: 512, nGen: 0, nDepth: 0,
					avgTS: 6058.31, stddevTS: 86.42,
					avgNS: 84512340, stddevNS: 1204518, samples: 3,
				},
				{
					kind: model.TestTG, nPrompt: 0, nGen: 128, nDepth: 0,
					avgTS: 116.74, stddevTS: 0.98,
					avgNS: 1096442100, stddevNS: 9218440, samples: 3,
				},
			},
		},
		{
			name:    "a depth test is pp+tg",
			fixture: "llama-bench-depth.json",
			want: []want{{
				kind: model.TestPPTG, nPrompt: 512, nGen: 128, nDepth: 4096,
				avgTS: 132.78, stddevTS: 1.71,
				avgNS: 4820117600, stddevNS: 61204800, samples: 3,
			}},
		},
		{
			name: "a single repetition reports a zero stddev, which is a " +
				"measurement rather than a missing value",
			fixture: "llama-bench-noisy.txt",
			want: []want{{
				kind: model.TestPP, nPrompt: 128, nGen: 0, nDepth: 0,
				avgTS: 318.31, stddevTS: 0,
				avgNS: 402118000, stddevNS: 0, samples: 1,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, err := ParseOutput(readFixture(t, tt.fixture))
			if err != nil {
				t.Fatalf("ParseOutput: %v", err)
			}
			if len(records) != len(tt.want) {
				t.Fatalf("got %d records, want %d", len(records), len(tt.want))
			}
			for i, w := range tt.want {
				rec := records[i]
				got := want{
					kind: rec.Kind(), nPrompt: rec.NPrompt, nGen: rec.NGen, nDepth: rec.NDepth,
					avgTS: rec.AvgTS, stddevTS: rec.StddevTS,
					avgNS: roundNS(rec.AvgNS), stddevNS: roundNS(rec.StddevNS),
					samples: len(rec.SamplesNS),
				}
				if diff := cmp.Diff(w, got, cmp.AllowUnexported(want{})); diff != "" {
					t.Errorf("record %d (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}

// TestRawJSONIsPreserved is section 10's "so a future llama-bench schema change
// never loses data": every field the object carried is still there, including
// the ones no column models.
func TestRawJSONIsPreserved(t *testing.T) {
	t.Parallel()

	records, err := ParseOutput(readFixture(t, "llama-bench-pp-tg.json"))
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(records[0].RawJSON, &raw); err != nil {
		t.Fatalf("raw_json does not parse: %v", err)
	}
	for _, field := range []string{
		"build_commit", "build_number", "cpu_info", "gpu_info", "backends",
		"model_filename", "model_type", "model_size", "model_n_params",
		"cpu_mask", "cpu_strict", "poll", "no_kv_offload", "tensor_buft_overrides",
		"use_mmap", "embeddings", "no_op_offload", "test_time", "samples_ts",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("raw_json lost %q, which no column models and which is exactly "+
				"what preserving the object is for", field)
		}
	}
}

// TestSamplesJSON: the per-repetition timings become the `samples_json` column,
// which is what makes a stddev checkable rather than merely reported.
func TestSamplesJSON(t *testing.T) {
	t.Parallel()

	records, err := ParseOutput(readFixture(t, "llama-bench-pp-tg.json"))
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	samples, err := records[0].SamplesJSON()
	if err != nil {
		t.Fatalf("SamplesJSON: %v", err)
	}
	if samples == nil {
		t.Fatal("SamplesJSON returned nil for a record with three samples")
	}
	var got []float64
	if err := json.Unmarshal([]byte(*samples), &got); err != nil {
		t.Fatalf("samples_json does not parse: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("samples_json has %d entries, want 3", len(got))
	}

	// A record with no samples writes NULL rather than `[]`: "not measured" and
	// "measured as empty" are different answers, and the column is nullable for
	// exactly that reason.
	empty := Record{}
	nilSamples, err := empty.SamplesJSON()
	if err != nil {
		t.Fatalf("SamplesJSON: %v", err)
	}
	if nilSamples != nil {
		t.Errorf("SamplesJSON = %q for a record with no samples, want NULL", *nilSamples)
	}
}

// TestParseOutputTolerance covers the surrounding noise. llama-bench writes its
// load lines to stderr and its JSON to stdout, but a build that logs one stray
// line to stdout must not cost a point its results.
func TestParseOutputTolerance(t *testing.T) {
	t.Parallel()

	t.Run("lines before and after the array", func(t *testing.T) {
		records, err := ParseOutput(readFixture(t, "llama-bench-noisy.txt"))
		if err != nil {
			t.Fatalf("ParseOutput: %v", err)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records, want 1", len(records))
		}
	})

	// The bracket inside `model_filename` is the reason extractJSONArray tracks
	// string literals instead of looking for the last `]`: a model path with a
	// bracket in it is unusual and entirely legal, and the failure it would
	// cause — a truncated array that still parses — is silent.
	t.Run("a bracket inside a string does not end the array", func(t *testing.T) {
		records, err := ParseOutput(readFixture(t, "llama-bench-noisy.txt"))
		if err != nil {
			t.Fatalf("ParseOutput: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(records[0].RawJSON, &raw); err != nil {
			t.Fatalf("raw_json does not parse: %v", err)
		}
		if name, _ := raw["model_filename"].(string); !strings.Contains(name, "[v2]") {
			t.Errorf("model_filename = %q; the bracketed path was truncated", name)
		}
	})
}

func TestParseOutputFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no array at all",
			in:   "error while handling argument \"-c\": unrecognized argument\n",
			want: "printed no JSON array",
		},
		{
			name: "an array the process was killed part way through",
			in:   `[{"n_prompt":512,"avg_ts":10.0`,
			want: "unterminated",
		},
		{
			name: "an empty array",
			in:   "[]\n",
			want: "empty result array",
		},
		{
			name: "an element that is not an object",
			in:   `[7]`,
			want: "could not be read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOutput([]byte(tt.in))
			if err == nil {
				t.Fatal("ParseOutput accepted unusable output")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// TestRoundNS pins the one conversion at the JSON boundary: llama-bench emits
// nanoseconds as JSON numbers (doubles) and the column is an INTEGER.
func TestRoundNS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   float64
		want int64
	}{
		{0, 0},
		{1204518, 1204518},
		{1204518.6, 1204519},
		{-1, 0},
	}
	for _, tt := range tests {
		if got := roundNS(tt.in); got != tt.want {
			t.Errorf("roundNS(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
