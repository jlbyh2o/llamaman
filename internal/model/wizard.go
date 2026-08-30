package model

// The wizard's rows and views (DESIGN sections 2.11 and 11.2).
//
// The step ENUMS live beside the rest of §2.11's enums in selfupdate.go; what is
// here is the row itself and the two views internal/setup hands to internal/api.
//
// §11.2's table is the whole state machine: six steps plus `done`, each
// idempotent and re-enterable from /settings later, and "a browser refresh or a
// daemon restart mid-build does not restart the wizard" — which is why the state
// is a table and not a field in a client-side store.

// WizardStepRow is one `wizard_steps` row (§2.11).
type WizardStepRow struct {
	Step      WizardStep
	State     WizardStepState
	DataJSON  *string
	UpdatedAt int64
}

// WizardStepView is one entry of `GET /api/v1/setup/state`'s `steps` array
// (§3.2), plus the two facts the SPA needs in order to render the step list
// without a second, hand-maintained copy of §11.2's table in TypeScript.
type WizardStepView struct {
	Step  WizardStep
	State WizardStepState
	// Skippable is §11.2's "skippable" column: `toolchain` (to CPU-only), `hf`,
	// `models` and `instance` may be skipped; `password` and `llamacpp` may not.
	Skippable bool
	// Blocked reports that this step cannot be entered yet because an earlier
	// non-skippable step is unfinished. The gate is enforced server-side —
	// a client that posts to a blocked step is refused — and this field only
	// lets the UI say so before the click.
	Blocked bool
}

// SetupState is the body behind `GET /api/v1/setup/state` (§3.2). It is public,
// like `GET /api/v1/meta`, and for the same reason: the SPA has to decide
// between the wizard and the login form before it has any credential at all.
type SetupState struct {
	// Claimed is `setup_claim.claimed_at IS NOT NULL` — the one-time token has
	// been burned and an admin account exists.
	Claimed bool
	// Complete is the `done` step being complete: the wizard has been finished.
	// A host can be claimed without being complete — that is a wizard
	// interrupted after the password step, which §11.2 requires be resumable.
	Complete bool
	// TokenRequired is D38's rule as the caller experiences it: false for a
	// loopback caller, who may claim the daemon with no token at all, and true
	// for every other origin, which must present `X-Setup-Token`.
	TokenRequired bool
	// Steps is §11.2's table for this install, in order.
	Steps []WizardStepView
}

// ActiveStep returns the step a resuming browser should open, and whether there
// is one. It is the first step that is neither complete nor skipped, which makes
// "resume where you left off" a property of the stored rows rather than of
// anything the client remembers.
func (s SetupState) ActiveStep() (WizardStep, bool) {
	for _, v := range s.Steps {
		if v.Step == StepDone {
			continue
		}
		if v.State != WizardComplete && v.State != WizardSkipped {
			return v.Step, true
		}
	}
	return "", false
}
