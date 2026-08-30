package app

import (
	"context"
	"errors"

	"github.com/jlbyh2o/llamaman/internal/fit"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/models"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// The two fit seams the composition root fills: D32's calibration lookup for the
// API, and D33/§8.7's PREDICTION for the supervisor.
//
// They are two halves of one loop and it only closes when both are wired:
//
//	estimate → instance starts → llama.cpp prints its own buffer sizes →
//	supervisor parses them and writes a `fit_observations` row BESIDE the
//	prediction → the next estimate reads the median of those rows and corrects
//	k_act and OH_gpu → the report says `calibrated` instead of `modeled`.
//
// Leave the predictor out and the row is never written, so the window never
// reaches fit.MinSamples and every report says `modeled` forever. Leave the
// calibration lookup out and the rows accumulate and are never read. Neither
// gap fails anything visibly, which is exactly why both are wired here rather
// than left for later.

// fitCalibration satisfies api.FitCalibrationSource over the store.
type fitCalibration struct{ st *store.Store }

// ActiveRuntime is the other half of D32's key: corrections are learned per
// `(arch, backend, llamacpp_tag)` because the compute buffer's real size is a
// property of the model family, the backend and the build.
//
// No active build is an ordinary state on a fresh install, and it is reported as
// an empty tag rather than an error: the caller then applies no correction and
// the report says `modeled`, which is the truth.
func (c fitCalibration) ActiveRuntime(ctx context.Context) (string, model.Backend, error) {
	var (
		tag     string
		backend model.Backend
	)
	err := c.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		active, err := c.st.ActiveVersion(ctx, tx)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		v, err := c.st.LlamacppVersion(ctx, tx, active.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, backend = v.Tag, v.Backend
		return nil
	})
	return tag, backend, err
}

// Observations reads the window D32 corrects from, newest first, and reduces
// each row to the three fields fit.NewCalibration needs.
func (c fitCalibration) Observations(ctx context.Context, key model.FitCalibrationKey,
	limit int) ([]fit.Observation, error) {

	var rows []model.FitObservation
	if err := c.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = c.st.FitObservations(ctx, tx, key, limit)
		return err
	}); err != nil {
		return nil, err
	}

	out := make([]fit.Observation, 0, len(rows))
	for _, r := range rows {
		o := fit.Observation{OOM: r.OOM}
		if r.PredictedBytes > 0 {
			o.PredictedBytes = uint64(r.PredictedBytes)
		}
		if r.ActualComputeBytes != nil && *r.ActualComputeBytes > 0 {
			o.ActualBytes = uint64(*r.ActualComputeBytes)
		}
		out = append(out, o)
	}
	return out, nil
}

// fitPredictor satisfies supervisor.FitPredictor: "what did the calculator say
// for this instance?".
//
// The supervisor cannot compute this itself and section 8.7 says why — the
// calculator needs the model's GGUF shape and the live GPU inventory, and a
// reconcile loop that read both on every ready transition would fork nvidia-smi
// and reparse a header to write a history row. The composition root has both
// already, so it answers.
type fitPredictor struct {
	st   *store.Store
	gpus hw.Prober
}

// Predict estimates for the instance's CURRENT configuration.
//
// It deliberately estimates with the IDENTITY calibration, not with the
// correction currently in force, and that is the difference between a
// calibration that learns and one that chases its own tail: D32's ratio is
// `actual / predicted`, so if `predicted` were itself already corrected, the
// next median would measure the residual of the correction rather than the error
// of the model, and the window would converge on 1.0 while the underlying model
// stayed as wrong as it ever was. The row records what the UNCORRECTED model
// said; fit.NewCalibration turns the ratios into the correction.
func (p fitPredictor) Predict(ctx context.Context, instanceID string) (
	supervisor.FitPrediction, bool, error) {

	var (
		inst  model.Instance
		local model.LocalModel
		tag   string
		back  model.Backend
	)
	if err := p.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if inst, err = p.st.Instance(ctx, tx, instanceID); err != nil {
			return err
		}
		if inst.ModelID == nil {
			return nil
		}
		if local, err = p.st.LocalModel(ctx, tx, *inst.ModelID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				local = model.LocalModel{}
				return nil
			}
			return err
		}
		active, err := p.st.ActiveVersion(ctx, tx)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		v, err := p.st.LlamacppVersion(ctx, tx, active.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, back = v.Tag, v.Backend
		return nil
	}); err != nil {
		return supervisor.FitPrediction{}, false, err
	}

	// Every one of these is an ordinary state, not a failure, and each one means
	// the same thing: there is nothing to compare against, so no row is written.
	// An observation with half a ratio is not a weaker signal, it is no signal.
	if local.ID == "" || tag == "" {
		return supervisor.FitPrediction{}, false, nil
	}
	// The mmproj is deliberately not resolved: it contributes WEIGHTS, and the
	// only number this row is the denominator of is the COMPUTE buffer.
	shape, ok := models.FitShape(models.View{LocalModel: local})
	if !ok {
		return supervisor.FitPrediction{}, false, nil
	}
	flags, err := model.ParseFlagSet([]byte(inst.FlagsJSON))
	if err != nil {
		return supervisor.FitPrediction{}, false, nil
	}

	var gpus []hw.GPU
	if p.gpus != nil {
		// A probe failure yields devices with unknown memory rather than none
		// (D16), which is exactly right here: the compute buffer does not depend
		// on how much VRAM is free, only on the shape and the flags.
		gpus, _ = p.gpus.Probe(ctx)
	}
	selected := hw.Select(gpus, hw.Declared(gpus, flags))
	devices := make([]fit.Device, 0, len(selected))
	for i, g := range selected {
		d := fit.Device{Index: i, UUID: g.UUID, Name: g.Name}
		if g.VRAMKnown() {
			d.TotalBytes, d.FreeBytes, d.Known = *g.VRAMTotalBytes, *g.VRAMFreeBytes, true
		}
		devices = append(devices, d)
	}

	rep := fit.Estimate(fit.Request{
		Model:   shape,
		Flags:   models.FitFlags(flags),
		Devices: devices,
	})
	if rep.ComputeBytes == 0 {
		return supervisor.FitPrediction{}, false, nil
	}

	in := rep.Inputs
	pred := supervisor.FitPrediction{
		Arch:                  shape.Arch,
		Backend:               back,
		LlamacppTag:           tag,
		PredictedComputeBytes: int64(rep.ComputeBytes),

		NLayer:     i64(in.NLayer),
		NEmbd:      i64(in.NEmbd),
		NHead:      i64(in.NHead),
		NVocab:     i64(in.NVocab),
		NCtx:       i64(in.NCtx),
		NBatch:     i64(in.NBatch),
		NUbatch:    i64(in.NUbatch),
		NParallel:  i64(in.NParallel),
		FlashAttn:  &in.FlashAttn,
		TypeK:      strPtr(in.TypeK),
		TypeV:      strPtr(in.TypeV),
		NGpuLayers: i64(rep.NGpuLayers),
	}
	if len(in.NHeadKV) > 0 {
		pred.NHeadKV = i64(in.NHeadKV[0])
	}
	if len(selected) > 0 {
		pred.GPUName = selected[0].Name
	}
	return pred, true, nil
}

func i64(n int) *int64 {
	if n == 0 {
		return nil
	}
	v := int64(n)
	return &v
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
