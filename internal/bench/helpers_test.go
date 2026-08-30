package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/hw"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/procx"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/store/storetest"
)

// The fixture is a REAL store and a REAL job queue over a temp state directory,
// following the precedent internal/llamacpp set for the same reason.
//
// Faking either would have made these tests assertions about the fakes. The D75
// lease is a conditional UPDATE; the boot restore is a query with a deliberately
// state-free predicate; §2.3a's pairing is two rows written in one transaction.
// None of that survives a stub. Nothing here writes SQL — every seed goes
// through storetest or through the same store methods production uses — so
// D49's "only internal/store contains SQL" holds.

const (
	testBootID  = "01BENCHBENCHBENCHBENCHBENCH"
	otherBootID = "01DEADDEADDEADDEADDEADDEAD"
	testVersion = "b10621-cpu-prebuilt"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeFleet is the desired-state writer. It records every call so a test can
// assert the ORDER — stopped, then running with `bench_restore` — and it writes
// through the store so the rows a reader sees are the rows production would see.
type fakeFleet struct {
	mu    sync.Mutex
	st    *store.Store
	clock *testClock
	calls []fleetCall
	// failFor makes SetDesiredState fail for one instance, which is how the
	// "the restore could not finish" path is reached without breaking anything
	// else.
	failFor string
	err     error
}

type fleetCall struct {
	InstanceID string
	Desired    model.DesiredState
	Trigger    model.PendingTrigger
}

func (f *fakeFleet) SetDesiredState(ctx context.Context, id string, desired model.DesiredState,
	trigger model.PendingTrigger) (instances.View, error) {

	f.mu.Lock()
	f.calls = append(f.calls, fleetCall{InstanceID: id, Desired: desired, Trigger: trigger})
	fail := f.failFor == id
	f.mu.Unlock()
	if fail {
		return instances.View{}, f.err
	}

	err := f.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		ok, err := f.st.SetInstanceDesiredState(ctx, tx, id, desired, f.clock.Now().UnixMilli())
		if err != nil {
			return err
		}
		if !ok {
			return store.ErrNotFound
		}
		if desired == model.DesiredRunning && trigger != "" {
			if _, err := f.st.StampPendingStart(ctx, tx, id, trigger, nil,
				f.clock.Now().UnixMilli()); err != nil {
				return err
			}
		}
		// The instance's OBSERVED state follows its desired state immediately,
		// which is what a supervisor pass would have done. Without it the stop
		// wait would spin until its grace period, and the test would be an
		// assertion about a timeout rather than about the protocol.
		st, err := f.st.InstanceStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		st.State = model.InstanceStopped
		if desired == model.DesiredRunning {
			st.State = model.InstanceReady
		}
		st.LastChangeAt = f.clock.Now().UnixMilli()
		_, err = f.st.UpdateInstanceStatus(ctx, tx, st)
		return err
	})
	return instances.View{}, err
}

func (f *fakeFleet) Calls() []fleetCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleetCall(nil), f.calls...)
}

// fakeRunner stands in for llama-bench. It records the argv it was handed and
// replies with a recorded output fixture, so the whole worker — preflight,
// per-point loop, parsing, persistence, finalizer — runs without a GPU.
type fakeRunner struct {
	mu      sync.Mutex
	argv    [][]string
	stdout  []byte
	err     error
	onPoint func(ctx context.Context, n int) ([]byte, error)
}

func (r *fakeRunner) Run(ctx context.Context, c procx.Cmd) (procx.Result, error) {
	r.mu.Lock()
	n := len(r.argv)
	r.argv = append(r.argv, append([]string{c.Path}, c.Args...))
	out, err := r.stdout, r.err
	hook := r.onPoint
	r.mu.Unlock()

	if hook != nil {
		out, err = hook(ctx, n)
	}
	if c.OnLine != nil {
		for _, line := range splitLines(string(out)) {
			c.OnLine(procx.Line{Stream: procx.StreamStdout, Text: line})
		}
	}
	if err != nil {
		return procx.Result{ExitCode: 1}, err
	}
	return procx.Result{ExitCode: 0}, nil
}

