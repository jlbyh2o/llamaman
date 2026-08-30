package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Queries the launcher (§5.6) and the supervisor (§5.8) need that no other
// caller does, kept in one file because they share one property: they are read
// by a process that must not grow a dependency on the services that own those
// tables.
//
// `instance-exec` runs with no D-Bus, no HTTP, no GPU probe and no network —
// its entire world is the binary, the DB file and `%i` — so it cannot ask
// internal/llamacpp which build is active or internal/models where a GGUF
// landed. Both answers are one indexed read each, and the projections below are
// deliberately narrow: exactly the columns §5.6 steps 5 and 6 consult, and not
// one more. When those services grow their own row types, these stay as they
// are; a launcher that had to construct a full `llamacpp_versions` row to learn
// whether a directory is safe to exec from would be coupled to every column a
// build pipeline adds.

// ActiveVersion is the `is_active=1` llamacpp_versions row as the launcher and
// the supervisor read it.
//
// State is here because of D78. A forced rebuild of the active version moves
// that row out of `ready` for the duration, and the directory `versions/active`
// names is the one being reinstalled — the row is the only thing either process
// can consult to know that. The launcher refuses with exit 69
// `runtime_rebuilding`; the supervisor does not attempt the start at all, so
// the exit code is the backstop rather than the normal path.
type ActiveVersion struct {
	ID      string
	DirName string
	State   model.VersionState
	// SupportsFit is `supports_fit`: false means this build predates `--fit`,
	// so `-ngl auto` renders as `-ngl 999` (§5.7).
	SupportsFit bool
	// HelpJSON is `help_flags_json` verbatim, or NULL. Only UnknownFlags reads
	// it, and the launcher never runs that check — it has no user to warn.
	HelpJSON *string
}

// Ready reports whether this build may be executed from right now.
func (v ActiveVersion) Ready() bool { return v.State == model.VersionReady }

// ActiveVersion returns the active llama.cpp build, or ErrNotFound when no
// build is active — an ordinary state on a fresh install, and the reason the
// launcher answers it with exit 69 rather than crashing.
func (s *Store) ActiveVersion(ctx context.Context, tx Tx) (ActiveVersion, error) {
	var (
		v     ActiveVersion
		state string
		fit   int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, dir_name, state, supports_fit, help_flags_json
		   FROM llamacpp_versions WHERE is_active = 1`).
		Scan(&v.ID, &v.DirName, &state, &fit, &v.HelpJSON)
	if err != nil {
		return ActiveVersion{}, notFound(err)
	}
	v.State = model.VersionState(state)
	v.SupportsFit = fit != 0
	return v, nil
}

// ModelPaths is a `models` row as a renderer reads it: one id for the hash and
// the two halves of the one path llama.cpp is given.
//
// PrimaryFile is shard 1 for a sharded set, which is the file llama.cpp opens
// and from which it finds the rest (§7.3), so the launcher validates exactly
// one path per model reference however many files are on disk.
type ModelPaths struct {
	ID          string
	SnapshotDir string
	PrimaryFile string
	ShardCount  int
	State       model.ModelState
	Kind        model.ModelKind
}

// ModelPathsByID returns one entry per id that exists. A missing id is simply
// absent from the map, which is how a reference to a purged model is reported
// without an error type — the launcher turns that absence into exit 72 with the
// resolved path in the message, exactly as it does for a file that is gone.
func (s *Store) ModelPathsByID(ctx context.Context, tx Tx, ids []string) (map[string]ModelPaths, error) {
	out := make(map[string]ModelPaths, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id, snapshot_dir, primary_file, shard_count, state, kind
		   FROM models WHERE id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("select models: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			m     ModelPaths
			state string
			kind  string
		)
		if err := rows.Scan(&m.ID, &m.SnapshotDir, &m.PrimaryFile,
			&m.ShardCount, &state, &kind); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		m.State = model.ModelState(state)
		m.Kind = model.ModelKind(kind)
		out[m.ID] = m
	}
	return out, rows.Err()
}

// RelabelBootStarts is D74's single relabel, and every one of its clauses is a
// condition §5.8 spells out.
//
// A start systemd performed at boot through `llamaman-instances.target`, before
// the daemon was up, is honestly recorded as `external` by the launcher —
// nobody stamped it. It is nevertheless misleading, because the user did ask
// for it by enabling autostart. Boot reconciliation rewrites it, and ONLY under
// all three conditions at once: this is the first daemon start of a new host
// boot (the caller's `host_boot_changed`, which is why this method is not
// called otherwise), the row's `at` falls in `[host_boot_at, boot_at)`, and the
// instance has `autostart=1`.
//
// The window's lower bound is the HOST boot instant from `/proc/stat`'s btime,
// never the daemon's own `boot_at`. Using the daemon start time was the bug the
// design names: every ordinary daemon restart, including the one every
// self-update performs, makes all prior `external` starts of an autostart
// instance older than `boot_at`, so a deliberate `systemctl start` typed at a
// shell three days ago would get rewritten as `autostart`.
func (s *Store) RelabelBootStarts(ctx context.Context, tx Tx, hostBootAt, bootAt int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instance_starts SET trigger = 'autostart'
		  WHERE trigger = 'external'
		    AND at >= ? AND at < ?
		    AND instance_id IN (SELECT id FROM instances WHERE autostart = 1)`,
		hostBootAt, bootAt)
	if err != nil {
		return 0, fmt.Errorf("relabel boot starts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("relabel boot starts: %w", err)
	}
	return n, nil
}

// AutostartCoupling is one instance as D53's coupling reads it: what the unit
// does at a host boot, and what the reconciler currently wants.
type AutostartCoupling struct {
	ID           string
	Name         string
	Autostart    bool
	DesiredState model.DesiredState
}

// AutostartCouplings returns every non-deleted instance's coupling pair, which
// is the whole input to boot reconciliation step 1: `desired_state :=
// autostart ? 'running' : 'stopped'`, one event per CHANGE.
//
// It is a projection rather than a full instance listing because the coupling
// reads two columns and writes one, and running it over `Instances` would make
// the only automatic write to `desired_state` depend on every column an
// instance has.
func (s *Store) AutostartCouplings(ctx context.Context, tx Tx) ([]AutostartCoupling, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, autostart, desired_state FROM instances
		  WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("select autostart couplings: %w", err)
	}
	defer rows.Close()

	var out []AutostartCoupling
	for rows.Next() {
		var (
			c       AutostartCoupling
			auto    int64
			desired string
		)
		if err := rows.Scan(&c.ID, &c.Name, &auto, &desired); err != nil {
			return nil, fmt.Errorf("scan autostart coupling: %w", err)
		}
		c.Autostart = auto != 0
		c.DesiredState = model.DesiredState(desired)
		out = append(out, c)
	}
	return out, rows.Err()
}

// placeholders renders `?, ?, …` for an IN clause of n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
