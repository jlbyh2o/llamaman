package model

// Settings (DESIGN section 2.1).
//
// `settings` rows are ABSENT until changed — defaults live in internal/settings,
// so a fresh database is a working install, which is the whole of SPEC §3.9's
// "no config file, ever". A row therefore always means "a human or a named
// system step decided this", and `updated_by` records which.

// SettingUpdatedBy is `settings.updated_by` (§2.1).
type SettingUpdatedBy string

const (
	// UpdatedByAdmin is the default: an edit made in the UI.
	UpdatedByAdmin SettingUpdatedBy = "admin"
	// UpdatedBySystem is a seed the daemon wrote on its own behalf — the one
	// case in v1 being `ui.port_desired` taken from `serve --port N` on a fresh
	// install (§11.1 step 6b), where the flag is a seed and never an override.
	UpdatedBySystem SettingUpdatedBy = "system"
	// UpdatedByWizard is a value the setup wizard stored (§11.2).
	UpdatedByWizard SettingUpdatedBy = "wizard"
)

// SettingUpdatedByValues lists the members of the `settings.updated_by` CHECK
// constraint, in order.
func SettingUpdatedByValues() []SettingUpdatedBy {
	return []SettingUpdatedBy{UpdatedByAdmin, UpdatedBySystem, UpdatedByWizard}
}

// Valid reports whether u is a member of the CHECK constraint.
func (u SettingUpdatedBy) Valid() bool { return valid(u, SettingUpdatedByValues()) }

// Setting is one row of `settings`. Value is JSON — the column carries
// `CHECK (json_valid(value))` — so the typed registry in internal/settings can
// hold ints, bools, strings and enums in one table without a type column.
type Setting struct {
	Key       string
	Value     string
	UpdatedAt int64
	UpdatedBy SettingUpdatedBy
}
