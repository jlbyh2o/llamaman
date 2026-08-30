package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// wizard_steps queries (DESIGN sections 2.11 and 11.2).
//
// The wizard is a table rather than client state for one reason §11.2 states
// outright: "a browser refresh or a daemon restart mid-build does not restart the
// wizard". Every method here is therefore an upsert or a read — there is no
// "reset the wizard" statement, because re-entering a step is what §11.2 calls
// idempotent and re-enterable, not a new run.

// WizardSteps returns every stored step row. A fresh database has none, which
// internal/setup reads as "every step is pending" rather than as an error: rows
// appear as steps are entered, so the absence of a row is itself the initial
// state.
func (s *Store) WizardSteps(ctx context.Context, tx Tx) ([]model.WizardStepRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT step, state, data_json, updated_at FROM wizard_steps`)
	if err != nil {
		return nil, fmt.Errorf("select wizard_steps: %w", err)
	}
	defer rows.Close()

	var out []model.WizardStepRow
	for rows.Next() {
		var v model.WizardStepRow
		if err := rows.Scan(&v.Step, &v.State, &v.DataJSON, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan wizard step: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// WizardStep returns one row, or ErrNotFound when the step has never been
// entered.
func (s *Store) WizardStep(ctx context.Context, tx Tx, step model.WizardStep) (model.WizardStepRow, error) {
	var out model.WizardStepRow
	err := tx.QueryRowContext(ctx,
		`SELECT step, state, data_json, updated_at FROM wizard_steps WHERE step = ?`,
		string(step)).
		Scan(&out.Step, &out.State, &out.DataJSON, &out.UpdatedAt)
	if err != nil {
		return model.WizardStepRow{}, notFound(err)
	}
	return out, nil
}

// PutWizardStep upserts one step. DataJSON is left ALONE when nil rather than
// nulled, because a step's stored data — the toolchain answer, the chosen
// version, the model the instance step was prefilled from — outlives a state
// change, and a caller that means to clear it passes an explicit JSON null
// through ClearWizardStepData.
func (s *Store) PutWizardStep(ctx context.Context, tx Tx, v model.WizardStepRow) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO wizard_steps (step, state, data_json, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(step) DO UPDATE SET
		   state      = excluded.state,
		   data_json  = COALESCE(excluded.data_json, wizard_steps.data_json),
		   updated_at = excluded.updated_at`,
		string(v.Step), string(v.State), v.DataJSON, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put wizard step: %w", err)
	}
	return nil
}

// ClearWizardStepData drops a step's stored data without touching its state.
func (s *Store) ClearWizardStepData(ctx context.Context, tx Tx, step model.WizardStep, at int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE wizard_steps SET data_json = NULL, updated_at = ? WHERE step = ?`,
		at, string(step))
	if err != nil {
		return fmt.Errorf("clear wizard step data: %w", err)
	}
	return nil
}
