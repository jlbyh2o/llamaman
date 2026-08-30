package model

// FitObservation is one row of `fit_observations` (DESIGN section 2.11, D32):
// what the fit calculator predicted for a load, and what llama.cpp itself
// reported once that load happened.
//
// The row is written BESIDE THE PREDICTION THAT WAS MADE — section 8.7 is
// explicit about that ordering — which is what makes the ratio meaningful: a
// table of observed buffer sizes with no prediction to compare them against
// cannot correct anything.
//
// Every `actual_*` column is nullable because llama.cpp's startup lines are the
// only source and a build may not print all of them. NULL is "the line was not
// found", never 0: a zero would enter the median as a claim that the buffer was
// free.
type FitObservation struct {
	ID string
	At int64

	// The calibration key of D32: corrections are learned per
	// `(arch, backend, llamacpp_tag)` because the compute buffer's real size is
	// a property of the model family, the backend and the build — not of the
	// host in general.
	Arch        string
	LlamacppTag string
	Backend     Backend
	// GPUName is context for a human reading the table, not part of the key.
	GPUName *string

	// The shape and flags the prediction was made from. They are stored so a row
	// can be understood a month later without joining back to an instance that
	// may since have been reconfigured or deleted.
	NLayer     *int64
	NEmbd      *int64
	NHead      *int64
	NHeadKV    *int64
	NVocab     *int64
	NCtx       *int64
	NBatch     *int64
	NUbatch    *int64
	NParallel  *int64
	FlashAttn  *bool
	TypeK      *string
	TypeV      *string
	NGpuLayers *int64

	// PredictedBytes is the COMPUTE-BUFFER figure this calculator produced, and
	// the denominator of the ratio. It is NOT NULL: a row with nothing predicted
	// has nothing to teach.
	PredictedBytes int64

	ActualWeightsBytes *int64
	ActualKVBytes      *int64
	ActualComputeBytes *int64
	ActualTotalBytes   *int64

	// OOM marks a load that died allocating. Such rows are kept — they are the
	// input to section 8.7's non-negotiable golden rule, that a verdict never
	// says "fits" for a load that OOM'd — and excluded from the median, because
	// the size of a failed allocation is not the size of a successful one.
	OOM bool

	Source FitObservationSource
}

// FitCalibrationKey is the `(arch, backend, llamacpp_tag)` triple corrections
// are grouped by, and the shape of `idx_fit_obs`.
type FitCalibrationKey struct {
	Arch        string
	Backend     Backend
	LlamacppTag string
}
