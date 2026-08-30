package setup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

func newTestService(t *testing.T, claimer Claimer) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if f, ok := claimer.(*fakeClaimer); ok {
		f.store = st
	}

	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	svc, err := New(Config{Repo: st, Claimer: claimer, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatalf("setup.New: %v", err)
	}
	return svc, st
}

// fakeClaimer stands in for internal/auth: the wizard's password step delegates
// the claim, and this package's own rules are what is under test here.
//
// It still writes `admin_account`, because that row is what State reads as
// "claimed" — a claimer that returned a credential without creating the account
// would be a stub that agrees with nothing the wizard then reports.
type fakeClaimer struct {
	store *store.Store
	calls int
	err   error
}

func (f *fakeClaimer) Claim(ctx context.Context, _, ip, _ string) (model.SessionCredential, error) {
	f.calls++
	if f.err != nil {
		return model.SessionCredential{}, f.err
	}
	if f.store != nil {
		at := int64(1756468800000)
		err := f.store.Write(ctx, func(ctx context.Context, tx store.Tx) error {
			created, err := f.store.CreateAdminAccount(ctx, tx, model.AdminAccount{
				PasswordHash: "$argon2id$fake", PasswordSetAt: at, UpdatedAt: at,
			})
			if err != nil {
				return err
			}
			if !created {
				return errors.New("already claimed")
			}
			_, err = f.store.ClaimSetup(ctx, tx, at, ip)
			return err
		})
		if err != nil {
			return model.SessionCredential{}, err
		}
	}
	return model.SessionCredential{SessionID: "01SESSION", SessionCookie: "01SESSION.secret"}, nil
}

// TestOrderMatchesTheSchema: §11.2's table and §2.11's CHECK constraint are two
// spellings of one list, and a step added to one and forgotten in the other would
// be a step the wizard can store but never show, or show but never store.
func TestOrderMatchesTheSchema(t *testing.T) {
	t.Parallel()

	got := Order()
	want := model.WizardStepValues()
	if len(got) != len(want) {
		t.Fatalf("Order() has %d steps, the schema closes the column over %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("step %d = %q, the schema says %q", i, got[i], want[i])
		}
	}
}

// TestSkippableMatchesTheDesign is §11.2's "skippable" column, verbatim.
func TestSkippableMatchesTheDesign(t *testing.T) {
	t.Parallel()

	cases := []struct {
		step model.WizardStep
		want bool
	}{
		{model.StepPassword, false},
		{model.StepToolchain, true},
		{model.StepLlamacpp, false},
		{model.StepHF, true},
		{model.StepModels, true},
		{model.StepInstance, true},
		{model.StepDone, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.step), func(t *testing.T) {
			t.Parallel()
			if got := Skippable(tc.step); got != tc.want {
				t.Fatalf("Skippable(%s) = %v, want %v", tc.step, got, tc.want)
			}
		})
	}
}

// TestFreshStateIsResumable: a database with no `wizard_steps` rows is a valid
// starting point — every step is pending, `password` is active, and everything
// behind it is blocked because `password` is not skippable.
func TestFreshStateIsResumable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _ := newTestService(t, &fakeClaimer{})
	st, err := svc.State(ctx, true)
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if st.Claimed || st.Complete {
		t.Fatalf("a fresh install reports claimed=%v complete=%v", st.Claimed, st.Complete)
	}
	if st.TokenRequired {
		t.Fatal("a loopback caller was told a setup token is required (D38)")
	}
	if len(st.Steps) != len(Order()) {
		t.Fatalf("%d steps in the view, want %d", len(st.Steps), len(Order()))
	}
	if st.Steps[0].State != model.WizardActive {
		t.Fatalf("the password step is %q, want active", st.Steps[0].State)
	}
	for _, v := range st.Steps[1:] {
		if !v.Blocked {
			t.Fatalf("step %s is not blocked while password is unfinished", v.Step)
		}
	}
	if active, ok := st.ActiveStep(); !ok || active != model.StepPassword {
		t.Fatalf("ActiveStep = %q/%v, want password", active, ok)
	}
}

