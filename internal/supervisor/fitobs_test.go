package supervisor_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// The §5.8 fit-observation hook, end to end through a reconcile pass.
//
// Two rules are load-bearing and neither is about parsing:
//
//   - the observation happens on the FIRST `ready` of a run and nowhere else, so
//     an instance that serves for a week contributes one row rather than one per
//     tick;
//   - it is gated on `runtime_info.journal_read = 'ok'` (D77). Without journal
//     access the scan would return nothing, and a row written from an empty scan
//     is a `0` actual that drags every median toward zero — a calculator
//     "calibrated" from a host it cannot observe. F23 names the honest
//     degradation: skip the row, stay `modeled`.

// fakeJournal answers the tail with canned lines and counts the calls.
type fakeJournal struct {
	mu    sync.Mutex
	lines []string
	err   error
	calls []string
}

func (j *fakeJournal) Tail(_ context.Context, unit string, _ int) ([]string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, unit)
	if j.err != nil {
		return nil, j.err
	}
	return j.lines, nil
}

func (j *fakeJournal) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.calls)
}

// fakePredictor stands in for the composition root's calculator call.
type fakePredictor struct {
	pred supervisor.FitPrediction
	ok   bool
	err  error
}

func (p fakePredictor) Predict(context.Context, string) (supervisor.FitPrediction, bool, error) {
	return p.pred, p.ok, p.err
}

func loadLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "journal", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// observeFixture drives one instance to its first `ready` through a supervisor
// wired with a journal and a predictor.
type observeFixture struct {
	*fixture
	sup     *supervisor.Supervisor
	journal *fakeJournal
}

