# Migrations

Numbered SQL files (`0001_init.sql`, `0002_….sql`, …) embedded into the binary
and applied in order by the ~120-line runner in `internal/store` — DESIGN
section 2 and section 14 ("a migration library … has no failure modes we do not
want to own").

Rules: a file is immutable once released; corrections ship as a new file.
`make migrate-new name=add_x` scaffolds the next number.

No migrations exist yet — the schema lands with DESIGN section 2's tables.