// TestTokenRequiredFollowsLoopback is D38 as the caller experiences it.
func TestTokenRequiredFollowsLoopback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _ := newTestService(t, &fakeClaimer{})

	remote, err := svc.State(ctx, false)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !remote.TokenRequired {
		t.Fatal("a non-loopback caller was told no token is required on an unclaimed host")
	}

	if _, err := svc.ClaimPassword(ctx, "a good password", "127.0.0.1", ""); err != nil {
		t.Fatalf("ClaimPassword: %v", err)
	}
	after, err := svc.State(ctx, false)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if after.TokenRequired {
		t.Fatal("a claimed host still asks a remote caller for a setup token; the claim window is closed")
	}
}

// TestStepGating is the server-side gate: a step may be entered, completed or
// skipped only when every step in front of it is finished, and only a step §11.2
// marks skippable may be skipped.
func TestStepGating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// finish applies a sequence of moves and reports the error of the last one.
	type move struct {
		step model.WizardStep
		skip bool
	}

	cases := []struct {
		name     string
		moves    []move
		wantCode model.ErrorCode // empty means the last move must succeed
	}{
		{
			name:  "the password step first",
			moves: []move{{step: model.StepPassword}},
		},
		{
			name:     "llamacpp before password",
			moves:    []move{{step: model.StepLlamacpp}},
			wantCode: model.CodeWizardStepLocked,
		},
		{
			name:     "skipping password",
			moves:    []move{{step: model.StepPassword, skip: true}},
			wantCode: model.CodeWizardStepLocked,
		},
		{
			name:     "skipping llamacpp",
			moves:    []move{{step: model.StepPassword}, {step: model.StepLlamacpp, skip: true}},
			wantCode: model.CodeWizardStepLocked,
		},
		{
			name:  "skipping toolchain to CPU-only, then llamacpp",
			moves: []move{{step: model.StepPassword}, {step: model.StepToolchain, skip: true}, {step: model.StepLlamacpp}},
		},
		{
			name: "an unfinished skippable step does not block the one after it",
			moves: []move{
				{step: model.StepPassword},
				{step: model.StepLlamacpp},
				{step: model.StepInstance},
			},
		},
		{
			name:     "completing the wizard before llamacpp",
			moves:    []move{{step: model.StepPassword}, {step: model.StepDone}},
			wantCode: model.CodeWizardStepLocked,
		},
		{
			name: "completing the wizard once the required steps are finished",
			moves: []move{
				{step: model.StepPassword},
				{step: model.StepLlamacpp},
				{step: model.StepDone},
			},
		},
		{
			name:     "a step that does not exist",
			moves:    []move{{step: model.WizardStep("nonsense")}},
			wantCode: model.CodeWizardStepUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newTestService(t, &fakeClaimer{})

			var err error
			for i, m := range tc.moves {
				switch {
				case m.skip:
					err = svc.Skip(ctx, m.step)
				case m.step == model.StepDone:
					err = svc.Finish(ctx)
				default:
					err = svc.MarkStep(ctx, m.step)
				}
				if i < len(tc.moves)-1 && err != nil {
					t.Fatalf("setup move %d (%s) failed unexpectedly: %v", i, m.step, err)
				}
			}

			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("the last move failed: %v", err)
				}
				return
			}
			var me model.Error
			if !errors.As(err, &me) || me.Code != tc.wantCode {
				t.Fatalf("the last move = %v, want %s", err, tc.wantCode)
			}
		})
	}
}