func newObserveFixture(t *testing.T, journalRead string, pred supervisor.FitPredictor) *observeFixture {
	t.Helper()
	f := newFixture(t, nil)
	f.SeedRuntimeInfo(t, "boot-1", f.clock.now().Add(-time.Hour).UnixMilli(), f.clock.now().UnixMilli())
	if journalRead != "" {
		f.Exec(t, `UPDATE runtime_info SET journal_read = ? WHERE id = 1`, journalRead)
	}

	j := &fakeJournal{lines: loadLines(t, "load-cuda.txt")}
	sup, err := supervisor.New(supervisor.Config{
		Store:    f.DB,
		Settings: fakeSettings{"instances.health_poll_sec": 5, "instances.start_timeout_sec": 900},
		Events:   f.ev,
		Control:  f.ctl,
		Prober:   f.probe,
		StateDir: f.Dir,
		Now:      f.clock.now,
		NewID:    f.ids.next,
		Journal:  j,
		Fit:      pred,
		Host: func() (supervisor.HostBoot, error) {
			return supervisor.HostBoot{ID: "boot-1", At: f.clock.now().Add(-time.Hour)}, nil
		},
		Exe: func(int) (string, error) { return "", context.Canceled },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &observeFixture{fixture: f, sup: sup, journal: j}
}

func (f *observeFixture) reconcileWith(t *testing.T) {
	t.Helper()
	if err := f.sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// driveToReady runs the start, the launcher's half, and the first 200.
func (f *observeFixture) driveToReady(t *testing.T) {
	t.Helper()
	f.reconcileWith(t)
	f.clock.advance(time.Second)
	f.launchLikeInstanceExec(t, 4242)
	f.probe.set(http.StatusOK)
	f.clock.advance(time.Second)
	f.reconcileWith(t)
	if got := f.Status(t, f.inst.ID).State; got != model.InstanceReady {
		t.Fatalf("state = %q, want ready", got)
	}
}

func goodPrediction() supervisor.FitPrediction {
	return supervisor.FitPrediction{
		Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621",
		GPUName:               "NVIDIA GeForce RTX 3090",
		PredictedComputeBytes: 280 << 20,
		NCtx:                  ptr(int64(8192)),
		NGpuLayers:            ptr(int64(33)),
	}
}

func (f *observeFixture) observations(t *testing.T) []model.FitObservation {
	t.Helper()
	var out []model.FitObservation
	err := f.DB.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = f.DB.FitObservations(ctx, tx, model.FitCalibrationKey{
			Arch: "llama", Backend: model.BackendCUDA, LlamacppTag: "b10621",
		}, 0)
		return err
	})
	if err != nil {
		t.Fatalf("read observations: %v", err)
	}
	return out
}

// TestFitObservationOnFirstReady is the happy path: one scan, one stamped
// `fit_report_json`, one `fit_observations` row — and nothing more on later
// passes.
func TestFitObservationOnFirstReady(t *testing.T) {
	f := newObserveFixture(t, string(model.JournalOK), fakePredictor{pred: goodPrediction(), ok: true})
	f.driveToReady(t)

	st := f.Status(t, f.inst.ID)
	if st.FitReportJSON == nil {
		t.Fatal("fit_report_json was not stamped")
	}
	if !strings.Contains(*st.FitReportJSON, `"fit_layers":33`) {
		t.Errorf("fit_report_json should carry llama.cpp's own projection (D33): %s",
			*st.FitReportJSON)
	}

	rows := f.observations(t)
	if len(rows) != 1 {
		t.Fatalf("got %d observations, want 1", len(rows))
	}
	o := rows[0]
	if o.PredictedBytes != 280<<20 {
		t.Errorf("predicted = %d, want %d", o.PredictedBytes, 280<<20)
	}
	if o.ActualComputeBytes == nil || *o.ActualComputeBytes != 304<<20 {
		t.Errorf("actual compute = %v, want %d", o.ActualComputeBytes, 304<<20)
	}
	if o.Source != model.FitFromInstanceStart {
		t.Errorf("source = %q, want instance_start", o.Source)
	}
	if o.GPUName == nil || *o.GPUName == "" {
		t.Error("the GPU name should be recorded for a human reading the table")
	}

	// Later passes are not first readys. A run that serves for a week must
	// contribute one row, not one per tick.
	f.clock.advance(30 * time.Second)
	f.reconcileWith(t)
	f.clock.advance(30 * time.Second)
	f.reconcileWith(t)
	if n := f.journal.count(); n != 1 {
		t.Errorf("the journal was scanned %d times, want 1", n)
	}
	if rows := f.observations(t); len(rows) != 1 {
		t.Errorf("got %d observations after three passes, want 1", len(rows))
	}
}

// TestFitObservationSkippedWithoutJournalAccess is D77 and F23: no journal, no
// row, and reports stay `modeled` rather than being calibrated from an empty
// scan.
func TestFitObservationSkippedWithoutJournalAccess(t *testing.T) {
	for _, state := range []model.JournalRead{model.JournalDenied, model.JournalUnavailable} {
		t.Run(string(state), func(t *testing.T) {
			f := newObserveFixture(t, string(state), fakePredictor{pred: goodPrediction(), ok: true})
			f.driveToReady(t)

			if n := f.journal.count(); n != 0 {
				t.Errorf("the journal was read %d times with journal_read = %q", n, state)
			}
			if st := f.Status(t, f.inst.ID); st.FitReportJSON != nil {
				t.Errorf("fit_report_json = %s, want NULL", *st.FitReportJSON)
			}
			if rows := f.observations(t); len(rows) != 0 {
				t.Errorf("got %d observations, want none", len(rows))
			}
		})
	}
}

// TestFitReportStampedWithoutAPrediction: D33's "reported by llama.cpp" panel
// works with no calculator behind it. What it must not do is write a
// calibration row with nothing to compare against.
func TestFitReportStampedWithoutAPrediction(t *testing.T) {
	f := newObserveFixture(t, string(model.JournalOK), nil)
	f.driveToReady(t)

	if st := f.Status(t, f.inst.ID); st.FitReportJSON == nil {
		t.Error("fit_report_json should still be stamped with no predictor wired")
	}
	if rows := f.observations(t); len(rows) != 0 {
		t.Errorf("got %d observations with no prediction, want none", len(rows))
	}
}

// TestFitObservationSurvivesAJournalFailure: an instance that is serving
// requests must not be reported as unhealthy because a history row could not be
// written.
func TestFitObservationSurvivesAJournalFailure(t *testing.T) {
	f := newObserveFixture(t, string(model.JournalOK), fakePredictor{pred: goodPrediction(), ok: true})
	f.journal.err = context.DeadlineExceeded
	f.driveToReady(t)

	if got := f.Status(t, f.inst.ID).State; got != model.InstanceReady {
		t.Errorf("state = %q, want ready — a failed scan is not an unhealthy instance", got)
	}
	if rows := f.observations(t); len(rows) != 0 {
		t.Errorf("got %d observations after a failed scan, want none", len(rows))
	}
}
