package app

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// DESIGN section 12.1 step 7 has TWO branches, and only one of them was here.
//
// In system scope the daemon summons `llamaman-selfupdate.service` and then
// WAITS to be SIGTERMed by that unit's own `systemctl restart` (D79). In the D2
// user-scope topology "there is no oneshot: the daemon performs section 12.2's
// swap sequence itself and then exits normally" (section 12.1 step 7, section
// 5.2a item 2) — `install-units` writes no such unit there and
// `selfupdate-apply` refuses to run in that scope, so summoning one summons
// nothing.
//
// Without the branch, a `--user-units` host passed the guard (clause 3 is
// deliberately inapplicable in user scope), downloaded, verified, snapshotted,
// wrote `update/pending`, committed `swapping`, drained its listeners, started a
// unit that does not exist, waited out section 9.4 step 7's 120 s failsafe with
// no management UI, came back on the same binary and raised F24 — every time,
// forever. This test is that branch.

// recordingController is a systemd.Controller that records the verbs issued to
// it. Faking the interface in a test is not a breach of D49's second invariant:
// the vocabulary still lives in internal/systemd, and what is asserted here is
// which verb the COMPOSITION ROOT chooses, which is exactly this package's job.
type recordingController struct {
	mu    sync.Mutex
	calls []string
}

func (c *recordingController) record(verb, unit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, verb+" "+unit)
}

func (c *recordingController) saw(verb, unit string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, got := range c.calls {
		if got == verb+" "+unit {
			return true
		}
	}
	return false
}

func (c *recordingController) Start(_ context.Context, unit string) (systemd.JobResult, error) {
	c.record("Start", unit)
	return systemd.JobResult(""), nil
}

func (c *recordingController) Stop(_ context.Context, unit string) (systemd.JobResult, error) {
	c.record("Stop", unit)
	return systemd.JobResult(""), nil
}

func (c *recordingController) Restart(_ context.Context, unit string) (systemd.JobResult, error) {
	c.record("Restart", unit)
	return systemd.JobResult(""), nil
}

func (c *recordingController) StartNoWait(_ context.Context, unit string) (systemd.JobPath, error) {
	c.record("StartNoWait", unit)
	return "", nil
}

func (c *recordingController) RestartNoWait(_ context.Context, unit string) (systemd.JobPath, error) {
	c.record("RestartNoWait", unit)
	return "", nil
}

func (c *recordingController) Enable(_ context.Context, units []string) error {
	for _, u := range units {
		c.record("Enable", u)
	}
	return nil
}

func (c *recordingController) Disable(_ context.Context, units []string) error {
	for _, u := range units {
		c.record("Disable", u)
	}
	return nil
}

func (c *recordingController) ResetFailed(_ context.Context, unit string) error {
	c.record("ResetFailed", unit)
	return nil
}

func (c *recordingController) Props(_ context.Context, unit string) (systemd.UnitProps, error) {
	c.record("Props", unit)
	return systemd.UnitProps{ActiveState: "inactive"}, nil
}

func (c *recordingController) SubscribeSubState(ctx context.Context, _ string) (
	<-chan systemd.SubStateEvent, error) {

	ch := make(chan systemd.SubStateEvent)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// swapDaemon is the smallest daemon runSwap needs: a logger, a notifier, a
// server to drain (there is nothing in flight), a scope and a controller.
//
// The state directory holds no staged release, so the user-scope branch's
// selfupdate.Apply refuses at section 12.2 step 0's marker read — writing
// nothing, deleting nothing, stopping nothing. That is the right shape for this
// test: what is under assertion is WHICH BRANCH runSwap takes, not what a
// verified swap does, which internal/selfupdate covers with a signed fixture.
func swapDaemon(t *testing.T, scope model.SystemdScope, ctl systemd.Controller) *daemon {
	t.Helper()
	return &daemon{
		log:      quiet(),
		scope:    scope,
		stateDir: t.TempDir(),
		server:   &http.Server{},
		opts:     Options{Notifier: nopNotifier{log: slog.Default()}},
		systemd:  SystemdEnv{Control: ctl},
	}
}

// TestRunSwapUserScopePerformsItInProcess: no oneshot is summoned, and the call
// returns rather than blocking on a SIGTERM that will never come.
func TestRunSwapUserScopePerformsItInProcess(t *testing.T) {
	t.Parallel()

	ctl := &recordingController{}
	d := swapDaemon(t, model.ScopeUser, ctl)

	errc := make(chan error, 1)
	errc <- nil

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.runSwap(context.Background(), errc)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runSwap blocked in user scope: it is waiting for a restart from a unit " +
			"install-units never wrote, which burns section 9.4 step 7's 120 s failsafe " +
			"with no management UI on every --user-units host")
	}

	if ctl.saw("StartNoWait", selfupdate.SwapUnit) {
		t.Errorf("user scope summoned %s, which install-units does not install there "+
			"and selfupdate-apply refuses to run (section 5.2a item 2)", selfupdate.SwapUnit)
	}
	// D93 is topology-independent: the start-limit counter is cleared in both
	// scopes, against the user's own manager here (section 5.2a).
	if !ctl.saw("ResetFailed", systemd.UnitDaemon) {
		t.Errorf("the start-limit counter was not cleared before the swap (D93)")
	}
	// Section 19's first preservation property: no step in this protocol stops a
	// unit.
	for _, verb := range []string{"Stop", "Restart", "RestartNoWait"} {
		if ctl.saw(verb, systemd.UnitDaemon) {
			t.Errorf("the user-scope swap issued %s on %s; no step in this protocol "+
				"stops or restarts a unit on the daemon's behalf", verb, systemd.UnitDaemon)
		}
	}
}

// TestRunSwapSystemScopeSummonsTheOneshot is the other branch, unchanged: the
// oneshot is started without waiting, and the daemon then blocks until its own
// `systemctl restart` arrives (D79) — here, until the context is canceled.
func TestRunSwapSystemScopeSummonsTheOneshot(t *testing.T) {
	t.Parallel()

	ctl := &recordingController{}
	d := swapDaemon(t, model.ScopeSystem, ctl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	errc <- nil

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.runSwap(ctx, errc)
	}()

	// The summons is issued from a detached goroutine, so poll for it rather
	// than assuming an ordering the design deliberately does not fix.
	deadline := time.Now().Add(30 * time.Second)
	for !ctl.saw("StartNoWait", selfupdate.SwapUnit) {
		if time.Now().After(deadline) {
			t.Fatalf("system scope never summoned %s", selfupdate.SwapUnit)
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-done:
		t.Fatal("runSwap returned without waiting: Restart=always would bring the OLD " +
			"binary back while selfupdate-apply is still verifying (D79)")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runSwap did not return after the restart signal")
	}
}
