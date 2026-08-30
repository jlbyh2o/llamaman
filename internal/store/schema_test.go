package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Schema tests: the migration is the contract of DESIGN section 2, so these
// assert the contract against a real database rather than against the file.
//
// The centerpiece is TestCheckConstraints, which proves every one of the
// schema's CHECK constraints with an illegal insert, and — because a table of
// cases can silently fall behind the schema it is meant to cover — derives the
// list of constraints from the migration SQL itself and fails when a case is
// missing.

// -----------------------------------------------------------------------------
// Inventory
// -----------------------------------------------------------------------------

// TestMigrationAppliesOnFreshDB is the "every migration applied to a fresh file"
// case of section 15, plus the inventory that makes a silent partial apply
// impossible to miss.
func TestMigrationAppliesOnFreshDB(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	t.Run("tables", func(t *testing.T) {
		got := scanStrings(t, s, `SELECT name FROM sqlite_master
		                           WHERE type='table' AND name NOT LIKE 'sqlite_%'
		                           ORDER BY name`)
		if len(got) != wantTableCount {
			t.Errorf("table count = %d, want %d\n%s", len(got), wantTableCount, strings.Join(got, "\n"))
		}
	})

	t.Run("every table is STRICT", func(t *testing.T) {
		loose := scanStrings(t, s, `SELECT name FROM sqlite_master
		                             WHERE type='table' AND name NOT LIKE 'sqlite_%'
		                               AND sql NOT LIKE '%STRICT%'
		                             ORDER BY name`)
		if len(loose) > 0 {
			t.Errorf("tables without STRICT: %v — section 2 says ALL tables are STRICT", loose)
		}
	})

	t.Run("explicit indexes", func(t *testing.T) {
		got := scanStrings(t, s, `SELECT name FROM sqlite_master
		                           WHERE type='index' AND sql IS NOT NULL ORDER BY name`)
		if len(got) != wantIndexCount {
			t.Errorf("index count = %d, want %d\n%s", len(got), wantIndexCount, strings.Join(got, "\n"))
		}
	})

	t.Run("integrity", func(t *testing.T) {
		if err := s.IntegrityCheck(ctx, CheckOptions{}); err != nil {
			t.Errorf("IntegrityCheck on a freshly migrated database: %v", err)
		}
		violations, err := s.ForeignKeyCheck(ctx, s.RO)
		if err != nil {
			t.Fatalf("ForeignKeyCheck: %v", err)
		}
		if len(violations) > 0 {
			t.Errorf("foreign key violations on a fresh database: %v", violations)
		}
	})

	t.Run("recorded in schema_migrations", func(t *testing.T) {
		embedded, err := Migrations()
		if err != nil {
			t.Fatalf("Migrations: %v", err)
		}
		applied, err := s.AppliedMigrations(ctx, s.RO)
		if err != nil {
			t.Fatalf("AppliedMigrations: %v", err)
		}
		if len(applied) != len(embedded) {
			t.Fatalf("applied %d migrations, embedded %d", len(applied), len(embedded))
		}
		for i, a := range applied {
			if a.Version != embedded[i].Version || a.Checksum != embedded[i].Checksum {
				t.Errorf("row %d = {%d %s}, want {%d %s}",
					i, a.Version, a.Checksum, embedded[i].Version, embedded[i].Checksum)
			}
			if a.AppliedAt == 0 {
				t.Errorf("migration %d has no applied_at stamp", a.Version)
			}
		}
	})
}

// wantTableCount and wantIndexCount are section 2's own inventory: 41 tables
// across 2.1–2.11 and the 25 indexes those subsections declare. They are
// hard-coded rather than derived so that DROPPING a table is a test failure and
// not merely a smaller number.
const (
	wantTableCount = 41
	wantIndexCount = 25
)

