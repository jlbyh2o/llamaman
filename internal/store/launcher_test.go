package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The queries `instance-exec` and the supervisor need (DESIGN sections 5.6 and
// 5.8), against a real database — which is the only place their behavior is
// decided, since every one of them is a projection or a correlated update.

// seedVersion writes one `llamacpp_versions` row.
func seedVersion(t *testing.T, s *Store, id string, state model.VersionState, active bool) {
	t.Helper()
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO llamacpp_versions
			   (id, channel, tag, acquisition, backend, dir_name, state, is_active,
			    supports_fit, help_flags_json, created_at)
			 VALUES (?, 'stable', ?, 'prebuilt', 'cpu', ?, ?, ?, 1, '["--jinja"]', 1000)`,
			id, id, id, string(state), boolInt(active))
		return err
	})
}

// seedModelRow writes a cache root and one `models` row pointing into it.
func seedModelRow(t *testing.T, s *Store, id, dir, file string) {
	t.Helper()
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO hf_cache_roots (id, path, is_primary, writable, created_at)
			 VALUES (?, ?, 1, 1, 1000) ON CONFLICT(id) DO NOTHING`,
			"root-1", "/var/cache/hub"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO models
			   (id, root_id, repo_id, revision, kind, state, origin, snapshot_dir,
			    primary_file, shard_count, created_at, updated_at)
			 VALUES (?, 'root-1', ?, 'deadbeef', 'text', 'ready', 'llamaman', ?, ?, 1, 1000, 1000)`,
			id, "acme/"+id, dir, file)
		return err
	})
}

// TestActiveVersion pins the narrow projection §5.6 step 5 reads, including the
// `state` column that closes D78's hazard: the directory `versions/active` names
// is the one a forced rebuild is reinstalling, and the row is the only thing the
// launcher can consult to know that.
func TestActiveVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ActiveVersion(ctx, s.RO); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with no active build: err = %v, want ErrNotFound — an ordinary state on a fresh install", err)
	}

	seedVersion(t, s, "b10600-cpu-bin", model.VersionReady, false)
	seedVersion(t, s, "b10621-cpu-bin", model.VersionReady, true)

	got, err := s.ActiveVersion(ctx, s.RO)
	if err != nil {
		t.Fatalf("ActiveVersion: %v", err)
	}
	if got.ID != "b10621-cpu-bin" || got.DirName != "b10621-cpu-bin" {
		t.Errorf("active version = %+v, want the is_active=1 row", got)
	}
	if !got.SupportsFit {
		t.Error("supports_fit did not round-trip; `-ngl auto` would render as `-ngl 999`")
	}
	if got.HelpJSON == nil || *got.HelpJSON != `["--jinja"]` {
		t.Errorf("help_flags_json = %v, want the stored capture", got.HelpJSON)
	}
	if !got.Ready() {
		t.Error("a `ready` row must report Ready()")
	}

	// A forced rebuild moves the ACTIVE row out of `ready` (D78). The row stays
	// active — that is the whole point — and Ready() is what refuses the start.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE llamacpp_versions SET state = 'building' WHERE id = 'b10621-cpu-bin'`)
		return err
	})
	got, err = s.ActiveVersion(ctx, s.RO)
	if err != nil {
		t.Fatalf("ActiveVersion: %v", err)
	}
	if got.Ready() {
		t.Error("a rebuilding version reported Ready(); nothing may start against a directory mid-swap")
	}
}

// TestModelPathsByID pins the launcher's step-6 lookup, including the one thing
// it must do rather than error on: report a missing id by ABSENCE, so a
// reference to a purged model becomes exit 72 like any other missing file.
func TestModelPathsByID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedModelRow(t, s, "m-primary", "/hub/primary/snapshots/a", "model.gguf")
	seedModelRow(t, s, "m-draft", "/hub/draft/snapshots/b", "draft-00001-of-00003.gguf")

	got, err := s.ModelPathsByID(ctx, s.RO,
		[]string{"m-primary", "m-draft", "m-gone", "m-primary", ""})
	if err != nil {
		t.Fatalf("ModelPathsByID: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2 (a missing id is absent, not an error)", len(got))
	}
	if p := got["m-primary"]; p.SnapshotDir != "/hub/primary/snapshots/a" || p.PrimaryFile != "model.gguf" {
		t.Errorf("m-primary = %+v", p)
	}
	if p := got["m-draft"]; p.PrimaryFile != "draft-00001-of-00003.gguf" {
		t.Errorf("a sharded set must resolve to SHARD 1: %+v", p)
	}
	if _, ok := got["m-gone"]; ok {
		t.Error("a model id that does not exist came back present")
	}

	// An empty request is not an error either: an instance may reference no
	// mmproj and no draft, and asking for nothing must not need a branch in the
	// caller.
	empty, err := s.ModelPathsByID(ctx, s.RO, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty request: (%v, %v), want an empty map and no error", empty, err)
	}
}

