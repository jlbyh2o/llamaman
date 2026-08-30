package fit

import "sort"

// Calibration (DESIGN section 8.7, D32).
//
// The compute-buffer term is the only genuinely empirical part of this model, so
// it is the only part that gets corrected from observation. Every time an
// instance reaches `ready` — and after every bench point — the supervisor parses
// llama.cpp's own logged buffer sizes and writes a `fit_observations` row beside
// the prediction that was made. This type turns a window of those rows into the
// two corrections section 8.7 names: `k_act` and `OH_gpu`, scaled by the MEDIAN
// observed ratio.
//
// The three guards are all about not learning nonsense from noise:
//
//   - the window is the last CalibrationWindow observations, so a correction
//     that stopped being true stops being applied;
//   - the ratio is clamped to [MinRatio, MaxRatio], because a ratio outside that
//     band is a parse bug or a load that failed, not a model this host actually
//     needs;
//   - at least MinSamples usable observations are required, below which the
//     defaults stand and the report says `modeled`.

// The bounds of section 8.7.
const (
	// CalibrationWindow is how many observations back the median looks.
	CalibrationWindow = 20
	// MinSamples is the floor below which no correction is applied at all.
	MinSamples = 3
	// MinRatio and MaxRatio clamp the correction.
	MinRatio = 0.5
	MaxRatio = 2.0
)

// Observation is one `fit_observations` row reduced to what the correction
// needs: what was predicted for the compute buffers, what llama.cpp actually
// reported, and whether that load died.
//
// The store hands these over newest-first or oldest-first indifferently — the
// median does not care about order — but the WINDOW does, so the caller passes
// at most CalibrationWindow rows, newest first, exactly as `idx_fit_obs` yields
// them.
type Observation struct {
	// PredictedBytes is the compute-buffer figure this calculator produced for
	// that load. A zero makes the row unusable: there is no ratio to take.
	PredictedBytes uint64
	// ActualBytes is `fit_observations.actual_compute_bytes`, parsed from
	// llama.cpp's own startup lines. Zero means the line was not found.
	ActualBytes uint64
	// OOM marks a load that died allocating. It is excluded from the median:
	// learning "the real number is 1.4× bigger" from a load that never finished
	// allocating would teach this calculator the size of a failure.
	OOM bool
}

// Calibration is the correction in force for one `(arch, backend, llamacpp_tag)`
// key. Its zero value is the identity, which is what makes `Calibration{}` a
// legitimate argument at every call site that has not looked anything up.
type Calibration struct {
	// Ratio is the clamped median of actual/predicted. Zero means no correction.
	Ratio float64
	// Samples is how many usable observations the ratio came from.
	Samples int
	// Applied reports whether Ratio is in effect — which is exactly what makes a
	// report `calibrated` rather than `modeled`.
	Applied bool
	// Clamped records that the raw median fell outside [MinRatio, MaxRatio] and
	// was pulled to the bound, so the UI can say the correction is capped rather
	// than showing a number that looks arbitrary.
	Clamped bool
}

// NewCalibration folds a window of observations into a correction.
//
// It is a pure function of the rows, which is the whole reason the lookup and
// the arithmetic are separate: the store fetches by `(arch, backend,
// llamacpp_tag)` and this decides what the rows mean, so the decision is table-
// testable without a database.
func NewCalibration(obs []Observation) Calibration {
	if len(obs) > CalibrationWindow {
		obs = obs[:CalibrationWindow]
	}
	ratios := make([]float64, 0, len(obs))
	for _, o := range obs {
		if o.OOM || o.PredictedBytes == 0 || o.ActualBytes == 0 {
			continue
		}
		ratios = append(ratios, float64(o.ActualBytes)/float64(o.PredictedBytes))
	}
	if len(ratios) < MinSamples {
		return Calibration{Samples: len(ratios)}
	}
	sort.Float64s(ratios)
	m := median(ratios)
	c := Calibration{Ratio: m, Samples: len(ratios), Applied: true}
	if m < MinRatio {
		c.Ratio, c.Clamped = MinRatio, true
	} else if m > MaxRatio {
		c.Ratio, c.Clamped = MaxRatio, true
	}
	return c
}

// median of a sorted slice; the mean of the middle pair on an even count.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// ApplyKAct scales the activation multiplier.
func (c Calibration) ApplyKAct(k float64) float64 {
	if !c.Applied || c.Ratio <= 0 {
		return k
	}
	return k * c.Ratio
}

// ApplyOverhead scales OH_gpu. The same ratio corrects both terms because the
// observation this host records — llama.cpp's reported compute buffer — cannot
// tell them apart, and pretending it could would be a second empirical claim
// with no evidence behind it.
func (c Calibration) ApplyOverhead(oh uint64) uint64 {
	if !c.Applied || c.Ratio <= 0 {
		return oh
	}
	return uint64(float64(oh) * c.Ratio)
}

// Confidence is section 3.9's `confidence` field.
type Confidence string

const (
	// ConfidenceCalibrated means a correction from this host's own loads is in
	// effect.
	ConfidenceCalibrated Confidence = "calibrated"
	// ConfidenceModeled means the defaults are standing — either for want of
	// observations, or because something in the input had to be assumed.
	ConfidenceModeled Confidence = "modeled"
)