// TestSeedRows proves the only rows a fresh database carries are the two
// singleton leases, and that `settings` is empty — section 2.1's "rows are
// absent until changed; defaults live in internal/settings, so a fresh DB is a
// working install", which is the whole of SPEC §3.9's "no config file, ever".
// Seeding the registry's defaults would freeze them at install time and stop a
// later release from ever changing one.
func TestSeedRows(t *testing.T) {
	s := newTestStore(t)

	for _, table := range []string{"build_lease", "bench_lease"} {
		var id int64
		if err := s.RO.QueryRow(`SELECT id FROM ` + table).Scan(&id); err != nil {
			t.Errorf("%s has no seed row: %v — §2.3's acquire is an UPDATE … WHERE id=1", table, err)
			continue
		}
		if id != 1 {
			t.Errorf("%s seed id = %d, want 1", table, id)
		}
		var owner *string
		if err := s.RO.QueryRow(`SELECT owner FROM ` + table).Scan(&owner); err != nil {
			t.Fatalf("%s owner: %v", table, err)
		}
		if owner != nil {
			t.Errorf("%s seed row is held by %q, want an unheld lease", table, *owner)
		}
	}

	for _, table := range []string{"settings", "runtime_info", "wizard_steps", "setup_claim"} {
		var n int
		if err := s.RO.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d seeded rows, want 0", table, n)
		}
	}
}

// -----------------------------------------------------------------------------
// STRICT and CHECK
// -----------------------------------------------------------------------------

// TestStrictRejectsWrongType proves STRICT is doing what section 2 relies on it
// for. Without it SQLite would happily store the string 'soon' in an INTEGER
// millisecond column and every later comparison against it would be quietly
// wrong.
func TestStrictRejectsWrongType(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		stmt string
		args []any
	}{
		{
			name: "text into an INTEGER timestamp",
			stmt: `INSERT INTO events (id, at, level, category, action, actor, message)
			       VALUES ('e1', ?, 'info', 'system', 'boot', 'system', 'hello')`,
			args: []any{"soon"},
		},
		{
			name: "text into an INTEGER port",
			stmt: `INSERT INTO instances
			         (id, name, public_port, internal_port, flags_json, config_hash, unit_name,
			          created_at, updated_at)
			       VALUES ('i1', 'a', ?, 21000, '{}', 'h', 'u', 1, 1)`,
			args: []any{"eight thousand"},
		},
		{
			name: "real into an INTEGER count",
			stmt: `INSERT INTO jobs (id, kind, subject_type, subject_id, state, run_after,
			                         created_at, attempts)
			       VALUES ('j1', 'maintenance', 'system', 'maintenance', 'queued', 0, 0, ?)`,
			args: []any{1.5},
		},
		{
			name: "text into a BLOB",
			stmt: `INSERT INTO secrets (name, nonce, ciphertext, created_at, updated_at)
			       VALUES ('hf_token', ?, x'00', 1, 1)`,
			args: []any{"not-a-blob"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
				_, err := tx.ExecContext(ctx, tt.stmt, tt.args...)
				return err
			})
			if err == nil {
				t.Fatal("insert succeeded; the table is not STRICT")
			}
			// SQLITE_CONSTRAINT_DATATYPE: STRICT names the column and both types,
			// which is what makes a bad write a fixable message rather than a
			// silently coerced value.
			if !strings.Contains(err.Error(), "cannot store") {
				t.Errorf("error = %v, want a STRICT datatype rejection", err)
			}
		})
	}
}