// TestCountFailedStartsExcludesTheThreeCodes is §5.8's counting query, and each
// exclusion is a bug the bare "count every failed row" rule would have shipped.
func TestCountFailedStartsExcludesTheThreeCodes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedInstance(t, s, newInstance("i-1", "qwen", 8081, 21001))

	rows := []struct {
		id        string
		outcome   model.StartOutcome
		errorCode string
		counted   bool
		why       string
	}{
		{"a", model.OutcomeFailed, "", true, "a plain failure counts"},
		{"b", model.OutcomeFailed, "model_missing", true,
			"a preflight failure counts — that is what D54 opens the row early FOR"},
		{"c", model.OutcomeStopped, "", false, "a user restart is not a crash"},
		{"d", model.OutcomeInhibited, "policy_never", false,
			"counting a refusal would make it self-reinforcing"},
		{"e", model.OutcomeFailed, "schema_mismatch", false,
			"the daemon's upgrade state is not the instance's fault"},
		{"f", model.OutcomeFailed, "schema_ahead", false, "likewise"},
		{"g", model.OutcomeFailed, "launcher_superseded", false,
			"nothing observed how that run ended, so counting it is a guess"},
	}

	wantCount := 0
	for i, r := range rows {
		if r.counted {
			wantCount++
		}
		row := model.InstanceStart{
			ID:         r.id,
			InstanceID: "i-1",
			At:         int64(2000 + i),
			Trigger:    model.StartBySupervisorRestart,
			ConfigHash: "hash",
			Outcome:    &rows[i].outcome,
			EndedAt:    ptr(int64(2000 + i)),
		}
		if r.errorCode != "" {
			row.ErrorCode = &rows[i].errorCode
		}
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			return s.InsertInstanceStart(ctx, tx, row)
		})
	}

	got, err := s.CountFailedStartsSince(ctx, s.RO, "i-1", 0)
	if err != nil {
		t.Fatalf("CountFailedStartsSince: %v", err)
	}
	if got != wantCount {
		t.Errorf("count = %d, want %d", got, wantCount)
		for _, r := range rows {
			t.Logf("  %s: outcome=%s error_code=%q counted=%t — %s",
				r.id, r.outcome, r.errorCode, r.counted, r.why)
		}
	}
}

// TestRelabelBootStarts is D74's window, and the three conditions that all have
// to hold at once.
func TestRelabelBootStarts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	auto := newInstance("i-auto", "qwen", 8081, 21001)
	auto.Autostart = true
	seedInstance(t, s, auto)
	seedInstance(t, s, newInstance("i-manual", "gemma", 8082, 21002))

	const hostBootAt, bootAt = 10_000, 40_000
	rows := []struct {
		id       string
		instance string
		at       int64
		trigger  model.StartTrigger
		want     model.StartTrigger
	}{
		{"in", "i-auto", 20_000, model.StartByExternal, model.StartByAutostart},
		{"at-lower-bound", "i-auto", hostBootAt, model.StartByExternal, model.StartByAutostart},
		{"at-upper-bound", "i-auto", bootAt, model.StartByExternal, model.StartByExternal},
		{"before", "i-auto", 5_000, model.StartByExternal, model.StartByExternal},
		{"after", "i-auto", 50_000, model.StartByExternal, model.StartByExternal},
		{"not-autostart", "i-manual", 20_000, model.StartByExternal, model.StartByExternal},
		// Only `external` rows are rewritten: a start the daemon stamped is
		// already the truth.
		{"stamped", "i-auto", 20_000, model.StartByUser, model.StartByUser},
	}
	for i, r := range rows {
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			return s.InsertInstanceStart(ctx, tx, model.InstanceStart{
				ID:         r.id,
				InstanceID: r.instance,
				At:         r.at,
				Trigger:    r.trigger,
				ConfigHash: "hash",
				Outcome:    ptr(model.OutcomeStopped),
				EndedAt:    ptr(r.at + int64(i)),
			})
		})
	}

	var n int64
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		got, err := s.RelabelBootStarts(ctx, tx, hostBootAt, bootAt)
		n = got
		return err
	})
	if n != 2 {
		t.Errorf("relabeled %d rows, want 2", n)
	}

	for _, r := range rows {
		var got string
		err := s.RO.QueryRowContext(ctx,
			`SELECT trigger FROM instance_starts WHERE id = ?`, r.id).Scan(&got)
		if err != nil {
			t.Fatalf("read %s: %v", r.id, err)
		}
		if model.StartTrigger(got) != r.want {
			t.Errorf("%s: trigger = %q, want %q", r.id, got, r.want)
		}
	}
}

// TestAutostartCouplings is the whole input to D53's coupling: two columns per
// non-deleted instance, and nothing else.
func TestAutostartCouplings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	on := newInstance("i-on", "qwen", 8081, 21001)
	on.Autostart = true
	on.DesiredState = model.DesiredStopped
	seedInstance(t, s, on)

	off := newInstance("i-off", "gemma", 8082, 21002)
	off.DesiredState = model.DesiredRunning
	seedInstance(t, s, off)

	gone := newInstance("i-gone", "phi", 8083, 21003)
	gone.Autostart = true
	seedInstance(t, s, gone)
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := s.SoftDeleteInstance(ctx, tx, "i-gone", 9000)
		return err
	})

	got, err := s.AutostartCouplings(ctx, s.RO)
	if err != nil {
		t.Fatalf("AutostartCouplings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d couplings, want 2 — a soft-deleted instance is not coupled: %+v", len(got), got)
	}
	if got[0].ID != "i-off" || got[0].Autostart || got[0].DesiredState != model.DesiredRunning {
		t.Errorf("first coupling = %+v", got[0])
	}
	if got[1].ID != "i-on" || !got[1].Autostart || got[1].DesiredState != model.DesiredStopped {
		t.Errorf("second coupling = %+v", got[1])
	}
}
