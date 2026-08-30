package app

import (
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/bench"
)

// Step 12's benchmark half: the bench runner and its one worker (DESIGN
// sections 3.13 and 10).
//
// The `bench_run` kind is registered HERE, at construction, and not in serve().
// §2.3's boot triage looks a worker up in the registry to move its domain row in
// the same transaction as the job row, and for this kind that lookup is
// load-bearing rather than tidy: the DomainWriter's answer for `interrupted` is
// a deliberate NO-OP, because `bench_runs.state='running'` with `restore_done=0`
// is the stop-and-restore finalizer's input. A daemon that registered after
// RecoverOrphans ran would have no DomainWriter, generic triage would mark the
// run `failed`, and a benchmark that stopped two serving instances would leave
// them down forever.
func (d *daemon) buildBench() error {
	svc, err := bench.New(bench.Config{
		Store:    d.store,
		Queue:    d.queue,
		Events:   d.recorder,
		Settings: d.settings,
		// Fleet writes the DESIRED axis and Supervisor takes the corrective
		// action now rather than at the next tick — the same split §5.8 gives
		// every other restart, and what makes a bench-stopped instance come
		// back with `instance_starts.trigger='bench_restore'`.
		Fleet:      d.instances,
		Supervisor: d.supervisor,
		// §8.6/D17's attribution probe, which is what gives the exclusivity
		// guard the per-GPU identity it intersects. It is the SAME prober the
		// supervisor writes `instance_status.gpu_uuids_json` from, so the two
		// sides of that intersection cannot come from different samples of a
		// host whose cards were renumbered in between.
		//
		// A probe that FAILS still fails the guard closed — every loaded
		// instance is then treated as a conflict, because an empty inventory
		// would otherwise make every intersection empty and wave the bench
		// through (§10: "a bench is never launched into a collision merely
		// because attribution was unavailable").
		GPUs:     d.gpus,
		StateDir: d.stateDir,
		BootID:   d.bootID,
		Now:      d.opts.Now,
		Logger:   d.log,
	})
	if err != nil {
		return fmt.Errorf("build the bench runner: %w", err)
	}
	d.bench = svc

	if err := d.queue.Register(svc.NewWorker()); err != nil {
		return err
	}
	return nil
}
