package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Settings queries (DESIGN section 2.1).
//
// A row exists only for a setting somebody changed; defaults live in
// internal/settings and never reach this table. Every method here is therefore
// about the OVERRIDE layer, and Delete is a first-class operation rather than an
// oversight: removing a row is how a setting goes back to following the built-in
// default, including a default a later release changes.

// Setting returns one override row, or ErrNotFound when the key has never been
// set — which the caller reads as "use the registry default".
func (s *Store) Setting(ctx context.Context, tx Tx, key string) (model.Setting, error) {
	var out model.Setting
	err := tx.QueryRowContext(ctx,
		`SELECT key, value, updated_at, updated_by FROM settings WHERE key = ?`, key).
		Scan(&out.Key, &out.Value, &out.UpdatedAt, &out.UpdatedBy)
	if err != nil {
		return model.Setting{}, notFound(err)
	}
	return out, nil
}

// Settings returns every override row, ordered by key, which is what the boot
// sequence loads once into the read-through cache (§11.1 step 5).
func (s *Store) Settings(ctx context.Context, tx Tx) ([]model.Setting, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key, value, updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("select settings: %w", err)
	}
	defer rows.Close()

	var out []model.Setting
	for rows.Next() {
		var v model.Setting
		if err := rows.Scan(&v.Key, &v.Value, &v.UpdatedAt, &v.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PutSetting upserts one override. Value must be valid JSON — the column's CHECK
// enforces it, which is what lets the typed registry keep ints, bools, strings
// and enums in one table without a type column.
func (s *Store) PutSetting(ctx context.Context, tx Tx, v model.Setting) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at, updated_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   value      = excluded.value,
		   updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		v.Key, v.Value, v.UpdatedAt, v.UpdatedBy)
	if err != nil {
		return fmt.Errorf("put setting %q: %w", v.Key, err)
	}
	return nil
}

// PutSettingIfAbsent inserts an override only when the key has no row yet, and
// reports whether it wrote one. This is §11.1 step 6b's seed rule expressed as
// one statement: `serve --port N` writes `ui.port_desired` on a FRESH install
// and is otherwise ignored, because the stored setting was chosen in the UI,
// deliberately, by a human. The flag is a seed, never an override.
func (s *Store) PutSettingIfAbsent(ctx context.Context, tx Tx, v model.Setting) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at, updated_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`,
		v.Key, v.Value, v.UpdatedAt, v.UpdatedBy)
	if err != nil {
		return false, fmt.Errorf("seed setting %q: %w", v.Key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("seed setting %q: %w", v.Key, err)
	}
	return n > 0, nil
}

// DeleteSetting removes an override, returning the setting to its built-in
// default. Deleting a key that has no row is not an error.
func (s *Store) DeleteSetting(ctx context.Context, tx Tx, key string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete setting %q: %w", key, err)
	}
	return nil
}