// TestCheckConstraints proves every CHECK constraint in the schema with one
// illegal insert, and asserts that the case table covers every constraint the
// migration actually declares — so a constraint added to the SQL without a case
// fails here rather than going untested.
func TestCheckConstraints(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedFixtures(t, s)

	declared := parseChecks(t)
	cases := checkCases()

	t.Run("coverage", func(t *testing.T) {
		var missing, extra []string
		for key := range declared {
			if _, ok := cases[key]; !ok {
				missing = append(missing, key)
			}
		}
		for key := range cases {
			if _, ok := declared[key]; !ok {
				extra = append(extra, key)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			t.Errorf("%d CHECK constraints have no illegal-insert case: %v", len(missing), missing)
		}
		if len(extra) > 0 {
			t.Errorf("%d cases name a CHECK the schema does not declare: %v", len(extra), extra)
		}
		if len(declared) != wantCheckCount {
			t.Errorf("schema declares %d CHECK constraints, expected %d",
				len(declared), wantCheckCount)
		}
	})

	keys := make([]string, 0, len(cases))
	for key := range cases {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		c := cases[key]
		t.Run(key, func(t *testing.T) {
			table, column, _ := strings.Cut(key, ".")

			// The valid row must insert cleanly, or the illegal insert below
			// would be proving nothing: a failure could come from anywhere.
			if err := insertRow(ctx, s, table, c.valid, c.pre); err != nil {
				t.Fatalf("the fixture's own valid row was rejected: %v", err)
			}

			bad := make(map[string]any, len(c.valid)+1)
			for k, v := range c.valid {
				bad[k] = v
			}
			bad[column] = c.illegal

			err := insertRow(ctx, s, table, bad, c.pre)
			if err == nil {
				t.Fatalf("%s = %#v was accepted; the CHECK is not enforced", key, c.illegal)
			}
			if !strings.Contains(err.Error(), "CHECK constraint failed") {
				t.Fatalf("%s = %#v failed for the wrong reason: %v", key, c.illegal, err)
			}
			if !strings.Contains(err.Error(), column) {
				t.Errorf("%s tripped a CHECK that does not mention %q: %v", key, column, err)
			}
		})
	}
}

// wantCheckCount is the number of CHECK constraints section 2 declares. It is
// pinned so that removing one from the schema — which would silently make its
// case unreachable — fails the coverage subtest.
const wantCheckCount = 107

// insertRow builds an INSERT from a column map and runs it inside a transaction
// that is always rolled back, so cases cannot see each other's rows. pre holds
// statements the row needs first — the two seeded singleton leases have to be
// cleared before a row with the same primary key can be inserted at all.
func insertRow(ctx context.Context, s *Store, table string, row map[string]any, pre []string) error {
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)

	args := make([]any, len(cols))
	ph := make([]string, len(cols))
	for i, c := range cols {
		args[i] = row[c]
		ph[i] = "?"
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ", "), strings.Join(ph, ", "))

	tx, err := s.RW.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, p := range pre {
		if _, err := tx.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, stmt, args...)
	return err
}

// -----------------------------------------------------------------------------
// Partial and unique indexes
// -----------------------------------------------------------------------------

// TestOneLiveJobPerSubject is D39 proven with two inserts: the second live job
// on one subject is refused by the DATABASE rather than by convention. Every
// 409 in the design — `download_exists`, `job_in_flight` — rests on this, and so
// does §2.3a's "there is exactly one live jobs row per domain row".
func TestOneLiveJobPerSubject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first := model.Job{
		ID: "job-a", Kind: model.JobModelDownload, SubjectType: model.SubjectDownload,
		SubjectID: "dl-1", State: model.JobQueued, Priority: 100, MaxAttempts: 1,
		RunAfter: 1000, CreatedAt: 1000,
	}
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, first) })

	second := first
	second.ID = "job-b"
	err := s.Write(ctx, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, second) })
	if err == nil {
		t.Fatal("a second live job on the same subject was accepted")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("error = %v, want a UNIQUE constraint failure", err)
	}

	// Every live state holds the subject, `paused` and `interrupted` included —
	// that is what stops a duplicate job from starting while a pause or an
	// unresolved finalizer stands.
	for _, st := range model.LiveJobStates() {
		mustWrite(t, s, func(ctx context.Context, tx Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE jobs SET state = ? WHERE id = 'job-a'`, string(st))
			return err
		})
		err := s.Write(ctx, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, second) })
		if err == nil {
			t.Errorf("state %q did not hold the subject", st)
		}
	}

	// A terminal job releases it, so the same subject can be worked again.
	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'succeeded' WHERE id = 'job-a'`)
		return err
	})
	mustWrite(t, s, func(ctx context.Context, tx Tx) error { return s.InsertJob(ctx, tx, second) })
}

