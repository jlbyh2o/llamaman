package store

import (
	"context"
	"fmt"
)

// `flag_presets` queries (DESIGN sections 2.8, 3.11).
//
// A preset is a named `flags_json` document plus `extra_flags` and NOTHING else
// — no model, no ports, no name of an instance. That is what makes "apply this
// preset to these five instances" a meaningful operation: everything a preset
// carries is a tuning decision, and everything it deliberately omits is an
// identity decision.
//
// `builtin` rows are shipped presets. They are readable and appliable like any
// other and refuse to be edited or deleted, so a user who has overwritten
// "Balanced" locally still has the original to return to.

// FlagPreset is one `flag_presets` row.
type FlagPreset struct {
	ID          string
	Name        string
	Description *string
	// FlagsJSON is the serialized model.FlagSet. It stays a string here for the
	// same reason `instances.flags_json` does: this package holds SQL, and the
	// FlagSet's meaning belongs to the package that renders argv from it.
	FlagsJSON  string
	ExtraFlags string
	Builtin    bool
	CreatedAt  int64
	UpdatedAt  int64
}

const presetColumns = `id, name, description, flags_json, extra_flags, builtin, created_at, updated_at`

// FlagPresets lists every preset, built-ins first and then by name, which is the
// order a picker wants: the shipped starting points, then what this host added.
func (s *Store) FlagPresets(ctx context.Context, tx Tx) ([]FlagPreset, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+presetColumns+` FROM flag_presets ORDER BY builtin DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("list flag presets: %w", err)
	}
	defer rows.Close()

	var out []FlagPreset
	for rows.Next() {
		p, err := scanFlagPreset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FlagPreset returns one row, or ErrNotFound.
func (s *Store) FlagPreset(ctx context.Context, tx Tx, id string) (FlagPreset, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+presetColumns+` FROM flag_presets WHERE id = ?`, id)
	p, err := scanFlagPreset(row)
	if err != nil {
		return FlagPreset{}, notFound(err)
	}
	return p, nil
}

// InsertFlagPreset writes a new preset. A duplicate name violates the table's
// UNIQUE and surfaces as the constraint error the service maps to a 409.
func (s *Store) InsertFlagPreset(ctx context.Context, tx Tx, p FlagPreset) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO flag_presets (`+presetColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.FlagsJSON, p.ExtraFlags, boolInt(p.Builtin),
		p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert flag preset %s: %w", p.Name, err)
	}
	return nil
}

// UpdateFlagPreset rewrites the editable half of a preset, reporting whether a
// row moved. `builtin` is never among them: a shipped preset is a constant.
func (s *Store) UpdateFlagPreset(ctx context.Context, tx Tx, p FlagPreset) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE flag_presets
		    SET name = ?, description = ?, flags_json = ?, extra_flags = ?, updated_at = ?
		  WHERE id = ? AND builtin = 0`,
		p.Name, p.Description, p.FlagsJSON, p.ExtraFlags, p.UpdatedAt, p.ID)
	if err != nil {
		return false, fmt.Errorf("update flag preset %s: %w", p.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update flag preset %s: %w", p.ID, err)
	}
	return n > 0, nil
}

// DeleteFlagPreset removes a non-builtin preset, reporting whether a row went.
func (s *Store) DeleteFlagPreset(ctx context.Context, tx Tx, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM flag_presets WHERE id = ? AND builtin = 0`, id)
	if err != nil {
		return false, fmt.Errorf("delete flag preset %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete flag preset %s: %w", id, err)
	}
	return n > 0, nil
}

func scanFlagPreset(sc scanner) (FlagPreset, error) {
	var (
		p       FlagPreset
		builtin int64
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.Description, &p.FlagsJSON, &p.ExtraFlags,
		&builtin, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return FlagPreset{}, err
	}
	p.Builtin = builtin != 0
	return p, nil
}
