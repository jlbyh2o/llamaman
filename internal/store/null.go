package store

import (
	"database/sql"
	"fmt"
)

// boolInt renders a Go bool into the INTEGER 0/1 a NOT NULL boolean column
// carries. STRICT tables reject a Go bool outright, so this is not a
// convenience: it is the conversion the driver boundary requires.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// rowsChanged reports whether a conditional UPDATE matched. Several statements
// in this package are written as "act only if the precondition still holds" —
// extend a lease only for the daemon that owns it, claim a setup token only
// while it is unclaimed — and for those, zero rows changed is the ANSWER rather
// than an error.
func rowsChanged(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// Nullable columns.
//
// Section 2 uses NULL as a fact, not as an absent zero: `polkit_ok` is NULL in
// user scope because the question was neither asked nor denied (§5.2a);
// `instance_status.requests_served` is NULL when the metrics endpoint is off, and
// the UI must say "metrics disabled" rather than show 0 (§2.9); a failed
// nvidia-smi returns null fields and never zeros (F14). So the row structs use
// pointers and these four helpers move values across the driver boundary without
// ever collapsing NULL into a zero.

// enumPtr converts a scanned *string into a pointer to a string-kinded enum.
func enumPtr[T ~string](s *string) *T {
	if s == nil {
		return nil
	}
	v := T(*s)
	return &v
}

// enumArg converts a pointer to a string-kinded enum into a driver argument:
// nil becomes SQL NULL, a value becomes its string.
func enumArg[T ~string](v *T) any {
	if v == nil {
		return nil
	}
	return string(*v)
}

// boolPtr converts a scanned nullable INTEGER 0/1 column into *bool.
func boolPtr(i *int64) *bool {
	if i == nil {
		return nil
	}
	b := *i != 0
	return &b
}

// boolArg converts a *bool into a driver argument for a nullable INTEGER 0/1
// column: nil becomes SQL NULL, not 0.
func boolArg(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return int64(1)
	}
	return int64(0)
}
