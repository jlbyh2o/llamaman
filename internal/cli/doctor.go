package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
	"github.com/jlbyh2o/llamaman/internal/store"
	"golang.org/x/sys/unix"
)

// `llamaman doctor` (DESIGN section 11.3): the thing to ask for in a bug report.
//
// It only ever REPORTS. Resolving `update/pending` is the daemon's gate and
// restoring a database is `restore-db`'s; nothing here writes, and — like
// `status` — nothing here creates a file under the state directory.
//
// §11.3 lists roughly twenty checks. The ones whose subsystems exist today are
// implemented; every other row is present and reports `not yet implemented`
// rather than being silently absent. That is the honest shape: a bug report that
// says "systemd: not yet implemented" tells the reader this binary never looked,
// while an absent row would let them believe it looked and found nothing.

// CheckStatus is one row's verdict.
type CheckStatus string

const (
	// CheckOK: the condition holds.
	CheckOK CheckStatus = "ok"
	// CheckWarn: it holds, but something about it deserves attention.
	CheckWarn CheckStatus = "warn"
	// CheckFail: it does not hold, and the row carries the remediation.
	CheckFail CheckStatus = "fail"
	// CheckSkipped: the check could not run — no database yet, or --skip-db.
	// §11.3 is explicit that DB checks are SKIPPED, not failed, when there is
	// no database: that is the state install.sh step 8 runs in and it is a
	// normal, successful outcome.
	CheckSkipped CheckStatus = "skipped"
	// CheckNotImplemented: this release does not make this check yet.
	CheckNotImplemented CheckStatus = "not_implemented"
)

// Check is one row of the report.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
	// Remediation is the exact command or action that fixes it, when there is
	// one. §17's remediation cards say the same things in the UI.
	Remediation string `json:"remediation,omitempty"`
}

// DoctorJSON is the `--format json` body.
type DoctorJSON struct {
	Version  string  `json:"version"`
	Commit   string  `json:"commit"`
	StateDir string  `json:"state_dir"`
	Checks   []Check `json:"checks"`
	// Failed is the number of `fail` rows, which is also the exit status's
	// input: doctor exits non-zero only when something is actually wrong.
	Failed int `json:"failed"`
}

// Doctor checks the host for the conditions this daemon needs and prints
// remediation for each one it fails (DESIGN sections 11.3 and 17).
func Doctor(env Env, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	format := fs.String("format", "text", "output format: text|json")
	skipDB := fs.Bool("skip-db", false, "do not open the database, even if one exists")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman doctor [--format text|json] [--skip-db]\n\n")
		fmt.Fprintf(env.Stderr, "Reports; never repairs. Creates nothing under the state directory.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(env.Stderr, "llamaman doctor: --format must be text or json, got %q\n", *format)
		return fmt.Errorf("unknown format %q", *format)
	}

	report := runDoctor(context.Background(), env, *skipDB)

	if *format == "json" {
		if err := writeJSON(env, report); err != nil {
			return err
		}
	} else {
		printDoctor(env, report)
	}
	if report.Failed > 0 {
		return &ExitError{Code: 1, Err: fmt.Errorf("%d check(s) failed", report.Failed)}
	}
	return nil
}

func runDoctor(ctx context.Context, env Env, skipDB bool) DoctorJSON {
	p := resolvePaths(env)
	report := DoctorJSON{
		Version:  buildinfo.Version,
		Commit:   buildinfo.Commit,
		StateDir: p.StateDir,
	}
	add := func(c Check) {
		if c.Status == CheckFail {
			report.Failed++
		}
		report.Checks = append(report.Checks, c)
	}

	add(checkStateDir(env, p))
	for _, c := range checkDatabase(ctx, env, p, skipDB) {
		add(c)
	}
	add(checkSetupToken(p))
	add(checkManagementPort(ctx, env, p, skipDB))
	for _, c := range checkSystemd(ctx, env, p) {
		add(c)
	}

	// The rows §11.3 asks for whose subsystems have not landed. Each names the
	// package that will answer it, so the list shrinks by deletion rather than
	// by rewriting.
	for _, row := range []struct{ name, detail string }{
		{"toolchain", "gcc, g++, cmake, ninja, make, git, nvcc, driver, glibc (internal/toolchain)"},
		{"gpu", "GPUs, driver and CUDA versions (internal/hw)"},
		{"hf-cache", "hub directory writability and symlink support (internal/hf/cache)"},
		{"self-update", "update/pending, llamaman.prev, and the OnFailure= revert (internal/selfupdate)"},
		{"binary-digest", "the installed binary against the running daemon's image (F25)"},
		{"start-limit", "whether this boot cleared its unit's start-limit counter (D93)"},
	} {
		add(Check{Name: row.name, Status: CheckNotImplemented, Detail: row.detail})
	}

	return report
}

