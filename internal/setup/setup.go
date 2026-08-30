// Package setup is the first-run wizard: the step state machine that decides
// which step is active, complete or skippable, and the service methods each step
// calls (DESIGN sections 1, 3.2 and 11.2).
//
// Two rules from §11.2 shape everything here. The wizard is RESUMABLE — "a
// browser refresh or a daemon restart mid-build does not restart the wizard" —
// so its state is rows in `wizard_steps` rather than anything a client holds. And
// every step is idempotent and re-enterable from /settings later, so there is no
// "finish" that closes a door: `complete` marks the wizard done and nothing else.
//
// The gate is server-side. A client that posts to a step whose prerequisites are
// unfinished is refused with `409 wizard_step_locked`, because the alternative —
// trusting the client's idea of where it is — is exactly what a resumable wizard
// cannot do.
package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Repo is the persistence this package needs (§1, invariant 1).
type Repo interface {
	WizardSteps(ctx context.Context, tx store.Tx) ([]model.WizardStepRow, error)
	PutWizardStep(ctx context.Context, tx store.Tx, v model.WizardStepRow) error

	AdminAccountExists(ctx context.Context, tx store.Tx) (bool, error)
	SetupClaim(ctx context.Context, tx store.Tx) (model.SetupClaim, error)

	AppendEvent(ctx context.Context, tx store.Tx, e model.Event) error

	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	Write(ctx context.Context, fn func(context.Context, store.Tx) error) error
}

// Claimer is the one thing the wizard needs from internal/auth: §2.2a step 5's
// burn, which creates `admin_account`, stamps the claim and mints the session,
// all in one transaction. The interface is declared here because the consumer
// owns it (§1).
type Claimer interface {
	Claim(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error)
}

// Config constructs a Service.
type Config struct {
	// Repo is required.
	Repo Repo
	// Claimer is required for the password step; the rest of the wizard works
	// without it.
	Claimer Claimer
	// Now supplies every instant. Nil uses time.Now.
	Now func() time.Time
}

// Service is the wizard's state machine and the endpoints of §3.2.
type Service struct {
	repo    Repo
	claimer Claimer
	now     func() time.Time
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Repo == nil {
		return nil, errors.New("setup: a repository is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{repo: cfg.Repo, claimer: cfg.Claimer, now: cfg.Now}, nil
}

// Order is §11.2's table, in order, with `done` as the terminal row of §2.11's
// CHECK constraint. It is the ONE statement of the wizard's shape: the gate, the
// state view and the completion check all read it, so a step added to the schema
// and forgotten here would fail the test that compares the two.
func Order() []model.WizardStep {
	return []model.WizardStep{
		model.StepPassword,
		model.StepToolchain,
		model.StepLlamacpp,
		model.StepHF,
		model.StepModels,
		model.StepInstance,
		model.StepDone,
	}
}

// Skippable is §11.2's "skippable" column:
//
//   - password  — no. Nothing works without an admin account.
//   - toolchain — yes, "to CPU-only": a host with no compiler can still run a
//     prebuilt llama.cpp, and the step's own copy says so.
//   - llamacpp  — no. There is nothing to run without it.
//   - hf        — yes. The token is optional; public repositories need none.
//   - models    — yes, "when the scan found GGUFs", and yes in general: a user
//     who wants to add models later is not blocked from finishing.
//   - instance  — yes. An instance can be created from the dashboard afterwards.
//
// `done` is not skippable because it is not a step a human does; Complete writes
// it.
func Skippable(step model.WizardStep) bool {
	switch step {
	case model.StepToolchain, model.StepHF, model.StepModels, model.StepInstance:
		return true
	default:
		return false
	}
}

// finished reports whether a step no longer blocks the one after it.
func finished(state model.WizardStepState) bool {
	return state == model.WizardComplete || state == model.WizardSkipped
}

// State is `GET /api/v1/setup/state` (§3.2). It is public, so it says only what
// an unclaimed host must reveal to route a browser: whether it has been claimed,
// whether this caller needs a token, and where the wizard stands.
//
// loopback is D38's rule as the caller experiences it, and it is passed in rather
// than derived because only the HTTP layer knows the request's address.
func (s *Service) State(ctx context.Context, loopback bool) (model.SetupState, error) {
	var (
		rows    []model.WizardStepRow
		claimed bool
	)
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if rows, err = s.repo.WizardSteps(ctx, tx); err != nil {
			return err
		}
		exists, err := s.repo.AdminAccountExists(ctx, tx)
		if err != nil {
			return err
		}
		claimed = exists
		if !claimed {
			// An account is the authoritative answer, but a stamped claim on a
			// database whose account row was removed by hand should still read
			// as claimed rather than inviting a second claim.
			c, err := s.repo.SetupClaim(ctx, tx)
			switch {
			case err == nil:
				claimed = c.Claimed()
			case errors.Is(err, store.ErrNotFound):
			default:
				return err
			}
		}
		return nil
	}); err != nil {
		return model.SetupState{}, err
	}

	return model.SetupState{
		Claimed:       claimed,
		Complete:      complete(rows),
		TokenRequired: !loopback && !claimed,
		Steps:         view(rows),
	}, nil
}

