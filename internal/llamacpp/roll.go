package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The canary roll's mechanism (DESIGN section 6.6 step 5), separated from its
// policy so the policy can be tested against a fake that fails on demand — which
// is the only way to exercise the revert path without a fleet.

// RollTarget is one instance a roll restarts.
type RollTarget struct {
	ID   string
	Name string
	// Port is the internal port `/health` is polled on.
	Port int
	// Timeout is `instances.start_timeout_sec` for this instance: how long it
	// has to answer `/health` before the restart counts as a failure.
	Timeout time.Duration
}

// Roller restarts instances one at a time and reports whether each came back.
//
// It is an interface rather than a concrete type because §6.6 step 5's ORDER —
// canary first, gate on `/health`, stop on failure — is the part this package
// owns, and the restarting itself is the supervisor's.
type Roller interface {
	// Targets returns the instances to restart, canary first and then creation
	// order. An empty list is an ordinary answer: nothing is running.
	Targets(ctx context.Context, canaryID string) ([]RollTarget, error)
	// Restart restarts one instance and returns nil only once it has answered
	// `/health` within its own start timeout.
	Restart(ctx context.Context, target RollTarget) error
}

// Fleet is the desired-state API §5.8 splits from the acting: `POST
// /instances/{id}/start`, `/stop` and `/restart` all land here, and it never
// calls systemd. *instances.Service satisfies it.
type Fleet interface {
	SetDesiredState(ctx context.Context, id string, desired model.DesiredState,
		trigger model.PendingTrigger) (instances.View, error)
}

// Reconciler is the supervisor's own exported pass: read `(desired, actual)`,
// take at most one corrective action, record what was observed.
// *supervisor.Supervisor satisfies it.
type Reconciler interface {
	Reconcile(ctx context.Context) error
}

// HealthProbe answers §6.6's gate: did the instance come back? The production
// implementation is the supervisor's own HTTP prober, so the roll and the
// reconcile loop agree about what "healthy" means.
type HealthProbe interface {
	Healthy(ctx context.Context, port int) bool
}

// SupervisorRoller is the production Roller. It writes the DESIRED axis through
// the instances service and lets the supervisor do the acting, which is the same
// split every other restart in this system uses — and the reason an instance
// that dies mid-roll is picked up by the ordinary reconcile loop rather than
// being lost to a roll that has moved on.
type SupervisorRoller struct {
	// Store lists the instances and reads back what was observed.
	Store Store
	// Fleet writes `desired_state` and stamps the trigger.
	Fleet Fleet
	// Supervisor is asked to take its one corrective action now rather than at
	// its next tick, so a roll proceeds at the pace of the restarts and not of
	// the poll interval.
	Supervisor Reconciler
	// Health is the gate. Nil makes every restart succeed as soon as the unit is
	// observed active, which is honest only in a test.
	Health HealthProbe
	// Settings reads `instances.start_timeout_sec`, which is the gate's
	// deadline. Nil uses DefaultTimeout.
	Settings Settings

	// Poll is how often the roll re-reads the fleet while waiting. Zero means
	// DefaultRollPoll.
	Poll time.Duration
	// DefaultTimeout applies to an instance whose own `start_timeout_sec` is
	// unset. Zero means DefaultStartTimeout.
	DefaultTimeout time.Duration

	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Defaults for the roll's two waits.
const (
	// DefaultRollPoll is how often the roll looks again. It is the supervisor's
	// own fast-poll interval, because a roll is exactly the moment §5.8 calls
	// "the one moment a user watches the state field".
	DefaultRollPoll = time.Second
	// DefaultStartTimeout is the fallback gate for an instance with no
	// `start_timeout_sec` of its own.
	DefaultStartTimeout = 120 * time.Second
	// stopGrace bounds the wait for an instance to actually go down before the
	// roll starts it again. It is deliberately shorter than the start gate: a
	// stop that has not been observed within it is reported as the restart
	// failure it is.
	stopGrace = 60 * time.Second
)

// Targets implements Roller: the running instances, canary first.
func (r *SupervisorRoller) Targets(ctx context.Context, canaryID string) ([]RollTarget, error) {
	var rows []model.InstanceView
	if err := r.Store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = r.Store.InstanceViews(ctx, tx, store.InstanceFilter{})
		return err
	}); err != nil {
		return nil, err
	}

	timeout := r.startTimeout(ctx)
	var out []RollTarget
	for _, row := range rows {
		if row.Deleted() || !isServing(row.Status.State) {
			continue
		}
		out = append(out, RollTarget{
			ID:      row.ID,
			Name:    row.Name,
			Port:    row.InternalPort,
			Timeout: timeout,
		})
	}

	// Creation order. Ids are ULIDs, so sorting by id is sorting by creation
	// with a unique tiebreak — the same ordering every listing in this system
	// uses.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	if canaryID != "" {
		for i, t := range out {
			if t.ID == canaryID {
				out[0], out[i] = out[i], out[0]
				break
			}
		}
	}
	return out, nil
}