func (r *fakeRunner) Argv() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.argv...)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// fakeProber is the GPU inventory. Two cards, because the guard's whole reason
// for reading `gpu_uuid` is telling "loaded on the GPU you are about to
// benchmark" from "loaded on the other one" (D17).
type fakeProber struct {
	gpus []hw.GPU
	err  error
}

func (p fakeProber) Probe(context.Context) ([]hw.GPU, error) { return p.gpus, p.err }

func twoGPUs() []hw.GPU {
	return []hw.GPU{
		{Index: 0, UUID: "GPU-aaaa", Name: "NVIDIA GeForce RTX 4090",
			VRAMTotalBytes: hw.Bytes(25769803776), VRAMFreeBytes: hw.Bytes(25000000000),
			DriverVersion: "560.35.03"},
		{Index: 1, UUID: "GPU-bbbb", Name: "NVIDIA GeForce RTX 3060",
			VRAMTotalBytes: hw.Bytes(12884901888), VRAMFreeBytes: hw.Bytes(12000000000),
			DriverVersion: "560.35.03"},
	}
}

// fakeReconciler counts the supervisor passes the protocol asks for.
type fakeReconciler struct {
	mu    sync.Mutex
	calls int
}

func (r *fakeReconciler) Reconcile(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

type fixture struct {
	t     *testing.T
	sd    *storetest.StateDir
	store *store.Store
	queue *jobs.Queue
	svc   *Service
	fleet *fakeFleet
	run   *fakeRunner
	sup   *fakeReconciler
	clock *testClock
	// ModelID is the seeded `models` row, ready with a file on disk.
	ModelID string
}

// newFixture builds a daemon-shaped set of collaborators around a temp state
// directory, with two GPUs, one ready model and one active llama.cpp build.
func newFixture(t *testing.T, tweak func(*Config)) *fixture {
	t.Helper()
	ctx := context.Background()

	sd := storetest.NewStateDir(t, testVersion, "")
	// The bench binary beside the server one: nothing execs it (the Runner is
	// faked), but the argv the points carry names it, and a fixture whose paths
	// are real is a fixture whose golden argv means something.
	benchPath := filepath.Join(sd.Dir, "versions", testVersion, "bin", "llama-bench")
	if err := os.WriteFile(benchPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write the stub llama-bench: %v", err)
	}

	clock := &testClock{now: time.Unix(1_700_000_000, 0).UTC()}
	// A fast heartbeat, because that tick is what carries `cancel_requested`
	// into the worker (§6.5). At the production interval a cancel issued inside
	// a test's point would never be observed and the test would be asserting
	// that nothing happened.
	q, err := jobs.New(sd.DB, jobs.Options{
		BootID: testBootID, Now: clock.Now, HeartbeatEvery: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}

	set := settings.New(settings.NewRegistry(), sd.DB)
	if err := set.Load(ctx); err != nil {
		t.Fatalf("load settings: %v", err)
	}

	modelID := "01MODELQWEN"
	sd.SeedModel(t, modelID, true)

	fleet := &fakeFleet{st: sd.DB, clock: clock}
	runner := &fakeRunner{stdout: mustFixture(t, "llama-bench-pp-tg.json")}
	sup := &fakeReconciler{}

	cfg := Config{
		Store:       sd.DB,
		Queue:       q,
		Events:      events.NewRecorder(sd.DB, events.NewHub(0)),
		Settings:    set,
		Fleet:       fleet,
		Supervisor:  sup,
		GPUs:        fakeProber{gpus: twoGPUs()},
		Runner:      runner,
		StateDir:    sd.Dir,
		BootID:      testBootID,
		RestorePoll: time.Millisecond,
		StopGrace:   50 * time.Millisecond,
		Now:         clock.Now,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("bench.New: %v", err)
	}
	if err := q.Register(svc.NewWorker()); err != nil {
		t.Fatalf("register the bench worker: %v", err)
	}

	return &fixture{
		t: t, sd: sd, store: sd.DB, queue: q, svc: svc,
		fleet: fleet, run: runner, sup: sup, clock: clock, ModelID: modelID,
	}
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// seedInstance writes a loaded instance holding one GPU, with the attribution
// the guard reads.
func (f *fixture) seedInstance(name string, state model.InstanceState,
	attribution model.GPUAttribution, gpuUUIDsJSON *string, flagsJSON string) string {

	f.t.Helper()
	id := "01INST" + name
	inst := storetest.NewInstance(id, name, f.ModelID, 8000+len(name), 21000+len(name))
	if flagsJSON != "" {
		inst.FlagsJSON = flagsJSON
	}
	inst.DesiredState = model.DesiredRunning
	f.sd.SeedInstance(f.t, inst)

	err := f.store.Write(context.Background(), func(ctx context.Context, tx store.Tx) error {
		st, err := f.store.InstanceStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		st.State = state
		st.GPUAttribution = attribution
		st.GPUUUIDsJSON = gpuUUIDsJSON
		st.LastChangeAt = f.clock.Now().UnixMilli()
		_, err = f.store.UpdateInstanceStatus(ctx, tx, st)
		return err
	})
	if err != nil {
		f.t.Fatalf("seed instance status: %v", err)
	}
	return id
}

// createRun posts a sweep through the real service.
func (f *fixture) createRun(sweep Sweep, draft bool) CreateResult {
	f.t.Helper()
	res, err := f.svc.Create(context.Background(), CreateRequest{
		Name:        "test run",
		ModelID:     f.ModelID,
		Repetitions: 3,
		Sweep:       sweep,
		Draft:       draft,
	})
	if err != nil {
		f.t.Fatalf("Create: %v", err)
	}
	return res
}

func (f *fixture) mustRun(id string) store.BenchRun {
	f.t.Helper()
	var run store.BenchRun
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		run, err = f.store.BenchRun(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read run %s: %v", id, err)
	}
	return run
}

func (f *fixture) mustPoints(id string) []store.BenchPoint {
	f.t.Helper()
	var points []store.BenchPoint
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		points, err = f.store.BenchPoints(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read the points of %s: %v", id, err)
	}
	return points
}

func (f *fixture) mustResults(id string) []store.BenchResult {
	f.t.Helper()
	var results []store.BenchResult
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		results, err = f.store.BenchResults(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read the results of %s: %v", id, err)
	}
	return results
}

func (f *fixture) mustLease() store.BenchLease {
	f.t.Helper()
	var l store.BenchLease
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		l, err = f.store.BenchLease(ctx, tx)
		return err
	})
	if err != nil {
		f.t.Fatalf("read the bench lease: %v", err)
	}
	return l
}

func (f *fixture) mustJob(id string) model.Job {
	f.t.Helper()
	var j model.Job
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		j, err = f.store.Job(ctx, tx, id)
		return err
	})
	if err != nil {
		f.t.Fatalf("read job %s: %v", id, err)
	}
	return j
}

func (f *fixture) instance(id string) model.Instance {
	f.t.Helper()
	return f.sd.Instance(f.t, id)
}

// orAbsent renders an optional field for a failure message. Every BenchLease
// column is a pointer, so formatting one with `%v` prints an address exactly
// when an assertion fires and the value is the thing worth reading.
func orAbsent[T any](p *T, absent string) string {
	if p == nil {
		return absent
	}
	return fmt.Sprint(*p)
}

func (f *fixture) benchLive() bool {
	f.t.Helper()
	var live bool
	err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		live, err = f.store.BenchLive(ctx, tx)
		return err
	})
	if err != nil {
		f.t.Fatalf("read whether a bench is live: %v", err)
	}
	return live
}