// stateOf reads one step's stored state; a step with no row is pending, which is
// what makes a fresh database a valid starting point with no seeding.
func stateOf(rows []model.WizardStepRow, step model.WizardStep) model.WizardStepState {
	for _, r := range rows {
		if r.Step == step {
			return r.State
		}
	}
	return model.WizardPending
}

// view renders §11.2's table for these rows, marking the first unfinished step
// active and every step behind an unfinished non-skippable one blocked.
func view(rows []model.WizardStepRow) []model.WizardStepView {
	out := make([]model.WizardStepView, 0, len(Order()))
	blocked := false
	activeSeen := false

	for _, step := range Order() {
		st := stateOf(rows, step)
		v := model.WizardStepView{
			Step:      step,
			State:     st,
			Skippable: Skippable(step),
			Blocked:   blocked,
		}
		// The first unfinished, unblocked step is the one to open. `active` is
		// derived rather than stored so that a step marked active by a boot that
		// crashed cannot leave the wizard pointing at the wrong place.
		if !finished(st) && !blocked && !activeSeen && step != model.StepDone {
			v.State = model.WizardActive
			activeSeen = true
		}
		if !finished(st) && !Skippable(step) {
			blocked = true
		}
		out = append(out, v)
	}
	return out
}

// complete reports whether the wizard has been finished — the `done` row being
// complete, which `POST /api/v1/setup/complete` writes.
func complete(rows []model.WizardStepRow) bool {
	return stateOf(rows, model.StepDone) == model.WizardComplete
}

// Claimed reports whether `admin_account` exists. `GET /api/v1/meta` reads it,
// and it is the honest answer to "has this host been claimed" — distinct from
// Complete, because a wizard interrupted after the password step is a claimed
// host with an unfinished wizard, which §11.2 requires be resumable.
func (s *Service) Claimed(ctx context.Context) (bool, error) {
	var claimed bool
	err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		claimed, err = s.repo.AdminAccountExists(ctx, tx)
		return err
	})
	return claimed, err
}

// Complete reports whether the wizard has been finished.
func (s *Service) Complete(ctx context.Context) (bool, error) {
	var done bool
	err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		rows, err := s.repo.WizardSteps(ctx, tx)
		if err != nil {
			return err
		}
		done = complete(rows)
		return nil
	})
	return done, err
}

// ClaimPassword is `POST /api/v1/setup/password` (§3.2): the wizard's first step.
// It delegates the claim itself to internal/auth — one transaction creating
// `admin_account`, stamping `setup_claim` and minting the session — and then
// marks the step complete.
//
// The two writes are deliberately not one transaction. The claim is the fact that
// matters and it must not be held open across the wizard's bookkeeping; a crash
// between them leaves a claimed host whose `password` step is still pending,
// which the state machine already handles: the step is idempotent, and MarkStep
// below closes it on the next call.
func (s *Service) ClaimPassword(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error) {
	if s.claimer == nil {
		return model.SessionCredential{}, errors.New("setup: no claimer is wired")
	}
	cred, err := s.claimer.Claim(ctx, password, ip, userAgent)
	if err != nil {
		return model.SessionCredential{}, err
	}
	if err := s.mark(ctx, model.StepPassword, model.WizardComplete); err != nil {
		return model.SessionCredential{}, err
	}
	return cred, nil
}

