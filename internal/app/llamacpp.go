package app

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/llamacpp"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/source"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

// Step 12's llama.cpp half: the lifecycle service, its three workers, and the
// nightly maintenance worker beside them.
//
// The three llama.cpp kinds are registered HERE, at construction, and not in
// serve(). The order matters and §2.3 says why: boot triage looks a worker up in
// the registry to move its domain row in the same transaction as the job row, so
// a daemon that registered after RecoverOrphans ran would triage an interrupted
// build with no DomainWriter to keep `llamacpp_versions` in step.

// buildLlamacpp constructs the version lifecycle service and registers every
// worker this daemon runs.
func (d *daemon) buildLlamacpp() error {
	// The GitHub token is read through a FUNCTION on every request, not captured
	// here: a token the user deletes through `DELETE /api/v1/github/token` must
	// stop being sent at the next request rather than at the next restart
	// (§6.2). With none stored the client is anonymous, which is a supported
	// mode — 60 requests an hour, served stale-while-revalidating.
	var githubToken func(context.Context) (string, error)
	if d.secrets != nil {
		githubToken = d.secrets.TokenFunc(model.SecretGitHubToken)
	}
	releases := github.New(github.Options{
		UserAgent: UserAgent(),
		Token:     githubToken,
		Now:       d.opts.Now,
		Logger:    d.log,
	})
	// Kept on the daemon so `GET /api/v1/github/token` can report the current
	// api.github.com headroom beside the token (§6.2).
	d.releases = releases

	// ONE registry, shared. The builder writes its running build's sink into it
	// and `GET /api/v1/llamacpp/versions/{id}/log` reads it back for the live
	// tail; two registries would leave that endpoint looking for a running build
	// in a map nothing writes to, and it would report `live: false` for every
	// build there has ever been.
	logs := source.NewLogRegistry()

	// The source builder probes its own toolchain: §6.5's `preflight` row is the
	// same probe the wizard's toolchain step and `GET /llamacpp/plan` read, so a
	// build and a plan can never disagree about whether this host can build.
	builder := &source.Builder{
		Layout: source.Layout{StateDir: d.stateDir},
		Logs:   logs,
		Now:    d.opts.Now,
		Logger: d.log,
	}

	svc, err := llamacpp.New(llamacpp.Config{
		Store:     d.store,
		Queue:     d.queue,
		Events:    d.recorder,
		Settings:  d.settings,
		Instances: d.instances,
		StateDir:  d.stateDir,
		BootID:    d.bootID,
		Releases:  releases,
		// The same client answers §3.5's release listing. Toolchain and
		// FreeSpace are left nil: the defaults probe THIS host, which is what
		// `GET /llamacpp/plan` is for.
		ReleaseList: releases,
		Logs:        logs,
		// Bench and Notify are the documented nils: internal/bench and the
		// notifications sink are not wired into this daemon yet, so no bench can
		// be live and a failed canary is reported through `events` alone. Both
		// facts are true of THIS build rather than assumed about every host.
		Now:    d.opts.Now,
		Logger: d.log,
	})
	if err != nil {
		return fmt.Errorf("build the llama.cpp lifecycle service: %w", err)
	}
	d.llamacpp = svc

	// GPUs is nil until internal/hw ships its NvidiaSMIProber: D21's compute
	// capabilities then come from `llamacpp.cuda_arch_list` alone, and a CUDA
	// build with neither fails its own preflight with a message that says so —
	// which is what D21 asks for, since `all` multiplies compile time and
	// `native` silently produces a binary that will not run if the GPU set
	// changes.
	installer, err := svc.NewInstallWorker(llamacpp.InstallWorkerConfig{
		Source: builder,
	})
	if err != nil {
		return fmt.Errorf("build the llamacpp_install worker: %w", err)
	}

	// The roll writes `desired_state` through the instances service and asks the
	// supervisor to take its one corrective action now rather than at its next
	// tick. That split is §5.8's, and it is what makes an instance that dies
	// mid-roll the ordinary reconcile loop's problem rather than a roll's.
	roller := &llamacpp.SupervisorRoller{
		Store:      d.store,
		Fleet:      d.instances,
		Supervisor: d.supervisor,
		Health:     healthProbe{prober: supervisor.NewHTTPProber()},
		Settings:   d.settings,
		Now:        d.opts.Now,
	}

	if err := d.queue.Register(installer); err != nil {
		return err
	}
	if err := d.queue.Register(svc.NewActivateWorker(llamacpp.ActivateWorkerConfig{
		Roller: roller,
	})); err != nil {
		return err
	}
	if err := d.queue.Register(svc.NewDeleteWorker()); err != nil {
		return err
	}
	if err := d.queue.Register(&maintenanceWorker{
		store:    d.store,
		settings: d.settings,
		now:      d.opts.Now,
		log:      d.log,
		stateDir: d.stateDir,
	}); err != nil {
		return err
	}
	return nil
}

// healthProbe adapts the supervisor's own HTTP prober to the roll's gate, so
// "healthy" means the same thing to the roll and to the reconcile loop.
type healthProbe struct {
	prober *supervisor.HTTPProber
}

// Healthy implements llamacpp.HealthProbe.
func (h healthProbe) Healthy(ctx context.Context, port int) bool {
	return h.prober.Health(ctx, port).OK()
}
