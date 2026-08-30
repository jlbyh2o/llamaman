package model

// Self-update, fit calibration and the wizard (DESIGN section 2.11).

// SelfUpdateState is `self_updates.state` (§2.11).
//
// Eight states, and every one of them is written by a named step of §12 that can
// reach the database: the daemon writes the first five, the confirmation gate of
// §12.3 writes `succeeded`/`failed`, and POST /jobs/{id}/cancel writes
// `canceled`. There is no state a privileged actor would have to report through
// a file, because no privileged actor in this design has anything to report
// (D87).
//
// The cancel cut-off is a rule about this enum and not about the endpoint: a
// cancel is accepted only while the row is `planned`, `downloading` or
// `verifying`. At or after the `staged` commit the marker is on disk and the
// swap belongs to systemd, so the answer is 409 selfupdate_not_cancelable
// (D96, §12.1).
//
// The §2.3a pairing with the job row, which the boot triage depends on:
//
//	jobs.state          self_updates.state
//	------------------- ------------------------------------------------------
//	queued              planned
//	leased|running      downloading|verifying|staged|swapping
//	interrupted         ANY non-terminal state — the confirmation gate's input.
//	                    A restart mid-download leaves exactly that pairing, and
//	                    so does a database restored from db-backups/, because the
//	                    D14 snapshot is taken DURING `verifying` (§12.1 step 4).
//	                    Neither is an anomaly to assert against.
//	succeeded           succeeded
//	failed              failed
//	canceled            canceled
type SelfUpdateState string

const (
	UpdatePlanned     SelfUpdateState = "planned"
	UpdateDownloading SelfUpdateState = "downloading"
	UpdateVerifying   SelfUpdateState = "verifying"
	UpdateStaged      SelfUpdateState = "staged"
	UpdateSwapping    SelfUpdateState = "swapping"
	UpdateSucceeded   SelfUpdateState = "succeeded"
	UpdateFailed      SelfUpdateState = "failed"
	UpdateCanceled    SelfUpdateState = "canceled"
)

// SelfUpdateStateValues lists the members of the `self_updates.state` CHECK
// constraint, in order.
func SelfUpdateStateValues() []SelfUpdateState {
	return []SelfUpdateState{
		UpdatePlanned, UpdateDownloading, UpdateVerifying, UpdateStaged,
		UpdateSwapping, UpdateSucceeded, UpdateFailed, UpdateCanceled,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s SelfUpdateState) Valid() bool { return valid(s, SelfUpdateStateValues()) }

// IsTerminal reports whether the update is over.
func (s SelfUpdateState) IsTerminal() bool {
	return s == UpdateSucceeded || s == UpdateFailed || s == UpdateCanceled
}

// Cancelable reports whether POST /jobs/{id}/cancel may still be accepted for a
// row in this state (D96): only before the `staged` commit.
func (s SelfUpdateState) Cancelable() bool {
	return s == UpdatePlanned || s == UpdateDownloading || s == UpdateVerifying
}

// FitObservationSource is `fit_observations.source` (§2.11, §8.6): where a
// learned correction for the fit calculator came from.
type FitObservationSource string

const (
	FitFromInstanceStart FitObservationSource = "instance_start"
	FitFromBench         FitObservationSource = "bench"
	FitFromFitFlag       FitObservationSource = "fit_flag"
)

// FitObservationSourceValues lists the members of the `fit_observations.source`
// CHECK constraint, in order.
func FitObservationSourceValues() []FitObservationSource {
	return []FitObservationSource{FitFromInstanceStart, FitFromBench, FitFromFitFlag}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s FitObservationSource) Valid() bool { return valid(s, FitObservationSourceValues()) }

// WizardStep is `wizard_steps.step` (§2.11, §11.2). The steps are rows rather
// than a counter so the wizard is resumable: a browser refresh or a daemon
// restart mid-build does not restart it.
type WizardStep string

const (
	StepPassword  WizardStep = "password"
	StepToolchain WizardStep = "toolchain"
	StepLlamacpp  WizardStep = "llamacpp"
	StepHF        WizardStep = "hf"
	StepModels    WizardStep = "models"
	StepInstance  WizardStep = "instance"
	StepDone      WizardStep = "done"
)

// WizardStepValues lists the members of the `wizard_steps.step` CHECK
// constraint, in order — which is also the order the wizard runs them.
func WizardStepValues() []WizardStep {
	return []WizardStep{
		StepPassword, StepToolchain, StepLlamacpp, StepHF, StepModels,
		StepInstance, StepDone,
	}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s WizardStep) Valid() bool { return valid(s, WizardStepValues()) }

// WizardStepState is `wizard_steps.state` (§2.11). Every step is idempotent and
// re-enterable from /settings later; `skipped` is a real outcome for the four
// steps §11.2 marks skippable, and is not the same as `pending`.
type WizardStepState string

const (
	WizardPending  WizardStepState = "pending"
	WizardActive   WizardStepState = "active"
	WizardSkipped  WizardStepState = "skipped"
	WizardComplete WizardStepState = "complete"
)

// WizardStepStateValues lists the members of the `wizard_steps.state` CHECK
// constraint, in order.
func WizardStepStateValues() []WizardStepState {
	return []WizardStepState{WizardPending, WizardActive, WizardSkipped, WizardComplete}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s WizardStepState) Valid() bool { return valid(s, WizardStepStateValues()) }