// MarkStep records that a step finished. It is what every later step's endpoint
// calls once its own work committed — the toolchain probe, the llama.cpp install,
// the token, the scan, the first instance — so the wizard's position is a
// consequence of work that actually happened rather than of a click.
func (s *Service) MarkStep(ctx context.Context, step model.WizardStep) error {
	if err := s.check(ctx, step, false); err != nil {
		return err
	}
	return s.mark(ctx, step, model.WizardComplete)
}

// Skip is `POST /api/v1/setup/skip` (§3.2). It refuses a step §11.2 marks
// non-skippable, and it refuses one that is blocked by an unfinished step in
// front of it.
func (s *Service) Skip(ctx context.Context, step model.WizardStep) error {
	if err := s.check(ctx, step, true); err != nil {
		return err
	}
	return s.mark(ctx, step, model.WizardSkipped)
}

// Finish is `POST /api/v1/setup/complete` (§3.2): mark `done` complete, which is
// what `GET /api/v1/meta`'s `setup_complete` reports and what sends the SPA to
// the dashboard.
//
// It refuses while a non-skippable step is unfinished. That is the same gate
// every other step passes through, applied to the last one: a wizard that could
// be "completed" without llama.cpp installed would send the user to a dashboard
// that cannot start anything.
func (s *Service) Finish(ctx context.Context) error {
	if err := s.check(ctx, model.StepDone, false); err != nil {
		return err
	}
	if err := s.mark(ctx, model.StepDone, model.WizardComplete); err != nil {
		return err
	}
	return nil
}

// check is the gate. A step may be entered, completed or skipped only when every
// step in front of it is finished — complete or skipped — and only skippable
// steps may be skipped.
func (s *Service) check(ctx context.Context, step model.WizardStep, skipping bool) error {
	if !step.Valid() {
		return model.Error{
			Code:    model.CodeWizardStepUnknown,
			Message: fmt.Sprintf("%q is not a wizard step", step),
			Details: map[string]any{"steps": stepStrings()},
		}
	}
	if skipping && !Skippable(step) {
		return model.Error{
			Code:    model.CodeWizardStepLocked,
			Message: fmt.Sprintf("the %s step cannot be skipped", step),
			Details: map[string]any{"step": string(step), "skippable": false},
		}
	}

	var rows []model.WizardStepRow
	if err := s.repo.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		rows, err = s.repo.WizardSteps(ctx, tx)
		return err
	}); err != nil {
		return err
	}

	for _, prior := range Order() {
		if prior == step {
			return nil
		}
		if finished(stateOf(rows, prior)) {
			continue
		}
		if Skippable(prior) {
			// An unfinished skippable step in front is not a blocker: moving
			// past it IS how §11.2's "skippable" works, and marking it skipped
			// on the way keeps the stored table honest about what happened.
			continue
		}
		return model.Error{
			Code: model.CodeWizardStepLocked,
			Message: fmt.Sprintf("the %s step cannot be entered until %s is finished",
				step, prior),
			Details: map[string]any{"step": string(step), "blocked_by": string(prior)},
		}
	}
	return nil
}

// mark upserts one step's state and records the move in `events`, so the wizard's
// history is in the same audit log as everything else the daemon did.
func (s *Service) mark(ctx context.Context, step model.WizardStep, state model.WizardStepState) error {
	now := s.now()
	nowMS := now.UnixMilli()

	return s.repo.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := s.repo.PutWizardStep(ctx, tx, model.WizardStepRow{
			Step:      step,
			State:     state,
			UpdatedAt: nowMS,
		}); err != nil {
			return err
		}
		to := string(state)
		subjectType, subjectID := "wizard", string(step)
		return s.repo.AppendEvent(ctx, tx, model.Event{
			ID:          store.NewID(now),
			At:          nowMS,
			Level:       model.LevelInfo,
			Category:    model.CategorySystem,
			SubjectType: &subjectType,
			SubjectID:   &subjectID,
			Action:      "wizard_step",
			ToState:     &to,
			Actor:       model.ActorWizard,
			Message:     fmt.Sprintf("wizard step %s is %s", step, state),
		})
	})
}

func stepStrings() []string {
	out := make([]string, 0, len(Order()))
	for _, s := range Order() {
		out = append(out, string(s))
	}
	return out
}
