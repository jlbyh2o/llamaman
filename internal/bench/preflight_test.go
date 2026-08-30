package bench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The preflight and the run lifecycle (DESIGN sections 3.13 and 10.1).
//
// The preflight's job is to answer, before the user commits, the four questions
// a sweep builder asks: how many points, how long, what would collide, and — the
// one section 10.1 spends a paragraph on — WHAT WOULD BE IGNORED. "Every dropped
// field is dropped LOUDLY … so 'why is my benchmark not measuring my 32k
// context' is answered before the run rather than after it."

func TestPreflightReportsIgnoredFlagsLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	out, err := f.svc.Preflight(ctx, PreflightRequest{
		ModelID: f.ModelID,
		Sweep: Sweep{
			Base: &model.FlagSet{
				CtxSize:      ptr(32768),
				Alias:        ptr("qwen"),
				Parallel:     ptr(4),
				Jinja:        ptr(true),
				SlotSavePath: ptr("/var/lib/llamaman/slots"),
				RopeScaling:  ptr("yarn"),
				Draft:        &model.DraftFlags{NMax: ptr(16)},
				FlashAttn:    ptr(model.FlashAttnAuto),
			},
			NBatch: IntAxis{512, 2048},
			Tests:  []Test{{PP: ptr(512)}},
		},
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	got := map[string]string{}
	for _, f := range out.IgnoredFlags {
		got[f.Field] = f.Reason
	}
	for _, field := range []string{
		"ctx_size", "alias", "parallel", "jinja", "slot_save_path", "rope_scaling", "draft",
	} {
		if _, ok := got[field]; !ok {
			t.Errorf("the preflight does not report %q as ignored", field)
		}
	}
	// The one whose message the design quotes verbatim.
	if reason := got["ctx_size"]; !strings.Contains(reason, "-p/-n/-d") {
		t.Errorf("ctx_size reason = %q; it must say where the context comes from instead", reason)
	}

	// The two SUBSTITUTIONS are notes rather than drops: they change what runs,
	// so they are visible in the results table.
	joined := strings.Join(out.Notes, "\n")
	if !strings.Contains(joined, "-fa 1") {
		t.Errorf("notes = %q; flash_attn auto → 1 must be recorded", out.Notes)
	}

	if out.PointsTotal != 2 {
		t.Errorf("points_total = %d, want 2", out.PointsTotal)
	}
	if out.EstimateFromHistory {
		t.Error("a fresh host reported an estimate from history")
	}
	if out.Estimate != time.Duration(2*DefaultSecondsPerPoint)*time.Second {
		t.Errorf("estimate = %s, want 2 × the default seconds-per-point", out.Estimate)
	}
}

func TestPreflightReportsConflictsAndTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	gpus := `["GPU-aaaa"]`
	f.seedInstance("busy", model.InstanceReady, model.AttributionMeasured, &gpus,
		`{"device_filter":"CUDA0"}`)

	t.Run("targeting the busy GPU", func(t *testing.T) {
		out, err := f.svc.Preflight(ctx, PreflightRequest{
			ModelID: f.ModelID,
			Sweep:   Sweep{Base: &model.FlagSet{DeviceFilter: ptr("CUDA0")}},
		})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if !out.ExclusiveGPU {
			t.Fatal("bench.exclusive_gpu defaults to on")
		}
		if !out.GPUIdentityKnown {
			t.Error("gpu_identity_known is false although the probe returned two cards")
		}
		if len(out.Conflicts) != 1 || out.Conflicts[0].Name != "busy" {
			t.Fatalf("conflicts = %+v, want the busy instance", out.Conflicts)
		}
		if out.Conflicts[0].Assumed {
			t.Error("a measured attribution was reported as assumed")
		}
		if _, ok := out.FreeVRAMBytes["GPU-aaaa"]; !ok {
			t.Error("the preflight does not report free VRAM for the target GPU")
		}
		if _, ok := out.FreeVRAMBytes["GPU-bbbb"]; ok {
			t.Error("the preflight reports free VRAM for a GPU this sweep does not target")
		}
	})

	t.Run("targeting the other GPU", func(t *testing.T) {
		out, err := f.svc.Preflight(ctx, PreflightRequest{
			ModelID: f.ModelID,
			Sweep:   Sweep{Base: &model.FlagSet{DeviceFilter: ptr("CUDA1")}},
		})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if len(out.Conflicts) != 0 {
			t.Errorf("conflicts = %+v; a measured instance on the OTHER card is not a "+
				"collision, which is exactly what per-GPU identity buys", out.Conflicts)
		}
	})

	t.Run("the runtime is reported", func(t *testing.T) {
		out, err := f.svc.Preflight(ctx, PreflightRequest{ModelID: f.ModelID})
		if err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if out.LlamacppVersionID != testVersion || !out.RuntimeReady {
			t.Errorf("runtime = %q ready=%v, want %q ready",
				out.LlamacppVersionID, out.RuntimeReady, testVersion)
		}
		if out.ModelPath == "" {
			t.Error("the preflight does not resolve the model path")
		}
	})
}

func TestPreflightRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)

	t.Run("no model", func(t *testing.T) {
		_, err := f.svc.Preflight(ctx, PreflightRequest{})
		var me model.Error
		if !errors.As(err, &me) || me.Code != model.CodeModelMissing {
			t.Fatalf("got %v, want %s", err, model.CodeModelMissing)
		}
	})

	t.Run("a model that does not exist", func(t *testing.T) {
		_, err := f.svc.Preflight(ctx, PreflightRequest{ModelID: "01NOSUCHMODEL"})
		var me model.Error
		if !errors.As(err, &me) || me.Code != model.CodeModelMissing {
			t.Fatalf("got %v, want %s", err, model.CodeModelMissing)
		}
	})

	t.Run("a sweep past the cap is refused before anything is read", func(t *testing.T) {
		_, err := f.svc.Preflight(ctx, PreflightRequest{
			ModelID: f.ModelID,
			Sweep: Sweep{
				NBatch: seqIntAxis(16), NUbatch: seqIntAxis(16), Threads: seqIntAxis(16),
			},
		})
		var me model.Error
		if !errors.As(err, &me) || me.Code != CodeSweepTooLarge {
			t.Fatalf("got %v, want %s", err, CodeSweepTooLarge)
		}
	})
}

// TestDraftLifecycle: `POST /bench/runs` with `draft: true` creates the row and
// its points and queues nothing; `POST …/start` is what queues it.
func TestDraftLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	created := f.createRun(simpleSweep(), true)

	if created.Job.ID != "" {
		t.Errorf("a draft created job %s", created.Job.ID)
	}
	if got := f.mustRun(created.Run.ID).State; got != model.BenchDraft {
		t.Fatalf("run is %s, want draft", got)
	}
	if len(f.mustPoints(created.Run.ID)) != 2 {
		t.Error("a draft did not expand its points; the estimate has nothing to count")
	}

	job, err := f.svc.Start(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.ID == "" {
		t.Fatal("Start queued no job")
	}
	if got := f.mustRun(created.Run.ID).State; got != model.BenchQueued {
		t.Errorf("run is %s, want queued", got)
	}

	// Starting again is refused: a run that is already queued is watched, not
	// started twice.
	_, err = f.svc.Start(ctx, created.Run.ID)
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeBenchNotStartable {
		t.Fatalf("second Start returned %v, want %s", err, CodeBenchNotStartable)
	}
}

func TestDeleteGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a live job blocks the delete", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		created := f.createRun(simpleSweep(), false)

		err := f.svc.Delete(ctx, created.Run.ID)
		var me model.Error
		if !errors.As(err, &me) || me.Code != CodeBenchRunning {
			t.Fatalf("got %v, want %s", err, CodeBenchRunning)
		}
	})

	t.Run("an outstanding restore blocks the delete", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		created := f.createRun(simpleSweep(), true)

		stopped := `["01INSTbusy"]`
		if err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := f.store.SetBenchRunStopped(ctx, tx, created.Run.ID, &stopped)
			return err
		}); err != nil {
			t.Fatalf("arm the boot sweep: %v", err)
		}

		err := f.svc.Delete(ctx, created.Run.ID)
		var me model.Error
		if !errors.As(err, &me) || me.Code != CodeBenchRunning {
			t.Fatalf("got %v, want %s — deleting the row would lose the list the boot "+
				"restore reads", err, CodeBenchRunning)
		}
	})

	t.Run("a finished run deletes, cascading its points and results", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		id := finishedRun(t, f)

		if err := f.svc.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		err := f.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := f.store.BenchRun(ctx, tx, id)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the run survived the delete: %v", err)
		}
		if len(f.mustPoints(id)) != 0 {
			t.Error("the points did not cascade away")
		}
		if len(f.mustResults(id)) != 0 {
			t.Error("the results did not cascade away")
		}
	})
}

func TestAnnotate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	created := f.createRun(simpleSweep(), true)

	notes := "ran with the fans on max"
	run, err := f.svc.Annotate(ctx, created.Run.ID, "renamed", &notes)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if run.Name != "renamed" {
		t.Errorf("name = %q, want renamed", run.Name)
	}
	if run.Notes == nil || *run.Notes != notes {
		t.Errorf("notes = %v, want %q", run.Notes, notes)
	}

	// An empty name keeps the current one rather than blanking it.
	run, err = f.svc.Annotate(ctx, created.Run.ID, "", nil)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if run.Name != "renamed" {
		t.Errorf("an empty name blanked the run: %q", run.Name)
	}
}

func TestCreateRefusesWithoutARuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	f.sd.SeedVersion(t, testVersion, model.VersionBuilding, true)

	_, err := f.svc.Create(ctx, CreateRequest{ModelID: f.ModelID, Sweep: simpleSweep()})
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeBenchNoRuntime {
		t.Fatalf("got %v, want %s", err, CodeBenchNoRuntime)
	}
}

func TestCancelRefusesWithNoLiveJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	created := f.createRun(simpleSweep(), true)

	_, err := f.svc.Cancel(ctx, created.Run.ID)
	var me model.Error
	if !errors.As(err, &me) || me.Code != CodeBenchNotCancelable {
		t.Fatalf("got %v, want %s", err, CodeBenchNotCancelable)
	}
}