// TestLiveJobStatesMatchTheIndex keeps model.LiveJobStates and the partial
// index's WHERE clause from drifting apart. They are the same rule written in
// two languages, and only this test makes them one.
func TestLiveJobStatesMatchTheIndex(t *testing.T) {
	s := newTestStore(t)

	var ddl string
	err := s.RO.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_jobs_one_live_per_subject'`).Scan(&ddl)
	if err != nil {
		t.Fatalf("read index DDL: %v", err)
	}
	for _, st := range model.LiveJobStates() {
		if !strings.Contains(ddl, "'"+string(st)+"'") {
			t.Errorf("model.LiveJobStates includes %q but the partial index does not", st)
		}
	}
	// And the reverse: nothing in the index that Go does not call live.
	for _, st := range model.JobStateValues() {
		inIndex := strings.Contains(ddl, "'"+string(st)+"'")
		if inIndex != st.IsLive() {
			t.Errorf("state %q: in index = %v, IsLive = %v", st, inIndex, st.IsLive())
		}
	}
	if !strings.Contains(liveStatesSQL, "'interrupted'") {
		t.Error("liveStatesSQL has drifted from the index")
	}
}

// TestPartialUniqueIndexes covers the other four places where section 2 asks the
// database to enforce an at-most-one rule that convention could not.
func TestPartialUniqueIndexes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedFixtures(t, s)

	tests := []struct {
		name  string
		first string
		again string
		why   string
	}{
		{
			name: "one active llama.cpp version",
			first: `INSERT INTO llamacpp_versions
			          (id, channel, tag, acquisition, backend, dir_name, state, created_at, is_active)
			        VALUES ('v-a', 'stable', 'b1', 'source', 'cpu', 'v-a', 'ready', 1, 1)`,
			again: `INSERT INTO llamacpp_versions
			          (id, channel, tag, acquisition, backend, dir_name, state, created_at, is_active)
			        VALUES ('v-b', 'stable', 'b2', 'source', 'cpu', 'v-b', 'ready', 1, 1)`,
			why: "the row wins over the symlink at boot, so two active rows would be undefined (§2.5)",
		},
		{
			name: "one previous llama.cpp version",
			first: `INSERT INTO llamacpp_versions
			          (id, channel, tag, acquisition, backend, dir_name, state, created_at, previous_active)
			        VALUES ('v-c', 'stable', 'b3', 'source', 'cpu', 'v-c', 'ready', 1, 1)`,
			again: `INSERT INTO llamacpp_versions
			          (id, channel, tag, acquisition, backend, dir_name, state, created_at, previous_active)
			        VALUES ('v-d', 'stable', 'b4', 'source', 'cpu', 'v-d', 'ready', 1, 1)`,
			why: "rollback depth is 0 or 1 by construction (§2.5)",
		},
		{
			name: "one open start row per instance",
			first: `INSERT INTO instance_starts (id, instance_id, at, trigger, config_hash)
			        VALUES ('s-a', 'inst1', 1, 'user', 'h')`,
			again: `INSERT INTO instance_starts (id, instance_id, at, trigger, config_hash)
			        VALUES ('s-b', 'inst1', 2, 'user', 'h')`,
			why: "restart_required is undefined the moment two open rows exist (D40)",
		},
		{
			name: "one primary cache root",
			first: `INSERT INTO hf_cache_roots (id, path, created_at, is_primary)
			        VALUES ('r-a', '/a/hub', 1, 1)`,
			again: `INSERT INTO hf_cache_roots (id, path, created_at, is_primary)
			        VALUES ('r-b', '/b/hub', 1, 1)`,
			why: "exactly one root is ever written to (§2.6)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := s.RW.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()

			if _, err := tx.ExecContext(ctx, tt.first); err != nil {
				t.Fatalf("first insert: %v", err)
			}
			if _, err := tx.ExecContext(ctx, tt.again); err == nil {
				t.Fatalf("second insert was accepted — %s", tt.why)
			} else if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				t.Errorf("second insert failed for the wrong reason: %v", err)
			}
		})
	}
}