// TestWizardIsResumableAcrossRestarts: the state lives in rows, so a new Service
// over the same database picks up exactly where the last one left off — which is
// §11.2's "a browser refresh or a daemon restart mid-build does not restart the
// wizard".
func TestWizardIsResumableAcrossRestarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, st := newTestService(t, &fakeClaimer{})
	if _, err := svc.ClaimPassword(ctx, "a good password", "127.0.0.1", ""); err != nil {
		t.Fatalf("ClaimPassword: %v", err)
	}
	if err := svc.Skip(ctx, model.StepToolchain); err != nil {
		t.Fatalf("Skip(toolchain): %v", err)
	}

	// A "restart": a second Service over the same rows.
	resumed, err := New(Config{Repo: st})
	if err != nil {
		t.Fatalf("setup.New: %v", err)
	}
	state, err := resumed.State(ctx, true)
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if !state.Claimed {
		t.Fatal("the resumed wizard does not see the claim")
	}
	if state.Complete {
		t.Fatal("an unfinished wizard reports complete")
	}
	active, ok := state.ActiveStep()
	if !ok || active != model.StepLlamacpp {
		t.Fatalf("ActiveStep = %q/%v, want llamacpp (password complete, toolchain skipped)", active, ok)
	}

	byStep := map[model.WizardStep]model.WizardStepState{}
	for _, v := range state.Steps {
		byStep[v.Step] = v.State
	}
	if byStep[model.StepPassword] != model.WizardComplete {
		t.Fatalf("password is %q, want complete", byStep[model.StepPassword])
	}
	if byStep[model.StepToolchain] != model.WizardSkipped {
		t.Fatalf("toolchain is %q, want skipped", byStep[model.StepToolchain])
	}
}

// TestCompleteFollowsTheDoneStep: `GET /api/v1/meta`'s `setup_complete` is the
// `done` row, and a claimed host with an unfinished wizard must not report it.
func TestCompleteFollowsTheDoneStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, _ := newTestService(t, &fakeClaimer{})
	if _, err := svc.ClaimPassword(ctx, "a good password", "127.0.0.1", ""); err != nil {
		t.Fatalf("ClaimPassword: %v", err)
	}

	claimed, err := svc.Claimed(ctx)
	if err != nil {
		t.Fatalf("Claimed: %v", err)
	}
	complete, err := svc.Complete(ctx)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !claimed || complete {
		t.Fatalf("claimed=%v complete=%v, want a claimed host with an unfinished wizard", claimed, complete)
	}

	if err := svc.MarkStep(ctx, model.StepLlamacpp); err != nil {
		t.Fatalf("MarkStep(llamacpp): %v", err)
	}
	if err := svc.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	complete, err = svc.Complete(ctx)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !complete {
		t.Fatal("the wizard does not report complete after Finish")
	}
}

// TestClaimPasswordIsIdempotentAboutItsStep: a crash between the claim's commit
// and the step write leaves a claimed host whose `password` step is pending, and
// re-entering the step must close it rather than fail.
func TestClaimPasswordMarksTheStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	claimer := &fakeClaimer{}
	svc, _ := newTestService(t, claimer)

	cred, err := svc.ClaimPassword(ctx, "a good password", "127.0.0.1", "browser")
	if err != nil {
		t.Fatalf("ClaimPassword: %v", err)
	}
	if cred.SessionID == "" {
		t.Fatal("ClaimPassword returned no session; the wizard's first step logs the browser in")
	}
	if claimer.calls != 1 {
		t.Fatalf("the claimer was called %d times, want once", claimer.calls)
	}

	// Re-marking the step is idempotent, which is what makes the crash window
	// harmless.
	if err := svc.MarkStep(ctx, model.StepPassword); err != nil {
		t.Fatalf("re-marking the password step: %v", err)
	}
}

// TestClaimPasswordPropagatesTheClaimError: the wizard must not mark its first
// step complete when the claim itself was refused — the loser of the claim race
// has not set a password.
func TestClaimPasswordPropagatesTheClaimError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	boom := errors.New("already claimed")
	svc, _ := newTestService(t, &fakeClaimer{err: boom})

	if _, err := svc.ClaimPassword(ctx, "a good password", "10.0.0.1", ""); !errors.Is(err, boom) {
		t.Fatalf("ClaimPassword = %v, want the claimer's error", err)
	}
	state, err := svc.State(ctx, true)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Steps[0].State == model.WizardComplete {
		t.Fatal("the password step was marked complete after a failed claim")
	}
}
