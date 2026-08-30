package store

import (
	"context"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The `fit_observations` round trip (DESIGN sections 2.11 and 8.7).
//
// Two properties carry the weight here and neither is about SQL: NULL must
// survive as NULL — an `actual_*` column the parser did not find is "not
// learned", never 0, and a 0 entering the median would be a claim that the
// buffer was free — and the read must be a WINDOW, newest first, so a correction
// that stopped being true ages out.

func obsFixture(id string, at int64, key model.FitCalibrationKey) model.FitObservation {
	return model.FitObservation{
		ID: id, At: at,
		Arch: key.Arch, Backend: key.Backend, LlamacppTag: key.LlamacppTag,
		PredictedBytes: 1_000_000,
		Source:         model.FitFromInstanceStart,
	}
}

func TestFitObservationRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := model.FitCalibrationKey{Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621"}

	full := obsFixture("01JFITFULL0000000000000000", 1788042587000, key)
	full.GPUName = ptr("NVIDIA GeForce RTX 3090")
	full.NLayer, full.NEmbd, full.NHead = ptr(int64(32)), ptr(int64(4096)), ptr(int64(32))
	full.NHeadKV, full.NVocab = ptr(int64(8)), ptr(int64(32000))
	full.NCtx, full.NBatch, full.NUbatch, full.NParallel =
		ptr(int64(8192)), ptr(int64(2048)), ptr(int64(512)), ptr(int64(4))
	full.FlashAttn = ptr(true)
	full.TypeK, full.TypeV = ptr("q8_0"), ptr("q8_0")
	full.NGpuLayers = ptr(int64(33))
	full.ActualWeightsBytes = ptr(int64(4_920_000_000))
	full.ActualKVBytes = ptr(int64(1_207_959_552))
	full.ActualComputeBytes = ptr(int64(1_200_000))
	full.ActualTotalBytes = ptr(int64(6_129_159_552))

	// The second row is what a build that printed only some of its buffer lines
	// leaves behind: every unparsed column stays NULL.
	sparse := obsFixture("01JFITSPARSE00000000000000", 1788042588000, key)
	sparse.OOM = true
	sparse.Source = model.FitFromBench

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertFitObservation(ctx, tx, full); err != nil {
			return err
		}
		return s.InsertFitObservation(ctx, tx, sparse)
	})

	var got []model.FitObservation
	if err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		got, err = s.FitObservations(ctx, tx, key, 0)
		return err
	}); err != nil {
		t.Fatalf("FitObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}

	// Newest first, by id.
	if got[0].ID != sparse.ID {
		t.Errorf("first row = %s, want the newest (%s)", got[0].ID, sparse.ID)
	}
	if !got[0].OOM {
		t.Error("the oom flag must survive the round trip")
	}
	if got[0].Source != model.FitFromBench {
		t.Errorf("source = %q, want bench", got[0].Source)
	}
	for name, p := range map[string]any{
		"gpu_name":             got[0].GPUName,
		"actual_compute_bytes": got[0].ActualComputeBytes,
		"flash_attn":           got[0].FlashAttn,
		"type_k":               got[0].TypeK,
	} {
		if !isNil(p) {
			t.Errorf("%s = %v, want NULL — an unparsed line is not a zero", name, p)
		}
	}

	back := got[1]
	if back.ID != full.ID || back.Arch != "llama" || back.Backend != model.BackendCUDA {
		t.Fatalf("identity did not round trip: %+v", back)
	}
	if back.PredictedBytes != full.PredictedBytes {
		t.Errorf("predicted = %d, want %d", back.PredictedBytes, full.PredictedBytes)
	}
	if back.ActualComputeBytes == nil || *back.ActualComputeBytes != 1_200_000 {
		t.Errorf("actual_compute_bytes = %v, want 1200000", back.ActualComputeBytes)
	}
	if back.FlashAttn == nil || !*back.FlashAttn {
		t.Errorf("flash_attn = %v, want true", back.FlashAttn)
	}
	if back.NGpuLayers == nil || *back.NGpuLayers != 33 {
		t.Errorf("n_gpu_layers = %v, want 33", back.NGpuLayers)
	}
	if back.OOM {
		t.Error("a successful load must not be recorded as an OOM")
	}
}

// TestFitObservationsAreKeyedAndWindowed: corrections are learned per
// `(arch, backend, llamacpp_tag)` (D32), and the limit is a window over the
// newest rows rather than an arbitrary page.
func TestFitObservationsAreKeyedAndWindowed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	cuda := model.FitCalibrationKey{Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621"}
	cpu := model.FitCalibrationKey{Arch: "llama", Backend: model.BackendCPU, LlamacppTag: "b10621"}
	older := model.FitCalibrationKey{Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10500"}
	other := model.FitCalibrationKey{Arch: "gemma3", Backend: model.BackendCUDA, LlamacppTag: "b10621"}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		for i, key := range []model.FitCalibrationKey{cpu, older, other} {
			id := "01JFITOTHER00000000000000" + string(rune('A'+i))
			if err := s.InsertFitObservation(ctx, tx, obsFixture(id, 1, key)); err != nil {
				return err
			}
		}
		// Twenty-five rows on the key under test, ids ascending with time.
		for i := range 25 {
			id := "01JFITCUDA000000000000" + string(rune('A'+i/26)) + string(rune('A'+i%26))
			o := obsFixture(id, int64(1000+i), cuda)
			o.ActualComputeBytes = ptr(int64(1_000_000 + i))
			if err := s.InsertFitObservation(ctx, tx, o); err != nil {
				return err
			}
		}
		return nil
	})

	var (
		window []model.FitObservation
		count  int
	)
	if err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		if window, err = s.FitObservations(ctx, tx, cuda, 0); err != nil {
			return err
		}
		count, err = s.CountFitObservations(ctx, tx, cuda)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(window) != DefaultFitObservationLimit {
		t.Fatalf("window = %d rows, want the default %d", len(window), DefaultFitObservationLimit)
	}
	if count != 25 {
		t.Errorf("count = %d, want 25 — the key must exclude the other three rows", count)
	}
	// The window holds the NEWEST rows: the last inserted is first, and the
	// oldest five are absent.
	if window[0].ActualComputeBytes == nil || *window[0].ActualComputeBytes != 1_000_024 {
		t.Errorf("newest row = %v, want the last inserted", window[0].ActualComputeBytes)
	}
	for _, o := range window {
		if o.ActualComputeBytes == nil || *o.ActualComputeBytes < 1_000_005 {
			t.Errorf("an aged-out row is inside the window: %v", o.ActualComputeBytes)
		}
	}

	// A key with no rows is an empty slice, not an error: a fresh install has no
	// observations and its reports say `modeled`.
	var none []model.FitObservation
	if err := s.Read(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		none, err = s.FitObservations(ctx, tx,
			model.FitCalibrationKey{Arch: "nobody", Backend: model.BackendCPU, LlamacppTag: "b1"}, 0)
		return err
	}); err != nil {
		t.Fatalf("read for an unknown key: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unknown key returned %d rows", len(none))
	}
}

func isNil(v any) bool {
	switch p := v.(type) {
	case *string:
		return p == nil
	case *int64:
		return p == nil
	case *bool:
		return p == nil
	default:
		return v == nil
	}
}
