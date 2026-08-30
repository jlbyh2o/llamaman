package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The CLI tests run against a real state directory and a real database, because
// the properties DESIGN section 11.3 states are properties of the FILESYSTEM: the
// database is found by walking D72's candidates, `status` and `doctor` must
// create nothing, and the setup token is read from a 0600 file rather than from
// the database.

// testEnv builds an Env whose environment points D72's chain at a temp
// directory, so no test touches a real installation and every test can run in
// parallel.
func testEnv(t *testing.T) (Env, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()

	root := t.TempDir()
	stateDir := filepath.Join(root, "llamaman")
	var out, errOut bytes.Buffer
	env := Env{
		Stdout: &out,
		Stderr: &errOut,
		Getenv: func(key string) string {
			switch key {
			case "XDG_STATE_HOME":
				return root
			default:
				return ""
			}
		},
		Now: func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) },
	}
	return env, &out, &errOut, stateDir
}

// initState creates the state directory and a migrated database, which is what a
// host looks like after one boot.
func initState(t *testing.T, stateDir string) *store.Store {
	t.Helper()
	ctx := context.Background()

	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(stateDir, "llamaman.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// TestStatusNotInitialized is section 11.3's first case: the file is absent, no
// open is attempted at all, and the exit status is 1. This is the state
// install.sh step 8 runs in.
func TestStatusNotInitialized(t *testing.T) {
	t.Parallel()

	env, out, _, stateDir := testEnv(t)
	err := Status(env, nil)

	code, ok := ExitCode(err)
	if !ok || code != 1 {
		t.Fatalf("exit = %v/%v, want 1", code, ok)
	}
	if !strings.Contains(out.String(), "not initialized") {
		t.Fatalf("output does not say the daemon has not run yet:\n%s", out.String())
	}
	if _, err := os.Stat(stateDir); err == nil {
		t.Fatal("status created the state directory; section 11.3 says it creates nothing")
	}
}

// TestStatusCreatesNothing is the rule section 11.3 states once for every
// root-invocable subcommand, asserted the way its CI test does: with a directory
// diff against a state directory this caller does not own the database in.
//
// The scenario that rule exists for is a ROOT `status` against a database owned
// by the service identity, which the section's own three-case table answers by
// REFUSING when the WAL sidecars are absent — a root-created `llamaman.db-shm`
// beside a 0600 database is a database the service identity can never write
// again. What this test can assert without a second uid is the half that needs no
// privilege: with no database at all, nothing is created — not the directory, not
// the database, not `secret.key`. The sidecar half is asserted by
// TestStatusOnlyEverTouchesSidecars below, which is the owner's case, and by the
// classify() table, which is the branch a differing uid takes.
func TestStatusCreatesNothing(t *testing.T) {
	t.Parallel()

	env, _, _, stateDir := testEnv(t)
	root := filepath.Dir(stateDir)

	before := listDir(t, root)
	_ = Status(env, nil)
	after := listDir(t, root)

	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("status created something with no installation present:\nbefore %v\nafter  %v",
			before, after)
	}
}

// TestStatusOnlyEverTouchesSidecars: reading an existing database may bring its
// WAL sidecars back, and section 11.3 tolerates that for the database's OWNER —
// the files it creates belong to the identity that already owns everything here.
// What must never appear is anything else.
func TestStatusOnlyEverTouchesSidecars(t *testing.T) {
	t.Parallel()

	env, _, _, stateDir := testEnv(t)
	st := initState(t, stateDir)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_ = Status(env, nil)

	allowed := map[string]bool{
		"llamaman.db": true, "llamaman.db-wal": true, "llamaman.db-shm": true,
	}
	for _, name := range listDir(t, stateDir) {
		if !allowed[name] {
			t.Errorf("status created %q under the state directory", name)
		}
	}
}

// TestStatusJSONReportsTheSetupToken is section 2.2a step 3 and section 11.3's
// setup block: before the claim is stamped, the token comes from the FILE.
func TestStatusJSONReportsTheSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	svc, err := auth.New(auth.Config{Repo: st, StateDir: stateDir, Now: env.now})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok, err := svc.EnsureSetupToken(ctx)
	if err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}

	if err := Status(env, []string{"--json"}); err == nil {
		t.Fatal("status exited 0 with no daemon running")
	}

	var body StatusJSON
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("decode --json: %v\n%s", err, out.String())
	}
	if body.Setup.Claimed || body.Setup.Complete {
		t.Fatalf("setup = %+v, want an unclaimed, incomplete host", body.Setup)
	}
	if body.Setup.Token == nil || *body.Setup.Token != tok.Token {
		t.Fatalf("setup.token = %v, want the token on disk", body.Setup.Token)
	}
	if body.Database == nil || body.Database.Integrity != "ok" {
		t.Fatalf("database = %+v, want an integrity of ok", body.Database)
	}
	if body.Running {
		t.Error("status reports a running daemon with no daemon running")
	}
}

// TestStatusAfterTheClaim: once the claim is stamped the token is gone and the
// block collapses.
func TestStatusAfterTheClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	svc, err := auth.New(auth.Config{
		Repo: st, StateDir: stateDir, Now: env.now,
		Params: auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	if _, err := svc.Claim(ctx, "a good password", "127.0.0.1", "go test"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	_ = Status(env, []string{"--json"})

	var body StatusJSON
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("decode --json: %v\n%s", err, out.String())
	}
	if !body.Setup.Claimed {
		t.Error("setup.claimed is false after a successful claim")
	}
	if body.Setup.Token != nil {
		t.Errorf("setup.token = %v after the claim; the file is unlinked by the burn", *body.Setup.Token)
	}
}

// TestStatusTextOutput checks the human form carries the same facts as --json,
// which is what section 11.3 means by "the same content".
func TestStatusTextOutput(t *testing.T) {
	t.Parallel()

	env, out, _, stateDir := testEnv(t)
	initState(t, stateDir)

	_ = Status(env, nil)
	text := out.String()

	for _, want := range []string{"llamaman", "Database", "Setup", "NOT COMPLETE", filepath.Join(stateDir, "llamaman.db")} {
		if !strings.Contains(text, want) {
			t.Errorf("status output does not mention %q:\n%s", want, text)
		}
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
