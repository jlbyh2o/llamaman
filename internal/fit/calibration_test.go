package fit

import (
	"math"
	"testing"
)

// D32's three guards, as a table: the window, the [0.5, 2.0] clamp, and the
// three-sample floor below which the defaults stand and the report says
// `modeled`.
func TestNewCalibration(t *testing.T) {
	// obs builds n observations whose actual/predicted ratio is r.
	obs := func(n int, ratios ...float64) []Observation {
		out := make([]Observation, 0, n)
		for i := range n {
			r := ratios[i%len(ratios)]
			out = append(out, Observation{
				PredictedBytes: 1000,
				ActualBytes:    uint64(1000 * r),
			})
		}
		return out
	}

	cases := []struct {
		name        string
		in          []Observation
		wantApplied bool
		wantRatio   float64
		wantClamped bool
		wantSamples int
	}{
		{
			name:        "two samples is below the floor, so nothing is corrected",
			in:          obs(2, 1.4),
			wantApplied: false, wantSamples: 2,
		},
		{
			name:        "three samples is the floor",
			in:          obs(3, 1.4),
			wantApplied: true, wantRatio: 1.4, wantSamples: 3,
		},
		{
			name: "the median ignores one outlier rather than averaging it in",
			in: []Observation{
				{PredictedBytes: 1000, ActualBytes: 1100},
				{PredictedBytes: 1000, ActualBytes: 1200},
				{PredictedBytes: 1000, ActualBytes: 1150},
				{PredictedBytes: 1000, ActualBytes: 9000}, // a parse bug
				{PredictedBytes: 1000, ActualBytes: 1180},
			},
			wantApplied: true, wantRatio: 1.18, wantSamples: 5,
		},
		{
			name:        "a ratio above 2.0 is clamped, not believed",
			in:          obs(4, 3.5),
			wantApplied: true, wantRatio: MaxRatio, wantClamped: true, wantSamples: 4,
		},
		{
			name:        "a ratio below 0.5 is clamped too",
			in:          obs(4, 0.2),
			wantApplied: true, wantRatio: MinRatio, wantClamped: true, wantSamples: 4,
		},
		{
			name: "an OOM row is excluded: it measures the size of a failure",
			in: []Observation{
				{PredictedBytes: 1000, ActualBytes: 1100},
				{PredictedBytes: 1000, ActualBytes: 1200},
				{PredictedBytes: 1000, ActualBytes: 8000, OOM: true},
			},
			wantApplied: false, wantSamples: 2,
		},
		{
			name: "rows with no parsed actual are excluded",
			in: []Observation{
				{PredictedBytes: 1000, ActualBytes: 1100},
				{PredictedBytes: 1000, ActualBytes: 0},
				{PredictedBytes: 0, ActualBytes: 1200},
			},
			wantApplied: false, wantSamples: 1,
		},
		{
			name:        "no observations at all is the identity",
			in:          nil,
			wantApplied: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCalibration(tc.in)
			if c.Applied != tc.wantApplied {
				t.Fatalf("applied = %v, want %v (%+v)", c.Applied, tc.wantApplied, c)
			}
			if c.Samples != tc.wantSamples {
				t.Errorf("samples = %d, want %d", c.Samples, tc.wantSamples)
			}
			if tc.wantApplied && math.Abs(c.Ratio-tc.wantRatio) > 1e-9 {
				t.Errorf("ratio = %v, want %v", c.Ratio, tc.wantRatio)
			}
			if c.Clamped != tc.wantClamped {
				t.Errorf("clamped = %v, want %v", c.Clamped, tc.wantClamped)
			}
		})
	}
}

// TestCalibrationWindowIsBounded: only the last CalibrationWindow observations
// count, so a correction that stopped being true stops being applied.
func TestCalibrationWindowIsBounded(t *testing.T) {
	var rows []Observation
	// Newest first, as `idx_fit_obs` yields them: twenty recent 1.2s, then a
	// hundred ancient 0.5s that must not reach the median.
	for range CalibrationWindow {
		rows = append(rows, Observation{PredictedBytes: 1000, ActualBytes: 1200})
	}
	for range 100 {
		rows = append(rows, Observation{PredictedBytes: 1000, ActualBytes: 500})
	}
	c := NewCalibration(rows)
	if !c.Applied {
		t.Fatal("twenty usable samples must produce a correction")
	}
	if math.Abs(c.Ratio-1.2) > 1e-9 {
		t.Errorf("ratio = %v, want 1.2 — the window let older rows in", c.Ratio)
	}
	if c.Samples != CalibrationWindow {
		t.Errorf("samples = %d, want %d", c.Samples, CalibrationWindow)
	}
}

// TestCalibrationAppliesToBothTerms: section 8.7 corrects `k_act` AND `OH_gpu`
// by the same ratio, because the observation this host records — llama.cpp's
// reported compute buffer — cannot tell the two apart.
func TestCalibrationAppliesToBothTerms(t *testing.T) {
	var zero Calibration
	if got := zero.ApplyKAct(DefaultKAct); got != DefaultKAct {
		t.Errorf("the zero calibration must be the identity, got k_act %v", got)
	}
	if got := zero.ApplyOverhead(OverheadPerGPUBytes); got != OverheadPerGPUBytes {
		t.Errorf("the zero calibration must be the identity, got OH %d", got)
	}

	c := Calibration{Ratio: 1.5, Applied: true, Samples: 4}
	if got := c.ApplyKAct(6); math.Abs(got-9) > 1e-9 {
		t.Errorf("k_act = %v, want 9", got)
	}
	if got := c.ApplyOverhead(400 << 20); got != 600<<20 {
		t.Errorf("OH_gpu = %d, want %d", got, uint64(600)<<20)
	}
}
