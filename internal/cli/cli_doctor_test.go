package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/store"
)

func runDoctorJSON(t *testing.T, env Env, out interface{ String() string }, args ...string) DoctorJSON {
	t.Helper()
	_ = Doctor(env, append([]string{"--format", "json"}, args...))

	var report DoctorJSON
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("decode the doctor report: %v\n%s", err, out.String())
	}
	return report
}

func byName(report DoctorJSON) map[string]Check {
	m := make(map[string]Check, len(report.Checks))
	for _, c := range report.Checks {
		m[c.Name] = c
	}
	return m
}

// TestDoctorWithoutADatabase is section 11.3's own sentence: DB-dependent checks
// are SKIPPED, not failed, when the database does not exist — that is the state
// install.sh step 8 runs in, and it is a normal, successful outcome.
func TestDoctorWithoutADatabase(t *testing.T) {
	t.Parallel()

	env, out, _, _ := testEnv(t)
	if err := Doctor(env, []string{"--format", "json"}); err != nil {
		t.Fatalf("Doctor on a host that has never run: %v", err)
	}

	var report DoctorJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode the doctor report: %v\n%s", err, out.String())
	}
	checks := byName(report)

	for _, name := range []string{"database", "db-integrity", "schema-version"} {
		c, ok := checks[name]
		if !ok {
			t.Fatalf("the report has no %q row; a check that did not run must still be reported", name)
		}
		if c.Status != CheckSkipped {
			t.Errorf("%s = %q, want skipped on a host with no database", name, c.Status)
		}
	}

	// The rows this release does not make yet are present and honest about it,
	// rather than absent — an absent row would let a bug report's reader believe
	// the binary looked and found nothing.
	for _, name := range []string{"toolchain", "gpu"} {
		if c, ok := checks[name]; !ok || c.Status != CheckNotImplemented {
			t.Errorf("%s = %+v, want a not_implemented row", name, c)
		}
	}

	// The three service-manager rows of §11.3 are answered, whatever this host
	// turns out to be: a machine with no systemd at all reports the F10 mode, a
	// machine with one that has never had `install-units` run against it
	// reports the two installed-service checks as skipped, and neither is a
	// failure. What must never happen is a row that was not looked at.
	for _, name := range []string{"systemd", "units", "polkit", "journal"} {
		c, ok := checks[name]
		if !ok {
			t.Errorf("the report has no %q row", name)
			continue
		}
		if c.Status == CheckNotImplemented {
			t.Errorf("%s = not_implemented; internal/systemd answers this check now", name)
		}
	}
}

// TestDoctorWithADatabase: the checks that exist today pass against a freshly
// migrated database, and the exit status stays 0.
func TestDoctorWithADatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	svc, err := auth.New(auth.Config{Repo: st, StateDir: stateDir, Now: env.now})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}

	if err := Doctor(env, []string{"--format", "json"}); err != nil {
		t.Fatalf("Doctor against a healthy installation: %v", err)
	}

	var report DoctorJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode the doctor report: %v\n%s", err, out.String())
	}
	if report.Failed != 0 {
		t.Fatalf("%d check(s) failed against a healthy installation: %+v", report.Failed, report.Checks)
	}

	checks := byName(report)
	if c := checks["database"]; c.Status != CheckOK {
		t.Errorf("database = %+v, want ok", c)
	}
	if c := checks["db-integrity"]; c.Status != CheckOK {
		t.Errorf("db-integrity = %+v, want ok", c)
	}
	if c := checks["schema-version"]; c.Status != CheckOK {
		t.Errorf("schema-version = %+v, want ok (the database is current for this binary)", c)
	}
	if c := checks["setup-token"]; c.Status != CheckOK || !strings.Contains(c.Detail, "unclaimed") {
		t.Errorf("setup-token = %+v, want an ok row naming the unclaimed token", c)
	}
}

// TestDoctorSkipDB forces the skipped arm explicitly, which is what --skip-db is
// for.
func TestDoctorSkipDB(t *testing.T) {
	t.Parallel()

	env, out, _, stateDir := testEnv(t)
	initState(t, stateDir)

	report := runDoctorJSON(t, env, out, "--skip-db")
	checks := byName(report)
	for _, name := range []string{"database", "db-integrity", "schema-version", "ui-port"} {
		if c := checks[name]; c.Status != CheckSkipped {
			t.Errorf("%s = %+v, want skipped under --skip-db", name, c)
		}
	}
}