// checkStateDir is §11.1 step 1's doctor check: the resolved directory must be
// writable by this identity, and when it is not, ALL THREE candidates are named
// — because the usual cause is that the units and the binary came from different
// installs, and the operator needs to see which directory each one meant.
func checkStateDir(env Env, p paths) Check {
	c := Check{Name: "state-dir", Detail: p.StateDir}

	fi, err := os.Stat(p.StateDir)
	if err != nil {
		// Not a failure: this is the host `install.sh` step 8 runs doctor on,
		// before the daemon has ever started. The unit's StateDirectory= — or
		// the boot sequence itself — creates it. What is worth saying is WHICH
		// directory the next boot will use, and what the other candidates were,
		// because a mismatch there is the usual sign that the units and the
		// binary came from different installs (§11.1 step 1).
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("%s does not exist yet; the next boot creates it (candidates: %s)",
			p.StateDir, strings.Join(app.StateDirCandidates(env.getenv()), ", "))
		return c
	}
	if !fi.IsDir() {
		c.Status = CheckFail
		c.Detail = p.StateDir + " is not a directory"
		return c
	}
	if err := unix.Access(p.StateDir, unix.W_OK); err != nil {
		c.Status = CheckFail
		c.Detail = fmt.Sprintf("%s is not writable by this identity (candidates: %s)",
			p.StateDir, strings.Join(app.StateDirCandidates(env.getenv()), ", "))
		c.Remediation = "run as the service identity, or `sudo chown -R <identity> " + p.StateDir + "`"
		return c
	}

	c.Status = CheckOK
	if perm := fi.Mode().Perm(); perm != 0o750 {
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("%s is mode %04o; section 2 specifies 0750 for the directory holding a 0600 database",
			p.StateDir, perm)
		c.Remediation = fmt.Sprintf("chmod 0750 %s", p.StateDir)
	}
	return c
}

// checkDatabase is the integrity, schema-version and migration set of §11.3.
//
// Every row here is SKIPPED rather than failed when there is no database, which
// §11.3 states outright, and --skip-db forces that explicitly.
func checkDatabase(ctx context.Context, env Env, p paths, skipDB bool) []Check {
	names := []string{"database", "db-integrity", "schema-version"}
	skip := func(reason string) []Check {
		out := make([]Check, 0, len(names))
		for _, n := range names {
			out = append(out, Check{Name: n, Status: CheckSkipped, Detail: reason})
		}
		return out
	}

	if skipDB {
		return skip("--skip-db was given")
	}
	access, owner, err := classify(p)
	if err != nil {
		return skip("could not stat " + p.DBPath + ": " + err.Error())
	}
	switch access {
	case dbAbsent:
		return skip("database not yet created (" + p.DBPath + ")")
	case dbSidecarsMissing:
		return []Check{{
			Name:        "database",
			Status:      CheckWarn,
			Detail:      "database present but not readable without creating WAL sidecars",
			Remediation: "run as " + identityName(owner),
		}, {
			Name: "db-integrity", Status: CheckSkipped, Detail: "the database could not be opened read-only",
		}, {
			Name: "schema-version", Status: CheckSkipped, Detail: "the database could not be opened read-only",
		}}
	}

	st, err := store.OpenReadOnly(ctx, p.DBPath)
	if err != nil {
		return []Check{{
			Name: "database", Status: CheckFail,
			Detail:      "could not open " + p.DBPath + " read-only: " + err.Error(),
			Remediation: "run as " + identityName(owner),
		}, {
			Name: "db-integrity", Status: CheckSkipped, Detail: "the database could not be opened",
		}, {
			Name: "schema-version", Status: CheckSkipped, Detail: "the database could not be opened",
		}}
	}
	defer st.Close()

	out := []Check{{Name: "database", Status: CheckOK, Detail: p.DBPath}}

	integrity := Check{Name: "db-integrity", Status: CheckOK, Detail: "ok"}
	schema := Check{Name: "schema-version"}
	err = st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := st.IntegrityCheckRead(ctx, tx, store.DefaultIntegrityMaxErrors); err != nil {
			integrity.Status = CheckFail
			integrity.Detail = err.Error()
			integrity.Remediation = "stop the daemon and restore the newest db-backups/ snapshot with `llamaman restore-db`"
		}

		version, err := st.SchemaVersion(ctx, tx)
		if err != nil {
			schema.Status = CheckFail
			schema.Detail = err.Error()
			return nil
		}
		embedded, err := store.Migrations()
		if err != nil {
			schema.Status = CheckFail
			schema.Detail = err.Error()
			return nil
		}
		highest := 0
		if len(embedded) > 0 {
			highest = embedded[len(embedded)-1].Version
		}
		switch {
		case version > highest:
			// The database was written by a newer release — the state a
			// downgrade leaves behind (§12.4, D90). §11.3 requires the FIVE
			// commands, never the restore-db line alone, which on its own is a
			// destructive no-op the newer binary migrates straight back.
			schema.Status = CheckFail
			schema.Detail = fmt.Sprintf(
				"the database is at v%d and this binary understands v%d: it was written by a newer release",
				version, highest)
			schema.Remediation = strings.Join([]string{
				"sudo systemctl stop llamaman.service",
				"install the previous binary at <prefix>/llamaman",
				"sudo llamaman restore-db <newest usable db-backups/ snapshot>",
				"sudo systemctl reset-failed llamaman.service",
				"sudo systemctl start llamaman.service",
			}, "; ")
		case version < highest:
			schema.Status = CheckWarn
			schema.Detail = fmt.Sprintf(
				"the database is at v%d and this binary carries migrations through v%d; the next boot will migrate",
				version, highest)
		default:
			schema.Status = CheckOK
			schema.Detail = fmt.Sprintf("v%d, current for this binary", version)
		}
		return nil
	})
	if err != nil {
		integrity.Status = CheckFail
		integrity.Detail = err.Error()
	}
	return append(out, integrity, schema)
}

