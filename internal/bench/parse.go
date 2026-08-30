package bench

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Parsing llama-bench's `-o json` output (DESIGN section 10, "Parsing").
//
// "Each object in llama-bench's JSON array becomes a `bench_results` row —
// `n_prompt`, `n_gen`, `n_depth`, `avg_ts`, `stddev_ts`, `avg_ns`, `stddev_ns`,
// `samples_ns` — with `raw_json` preserved verbatim, so a future llama-bench
// schema change never loses data."
//
// The verbatim half is not a nicety. llama-bench's JSON carries two dozen fields
// this schema does not model — `build_commit`, `cpu_info`, `backends`,
// `model_n_params`, `tensor_buft_overrides`, `samples_ts` — and upstream adds to
// them freely. Keeping the object means a column added later can be backfilled
// from history instead of only from runs made after the change.

// Record is the subset of one llama-bench result object this schema models. Every
// other field of the object survives in RawJSON.
//
// The nanosecond fields are float64 here and INTEGER in the column: llama-bench
// emits them as JSON numbers, and a JSON number is a double whatever the value
// looks like. Rounding at this boundary — once, on the way in — is what keeps the
// rest of the system from having to ask.
type Record struct {
	NPrompt int `json:"n_prompt"`
	NGen    int `json:"n_gen"`
	NDepth  int `json:"n_depth"`

	AvgNS    float64 `json:"avg_ns"`
	StddevNS float64 `json:"stddev_ns"`
	AvgTS    float64 `json:"avg_ts"`
	StddevTS float64 `json:"stddev_ts"`

	// SamplesNS is the per-repetition timing. It is what makes a stddev
	// checkable rather than merely reported, and it is why `-r` is on the run.
	SamplesNS []float64 `json:"samples_ns"`

	// RawJSON is the object exactly as llama-bench wrote it.
	RawJSON []byte `json:"-"`
}

// Kind is the `bench_results.test_kind` this record describes, derived from the
// two lengths the way llama-bench's own table labels its rows: a prompt with no
// generation is `pp`, a generation with no prompt is `tg`, and both together —
// the shape a `depth` test produces — is `pp+tg`.
func (r Record) Kind() model.BenchTestKind {
	switch {
	case r.NPrompt > 0 && r.NGen > 0:
		return model.TestPPTG
	case r.NGen > 0:
		return model.TestTG
	default:
		return model.TestPP
	}
}

// SamplesJSON renders the per-repetition timings for the `samples_json` column,
// or nil when llama-bench reported none — which is what a single-repetition run
// looks like on some builds, and is a real "not measured" rather than an empty
// list.
func (r Record) SamplesJSON() (*string, error) {
	if len(r.SamplesNS) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(r.SamplesNS)
	if err != nil {
		return nil, fmt.Errorf("bench: render the samples: %w", err)
	}
	s := string(b)
	return &s, nil
}

// roundNS converts a JSON number to the INTEGER the column holds. A negative or
// non-finite value — which no llama-bench build produces, but which a truncated
// pipe could — becomes zero rather than an out-of-range integer, because the
// column is NOT NULL and a garbage timing must not take a whole point down.
func roundNS(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return int64(math.Round(v))
}

// ParseOutput reads llama-bench's stdout and returns one Record per object.
//
// It tolerates surrounding noise. llama-bench writes its load and progress lines
// to stderr and its JSON to stdout, but a build that logs one stray line to
// stdout — or a wrapper that prepends one — must not cost a point its results,
// so the array is located rather than assumed to be the whole stream.
func ParseOutput(stdout []byte) ([]Record, error) {
	arr, err := extractJSONArray(stdout)
	if err != nil {
		return nil, err
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(arr, &raw); err != nil {
		return nil, fmt.Errorf("bench: llama-bench's JSON output could not be read: %w", err)
	}

	out := make([]Record, 0, len(raw))
	for i, el := range raw {
		var rec Record
		if err := json.Unmarshal(el, &rec); err != nil {
			return nil, fmt.Errorf("bench: result %d of llama-bench's output could not be read: %w",
				i, err)
		}
		// A copy, not the slice header: `raw` aliases the buffer the caller may
		// reuse, and `raw_json` is a column that outlives this call by years.
		rec.RawJSON = append([]byte(nil), el...)
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bench: llama-bench produced an empty result array")
	}
	return out, nil
}

// extractJSONArray finds the results array in a stream that may carry other
// lines around it.
//
// It considers EVERY `[` in turn, not just the first, and that is not caution
// for its own sake: llama.cpp's own startup lines contain brackets —
// `ggml_cuda_init: found 1 CUDA device [model=NVIDIA GeForce RTX 4090]` is a
// real one — so "the first bracket" is reliably the wrong bracket on a CUDA
// host. Each candidate is balanced with JSON string literals respected, so a
// `model_filename` containing a bracket cannot end the array early either, and
// then probed as an actual JSON array. The first candidate that IS one wins.
func extractJSONArray(b []byte) ([]byte, error) {
	var (
		sawBracket      bool
		sawUnterminated bool
	)
	for i := 0; i < len(b); i++ {
		if b[i] != '[' {
			continue
		}
		sawBracket = true
		slice, ok := balancedArray(b[i:])
		if !ok {
			sawUnterminated = true
			continue
		}
		var probe []json.RawMessage
		if err := json.Unmarshal(slice, &probe); err != nil {
			continue
		}
		return slice, nil
	}

	switch {
	case !sawBracket:
		return nil, fmt.Errorf("bench: llama-bench printed no JSON array " +
			"(was it run with -o json?)")
	case sawUnterminated:
		return nil, fmt.Errorf("bench: llama-bench's JSON array is unterminated " +
			"(the process was probably killed part way through printing it)")
	default:
		return nil, fmt.Errorf("bench: llama-bench's output contains no readable JSON array")
	}
}

// balancedArray returns the `[…]` that starts at b[0], with JSON string literals
// and their escapes respected. It reports false when the brackets never balance.
func balancedArray(b []byte) ([]byte, bool) {
	var (
		depth    int
		inString bool
		escaped  bool
	)
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// An ordinary character inside a string literal.
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return b[:i+1], true
			}
		}
	}
	return nil, false
}