// TestDoctorFlagsAWideOpenSetupToken: a claim credential must be 0600, and the
// row that says otherwise carries the exact chmod that fixes it.
func TestDoctorFlagsAWideOpenSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	svc, err := auth.New(auth.Config{Repo: st, StateDir: stateDir, Now: env.now})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := svc.EnsureSetupToken(ctx); err != nil {
		t.Fatalf("EnsureSetupToken: %v", err)
	}
	if err := os.Chmod(auth.SetupTokenPath(stateDir), 0o644); err != nil {
		t.Fatalf("chmod the token file: %v", err)
	}

	err = Doctor(env, []string{"--format", "json"})
	code, ok := ExitCode(err)
	if !ok || code != 1 {
		t.Fatalf("Doctor exit = %v/%v, want 1 when a check fails", code, ok)
	}

	var report DoctorJSON
	if jsonErr := json.Unmarshal(out.Bytes(), &report); jsonErr != nil {
		t.Fatalf("decode the doctor report: %v\n%s", jsonErr, out.String())
	}
	c := byName(report)["setup-token"]
	if c.Status != CheckFail {
		t.Fatalf("setup-token = %+v, want fail on a world-readable token", c)
	}
	if !strings.Contains(c.Remediation, "chmod 0600") {
		t.Errorf("remediation = %q, want the chmod that fixes it", c.Remediation)
	}
}

// TestDoctorTextOutputAndBadFormat.
func TestDoctorTextOutput(t *testing.T) {
	t.Parallel()

	env, out, errOut, stateDir := testEnv(t)
	initState(t, stateDir)

	if err := Doctor(env, nil); err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	text := out.String()
	for _, want := range []string{"llamaman doctor", "state dir:", "not_implemented"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text report does not contain %q:\n%s", want, text)
		}
	}

	if err := Doctor(env, []string{"--format", "yaml"}); err == nil {
		t.Fatal("Doctor accepted an unknown --format")
	}
	if !strings.Contains(errOut.String(), "--format must be text or json") {
		t.Errorf("stderr does not explain the format: %s", errOut.String())
	}
}

// TestResetPasswordRefusesANonTerminal is section 11.3's rule: it "refuses on a
// non-TTY without --stdin", so a password can never be taken from a pipe by
// accident.
func TestResetPasswordRefusesANonTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, _, errOut, stateDir := testEnv(t)
	st := initState(t, stateDir)

	svc, err := auth.New(auth.Config{
		Repo: st, StateDir: stateDir, Now: env.now,
		Params: auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := svc.Claim(ctx, "the first password", "127.0.0.1", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := ResetPassword(env, nil); err == nil {
		t.Fatal("reset-password read a password from a non-terminal without --stdin")
	}
	if !strings.Contains(errOut.String(), "--stdin") {
		t.Errorf("stderr does not name --stdin: %s", errOut.String())
	}
}

// TestResetPasswordFromStdin walks the whole recovery: the operator who locked
// themselves out sets a new password from the host, every session is revoked, and
// the new password works.
func TestResetPasswordFromStdin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env, out, _, stateDir := testEnv(t)
	st := initState(t, stateDir)

	cheap := auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16}
	svc, err := auth.New(auth.Config{Repo: st, StateDir: stateDir, Now: env.now, Params: cheap})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if _, err := svc.Claim(ctx, "the first password", "127.0.0.1", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	live, err := svc.Login(ctx, "the first password", "127.0.0.1", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	env.Stdin = strings.NewReader("the recovered password\n")
	if err := ResetPassword(env, []string{"--stdin"}); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if !strings.Contains(out.String(), "reset") {
		t.Errorf("reset-password printed nothing about what it did: %s", out.String())
	}

	if _, err := svc.ResolveSession(ctx, live.SessionCookie, "127.0.0.1", ""); !errors.Is(err, auth.ErrNoSession) {
		t.Error("a session survived reset-password; section 11.3 says every session is deleted")
	}
	if _, err := svc.Login(ctx, "the recovered password", "127.0.0.1", ""); err != nil {
		t.Fatalf("the new password does not log in: %v", err)
	}
}

// TestResetPasswordRefusesAShortPassword: the same rule the API applies, applied
// where there is no API.
func TestResetPasswordRefusesAShortPassword(t *testing.T) {
	t.Parallel()

	env, _, _, stateDir := testEnv(t)
	initState(t, stateDir)

	env.Stdin = strings.NewReader("short\n")
	if err := ResetPassword(env, []string{"--stdin"}); err == nil {
		t.Fatal("reset-password accepted a password below the minimum length")
	}
}

// TestResetPasswordOnAnUninitializedHost exits 1 rather than creating a database
// — the state directory belongs to the daemon, and a CLI-created 0600 file there
// is a file the service identity may not own.
func TestResetPasswordOnAnUninitializedHost(t *testing.T) {
	t.Parallel()

	env, _, _, stateDir := testEnv(t)
	env.Stdin = strings.NewReader("a good password\n")

	err := ResetPassword(env, []string{"--stdin"})
	code, ok := ExitCode(err)
	if !ok || code != 1 {
		t.Fatalf("exit = %v/%v, want 1", code, ok)
	}
	if _, statErr := os.Stat(stateDir); statErr == nil {
		t.Fatal("reset-password created the state directory on a host that has never run")
	}
}

// TestOpenReadOnlyRefusesToCreate is the store-side half of the same promise:
// `mode=ro` means SQLite will not create the database, so a mistyped path fails
// instead of quietly making an empty one.
func TestOpenReadOnlyRefusesToCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := dir + "/does-not-exist.db"
	if _, err := store.OpenReadOnly(context.Background(), path); err == nil {
		t.Fatal("OpenReadOnly created a database that did not exist")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("OpenReadOnly left a file behind")
	}
}