// TestSoftDeleteFreesNameAndPorts is D68's reason for scoping the instance
// unique indexes to live rows: a plain UNIQUE would consume the name and both
// ports forever the first time an instance was deleted.
func TestSoftDeleteFreesNameAndPorts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const ins = `INSERT INTO instances
	               (id, name, public_port, internal_port, flags_json, config_hash, unit_name,
	                created_at, updated_at, deleted_at)
	             VALUES (?, 'qwen', 8081, 21001, '{}', 'h', 'u', 1, 1, ?)`

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, ins, "i-1", nil)
		return err
	})

	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, ins, "i-2", nil)
		return err
	})
	if err == nil {
		t.Fatal("two live instances took the same name and ports")
	}

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET deleted_at = 99 WHERE id = 'i-1'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, ins, "i-2", nil)
		return err
	})
}

// TestForeignKeysEnforced proves foreign_keys=ON is in force for real writes,
// which is what the two-layer job/domain design assumes everywhere.
func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Write(ctx, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key, route, request_fingerprint, job_id, created_at, expires_at)
			 VALUES ('k', 'POST /downloads', 'fp', 'no-such-job', 1, 2)`)
		return err
	})
	if err == nil {
		t.Fatal("an idempotency key naming a nonexistent job was accepted")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("error = %v, want a FOREIGN KEY constraint failure", err)
	}
}

// TestIdempotencyKeyCascades proves the ON DELETE CASCADE on job_id: pruning a
// terminal job (§2.11 retention) must not leave a key naming a row that is gone.
func TestIdempotencyKeyCascades(t *testing.T) {
	s := newTestStore(t)

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		if err := s.InsertJob(ctx, tx, model.Job{
			ID: "j1", Kind: model.JobModelDownload, SubjectType: model.SubjectDownload,
			SubjectID: "d1", State: model.JobQueued, MaxAttempts: 1, RunAfter: 1, CreatedAt: 1,
		}); err != nil {
			return err
		}
		return s.InsertIdempotencyKey(ctx, tx, model.IdempotencyKey{
			Key: "k1", Route: "POST /downloads", RequestFingerprint: "fp",
			JobID: "j1", CreatedAt: 1, ExpiresAt: 600_001,
		}, 1)
	})

	mustWrite(t, s, func(ctx context.Context, tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = 'j1'`)
		return err
	})

	var n int
	if err := s.RO.QueryRow(`SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("idempotency_keys rows after the job was deleted = %d, want 0", n)
	}
}

// -----------------------------------------------------------------------------
// Go / SQL agreement
// -----------------------------------------------------------------------------

// TestGoAndSQLEnumsAgree is the whole reason model.ClosedEnums exists: every
// closed enum in the schema is spelled twice, once in SQL and once in Go, and
// nothing but this test can keep the two lists equal — member for member and in
// the same order.
func TestGoAndSQLEnumsAgree(t *testing.T) {
	sqlEnums := parseEnums(t)
	goEnums := model.ClosedEnums()

	for key, want := range sqlEnums {
		got, ok := goEnums[key]
		if !ok {
			t.Errorf("%s is a closed enum in SQL with no Go counterpart: %v", key, want)
			continue
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s members differ (-sql +go):\n%s", key, diff)
		}
	}
	for key := range goEnums {
		if _, ok := sqlEnums[key]; !ok {
			t.Errorf("model.ClosedEnums has %s, which the schema does not close with a CHECK", key)
		}
	}
	if len(sqlEnums) != wantEnumCount {
		t.Errorf("schema closes %d enums, expected %d", len(sqlEnums), wantEnumCount)
	}
}

// wantEnumCount pins how many string-membership CHECK constraints the schema
// declares, so deleting one cannot quietly shrink the agreement test.
const wantEnumCount = 43

// -----------------------------------------------------------------------------
// Reading the schema back out of the migration
// -----------------------------------------------------------------------------

var (
	reCreateTable = regexp.MustCompile(`(?s)CREATE TABLE (\w+) \((.*?)\n\) STRICT;`)
	reComment     = regexp.MustCompile(`--.*`)
	reEnumClause  = regexp.MustCompile(`^(?:\w+ IS NULL OR )?(\w+) IN \((.*)\)$`)
	reMember      = regexp.MustCompile(`'([^']*)'`)
)