// isServing reports whether an instance is one the roll has to move. A stopped
// or failed instance is not restarted BY the roll: it is not serving on the old
// build, so migrating it is not this operation's business, and starting it would
// be a change the user did not ask for.
func isServing(s model.InstanceState) bool {
	switch s {
	case model.InstanceReady, model.InstanceDegraded, model.InstanceLoading,
		model.InstanceStarting:
		return true
	}
	return false
}

// startTimeout is `instances.start_timeout_sec` — §6.6 step 5's gate. It is a
// host-wide setting rather than a per-instance column, so it is read once per
// roll rather than once per instance.
func (r *SupervisorRoller) startTimeout(ctx context.Context) time.Duration {
	if r.Settings != nil {
		if n, err := r.Settings.GetInt(ctx, "instances.start_timeout_sec"); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if r.DefaultTimeout > 0 {
		return r.DefaultTimeout
	}
	return DefaultStartTimeout
}

// Restart implements Roller. It is a stop and a start rather than a systemd
// `RestartUnit` because that is what the desired-state split makes available to
// a caller that is not the supervisor — and it is also what makes the ledger
// read correctly: the start stamps `pending_trigger='rolling'`, so
// `instance_starts` names the roll (§5.8).
func (r *SupervisorRoller) Restart(ctx context.Context, target RollTarget) error {
	if _, err := r.Fleet.SetDesiredState(ctx, target.ID, model.DesiredStopped, ""); err != nil {
		return fmt.Errorf("stop %s: %w", target.Name, err)
	}
	if err := r.settle(ctx, target, stopGrace, func(row model.InstanceView) bool {
		return !isServing(row.Status.State) && row.Status.MainPID == nil
	}); err != nil {
		return fmt.Errorf("stop %s: %w", target.Name, err)
	}

	if _, err := r.Fleet.SetDesiredState(ctx, target.ID, model.DesiredRunning,
		model.TriggerRolling); err != nil {
		return fmt.Errorf("start %s: %w", target.Name, err)
	}
	return r.awaitHealthy(ctx, target)
}

// settle drives the supervisor until a condition over the observed row holds.
func (r *SupervisorRoller) settle(ctx context.Context, target RollTarget, within time.Duration,
	done func(model.InstanceView) bool) error {

	deadline := r.now().Add(within)
	for {
		if err := r.Supervisor.Reconcile(ctx); err != nil {
			return err
		}
		row, err := r.view(ctx, target.ID)
		if err != nil {
			return err
		}
		if done(row) {
			return nil
		}
		if r.now().After(deadline) {
			return fmt.Errorf("it was still %s after %s", row.Status.State, within)
		}
		if err := r.sleep(ctx); err != nil {
			return err
		}
	}
}

// awaitHealthy is the gate: the instance must answer `/health` within its own
// `start_timeout_sec`.
func (r *SupervisorRoller) awaitHealthy(ctx context.Context, target RollTarget) error {
	deadline := r.now().Add(target.Timeout)
	for {
		if err := r.Supervisor.Reconcile(ctx); err != nil {
			return err
		}
		row, err := r.view(ctx, target.ID)
		if err != nil {
			return err
		}
		switch {
		case row.Status.State == model.InstanceReady && r.healthy(ctx, target):
			return nil
		case row.Status.State == model.InstanceFailed ||
			row.Status.State == model.InstanceCrashLooping:
			// A failed start is a failure now rather than at the deadline: the
			// canary exists to give a fast answer, and waiting two minutes for
			// one that is already known is the opposite of that.
			return fmt.Errorf("%s is %s", target.Name, row.Status.State)
		case r.now().After(deadline):
			return fmt.Errorf("%s did not answer /health within %s", target.Name, target.Timeout)
		}
		if err := r.sleep(ctx); err != nil {
			return err
		}
	}
}

func (r *SupervisorRoller) healthy(ctx context.Context, target RollTarget) bool {
	if r.Health == nil {
		return true
	}
	return r.Health.Healthy(ctx, target.Port)
}

func (r *SupervisorRoller) view(ctx context.Context, id string) (model.InstanceView, error) {
	var row model.InstanceView
	err := r.Store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := r.Store.InstanceViews(ctx, tx, store.InstanceFilter{IDs: []string{id}})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return store.ErrNotFound
		}
		row = rows[0]
		return nil
	})
	return row, err
}

func (r *SupervisorRoller) sleep(ctx context.Context) error {
	poll := r.Poll
	if poll <= 0 {
		poll = DefaultRollPoll
	}
	t := time.NewTimer(poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *SupervisorRoller) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// ErrNoRoller is returned by a Roller that has nothing to drive. It exists so a
// caller can tell "no instances were running" from "this daemon cannot restart
// anything", which read the same from an empty target list.
var ErrNoRoller = errors.New("llamacpp: this daemon has no way to restart instances")