// checkSetupToken reports whether this host has been claimed and whether the
// one-time token is where §2.2a says it should be, with the mode it says.
func checkSetupToken(p paths) Check {
	c := Check{Name: "setup-token"}
	path := auth.SetupTokenPath(p.StateDir)

	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		c.Status = CheckOK
		c.Detail = "no setup token on disk (normal once the host is claimed)"
		return c
	}
	if err != nil {
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("%s exists but could not be read: %v", path, err)
		c.Remediation = "run as root or the service identity"
		return c
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		c.Status = CheckFail
		c.Detail = fmt.Sprintf("%s is mode %04o; a claim credential must be 0600", path, perm)
		c.Remediation = "chmod 0600 " + path
		return c
	}
	c.Status = CheckOK
	c.Detail = "unclaimed; the one-time token is at " + path + " (0600) — `llamaman status` prints it"
	return c
}

// checkManagementPort answers the "ports" row of §11.3 for the one listener that
// exists today.
//
// It reads the port the walk ACTUALLY landed on and probes it. A daemon that is
// running is expected to hold it — that is the healthy answer, not a conflict —
// so the check reads as "in use by the running daemon" or "free", and only warns
// when the port is held while no daemon is running, which is the case that makes
// the next boot walk to a different port.
func checkManagementPort(ctx context.Context, env Env, p paths, skipDB bool) Check {
	c := Check{Name: "ui-port"}
	if skipDB || !p.Exists {
		c.Status = CheckSkipped
		c.Detail = "the port this daemon landed on is recorded in the database"
		return c
	}

	access, _, err := classify(p)
	if err != nil || access != dbReadable {
		c.Status = CheckSkipped
		c.Detail = "the database could not be opened read-only"
		return c
	}
	st, err := store.OpenReadOnly(ctx, p.DBPath)
	if err != nil {
		c.Status = CheckSkipped
		c.Detail = "the database could not be opened read-only"
		return c
	}
	defer st.Close()

	var ri model.RuntimeInfo
	if err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		ri, err = st.RuntimeInfo(ctx, tx)
		return err
	}); err != nil || ri.UIPort == nil {
		c.Status = CheckSkipped
		c.Detail = "no management port has been recorded yet"
		return c
	}

	port := int(*ri.UIPort)
	bind := ""
	if ri.UIBindAddr != nil {
		bind = *ri.UIBindAddr
	}
	_, running := daemonPID(p)
	free := netutil.Free(bind, port)

	switch {
	case running && !free:
		c.Status = CheckOK
		c.Detail = fmt.Sprintf("port %d on %s is held by the running daemon", port, bindLabel(bind))
	case running && free:
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("a daemon is running but port %d on %s is not bound", port, bindLabel(bind))
	case free:
		c.Status = CheckOK
		c.Detail = fmt.Sprintf("port %d on %s is free for the next boot", port, bindLabel(bind))
	default:
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("port %d on %s is held by another process; the next boot will walk past it",
			port, bindLabel(bind))
		c.Remediation = fmt.Sprintf("free the port, or set ui.port_desired in the UI (walk window: %d ports)",
			netutil.DefaultWindow)
	}
	return c
}

func bindLabel(bind string) string {
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return "all interfaces"
	default:
		return bind
	}
}

func printDoctor(env Env, r DoctorJSON) {
	fmt.Fprintf(env.Stdout, "llamaman doctor — %s (commit %s)\n", r.Version, r.Commit)
	fmt.Fprintf(env.Stdout, "state dir: %s\n\n", r.StateDir)

	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(env.Stdout, "  [%-15s] %-*s  %s\n", c.Status, width, c.Name, c.Detail)
		if c.Remediation != "" {
			fmt.Fprintf(env.Stdout, "  %17s %-*s  fix: %s\n", "", width, "", c.Remediation)
		}
	}

	fmt.Fprintln(env.Stdout)
	if r.Failed == 0 {
		fmt.Fprintf(env.Stdout, "no failures. Rows marked `not_implemented` are checks this release does not make yet.\n")
		return
	}
	fmt.Fprintf(env.Stdout, "%d check(s) failed.\n", r.Failed)
}