// checkFunctions are the SQL functions a CHECK may open with; the column a CHECK
// governs is the first identifier that is not one of them.
var checkFunctions = map[string]bool{"json_valid": true, "length": true}

// migrationSQL returns the init migration with its comments stripped, so a
// column name mentioned in prose cannot be mistaken for SQL.
func migrationSQL(t *testing.T) string {
	t.Helper()
	ms, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations are embedded")
	}
	return reComment.ReplaceAllString(ms[0].SQL, "")
}

// parseChecks returns every CHECK constraint the schema declares, keyed
// "<table>.<column>" and valued with the constraint's normalized text.
func parseChecks(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range reCreateTable.FindAllStringSubmatch(migrationSQL(t), -1) {
		table, body := m[1], m[2]
		for _, clause := range balancedChecks(body) {
			col := checkColumn(clause)
			if col == "" {
				t.Fatalf("cannot tell which column %s's CHECK (%s) governs", table, clause)
			}
			key := table + "." + col
			if prev, dup := out[key]; dup {
				t.Fatalf("%s has two CHECK constraints: %q and %q", key, prev, clause)
			}
			out[key] = clause
		}
	}
	return out
}

// parseEnums narrows parseChecks to the membership tests over string literals —
// the closed enums — and returns their members in declaration order.
func parseEnums(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for key, clause := range parseChecks(t) {
		m := reEnumClause.FindStringSubmatch(clause)
		if m == nil {
			continue
		}
		members := reMember.FindAllStringSubmatch(m[2], -1)
		if len(members) == 0 {
			continue // an integer membership test like `polkit_ok IN (0,1)`
		}
		vals := make([]string, len(members))
		for i, mm := range members {
			vals[i] = mm[1]
		}
		out[key] = vals
	}
	return out
}

// balancedChecks pulls the text inside every `CHECK ( … )` in a table body,
// counting parentheses so a nested call or a member list cannot end the clause
// early, and collapsing whitespace so a constraint wrapped over four lines reads
// as one.
func balancedChecks(body string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(body[i:], "CHECK (")
		if j < 0 {
			return out
		}
		open := i + j + len("CHECK (") - 1
		depth := 0
		for k := open; k < len(body); k++ {
			switch body[k] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					out = append(out, strings.Join(strings.Fields(body[open+1:k]), " "))
					i = k
					goto next
				}
			}
		}
		return out
	next:
	}
}

// checkColumn names the column a CHECK governs: the first identifier in the
// clause that is not one of the SQL functions a constraint may open with.
func checkColumn(clause string) string {
	for _, word := range regexp.MustCompile(`[a-z_][a-z0-9_]*`).FindAllString(clause, -1) {
		if !checkFunctions[word] {
			return word
		}
	}
	return ""
}

// scanStrings runs a single-column query and returns every value.
func scanStrings(t *testing.T, s *Store, q string) []string {
	t.Helper()
	rows, err := s.RO.Query(q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
