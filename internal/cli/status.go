package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/buildinfo"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/settings"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// `llamaman status` (DESIGN section 11.3).
//
// It works with the daemon down, reads the database READ-ONLY and systemd
// directly, and creates nothing — no database, no `-wal`/`-shm` sidecar, no
// `secret.key`, no directory. A root-created file under the state directory is a
// file the service identity can never write again, which is why §11.3 states that
// rule once for every root-invocable subcommand and why a CI test asserts it with
// a directory diff.
//
// Exit codes are a contract: 0 running, 1 not running (or not yet initialized),
// 2 database present but unreadable.

// StatusJSON is the `--json` body. It is the same content as the text form, per
// §11.3 ("`--json` emits the same content for scripts"), which is why both are
// rendered from this one struct rather than printed twice.
type StatusJSON struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Running bool   `json:"running"`
	PID     *int   `json:"pid"`
	// UptimeSec is measured from `runtime_info.boot_at`, and is null when no
	// daemon is running.
	UptimeSec *int64 `json:"uptime_sec"`

	UI       *StatusUIJSON       `json:"ui"`
	Database *StatusDatabaseJSON `json:"database"`
	Identity *StatusIdentityJSON `json:"identity"`
	Jobs     *StatusJobsJSON     `json:"jobs"`
	Setup    StatusSetupJSON     `json:"setup"`
}

// StatusUIJSON is the management listener, as this boot resolved it.
type StatusUIJSON struct {
	URL string `json:"url"`
	// Desired is `ui.port_desired`; Actual is the port the walk landed on
	// (§11.1 step 7). They differ when the desired port was taken, and printing
	// both is the whole point.
	Desired *int   `json:"desired_port"`
	Actual  *int   `json:"actual_port"`
	Bind    string `json:"bind"`
}

// StatusDatabaseJSON describes the file itself.
type StatusDatabaseJSON struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"size_bytes"`
	SchemaVersion int    `json:"schema_version"`
	// Integrity is "ok" or the first line SQLite complained with.
	Integrity string `json:"integrity"`
}

// StatusIdentityJSON is who owns this installation and how it talks to systemd.
type StatusIdentityJSON struct {
	Owner string  `json:"owner"`
	UID   int     `json:"uid"`
	Scope *string `json:"systemd_scope"`
	// Control is `runtime_info.systemd_control`, and is null until the boot
	// probe of §11.1 step 6 lands — a fact that has not been learned is null
	// rather than a zero that reads as an answer (F14).
	Control  *string `json:"systemd_control"`
	StateDir string  `json:"state_dir"`
}

// StatusJobsJSON is the job queue at a glance.
type StatusJobsJSON struct {
	Running int `json:"running"`
	Queued  int `json:"queued"`
}

// StatusSetupJSON is §11.3's setup block, verbatim: the token comes from
// `<state_dir>/setup-token` (§2.2a), which is the only place a plaintext token
// ever exists — `setup_claim` holds a sha256 and could never produce it.
type StatusSetupJSON struct {
	Complete bool `json:"complete"`
	Claimed  bool `json:"claimed"`
	// Token is null when the file is unreadable by this uid, in which case
	// TokenHint says who to run as.
	Token     *string `json:"token"`
	TokenHint *string `json:"token_hint"`
	URL       *string `json:"url"`
}

// Status prints the health of this installation — units, instances, database,
// disk — for a human or, with --json, for a script (DESIGN section 11.3).
func Status(env Env, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "emit the same content as JSON, for scripts")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman status [--json]\n\n")
		fmt.Fprintf(env.Stderr, "Works with the daemon down. Reads the database read-only and creates nothing.\n")
		fmt.Fprintf(env.Stderr, "Exit codes: 0 running, 1 not running or not initialized, 2 database unreadable.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := collectStatus(context.Background(), env)
	if err != nil {
		var unreadable *dbUnreadableError
		if errors.As(err, &unreadable) {
			fmt.Fprintf(env.Stderr, "llamaman status: %v\n", unreadable)
			return &ExitError{Code: 2, Err: unreadable}
		}
		var uninit *notInitializedError
		if errors.As(err, &uninit) {
			if *asJSON {
				_ = writeJSON(env, StatusJSON{
					Version: buildinfo.Version, Commit: buildinfo.Commit,
					Setup: StatusSetupJSON{},
				})
			} else {
				fmt.Fprintf(env.Stdout, "llamaman %s (commit %s) — not initialized: the daemon has not run yet\n",
					buildinfo.Version, buildinfo.Commit)
				fmt.Fprintf(env.Stdout, "  Database      %s (absent)\n", uninit.path)
			}
			return &ExitError{Code: 1, Err: uninit}
		}
		fmt.Fprintf(env.Stderr, "llamaman status: %v\n", err)
		return err
	}

	if *asJSON {
		if err := writeJSON(env, st); err != nil {
			return err
		}
	} else {
		printStatus(env, st)
	}
	if !st.Running {
		return &ExitError{Code: 1, Err: errors.New("the daemon is not running")}
	}
	return nil
}

// notInitializedError is §11.3's "file absent" case: no open is attempted at all.
type notInitializedError struct{ path string }

func (e *notInitializedError) Error() string {
	return "not initialized — the daemon has not run yet (no " + e.path + ")"
}

// dbUnreadableError is §11.3's second case, and its exit status 2.
type dbUnreadableError struct {
	path     string
	identity string
	cause    error
}

func (e *dbUnreadableError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s could not be read: %v", e.path, e.cause)
	}
	return fmt.Sprintf(
		"database present but not readable without creating WAL sidecars — run as %s", e.identity)
}

func (e *dbUnreadableError) Unwrap() error { return e.cause }

func collectStatus(ctx context.Context, env Env) (StatusJSON, error) {
	p := resolvePaths(env)
	access, owner, err := classify(p)
	if err != nil {
		return StatusJSON{}, err
	}
	switch access {
	case dbAbsent:
		return StatusJSON{}, &notInitializedError{path: p.DBPath}
	case dbSidecarsMissing:
		return StatusJSON{}, &dbUnreadableError{path: p.DBPath, identity: identityName(owner)}
	}

	st, err := store.OpenReadOnly(ctx, p.DBPath)
	if err != nil {
		return StatusJSON{}, &dbUnreadableError{path: p.DBPath, identity: identityName(owner), cause: err}
	}
	defer st.Close()

	out := StatusJSON{Version: buildinfo.Version, Commit: buildinfo.Commit}
	if pid, running := daemonPID(p); running {
		out.Running, out.PID = true, &pid
	}

	var (
		ri        model.RuntimeInfo
		haveRI    bool
		claim     model.SetupClaim
		haveRow   bool
		steps     []model.WizardStepRow
		integrity = "ok"
		schema    int
		running   int
		queued    int
	)
	err = st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := st.IntegrityCheckRead(ctx, tx, store.DefaultIntegrityMaxErrors); err != nil {
			var bad *store.IntegrityError
			if errors.As(err, &bad) {
				integrity = bad.Lines[0]
			} else {
				return err
			}
		}
		v, err := st.SchemaVersion(ctx, tx)
		if err != nil {
			return err
		}
		schema = v

		r, err := st.RuntimeInfo(ctx, tx)
		switch {
		case err == nil:
			ri, haveRI = r, true
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}

		c, err := st.SetupClaim(ctx, tx)
		switch {
		case err == nil:
			claim, haveRow = c, true
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}

		if steps, err = st.WizardSteps(ctx, tx); err != nil {
			return err
		}

		live, err := st.Jobs(ctx, tx, store.JobFilter{States: []model.JobState{model.JobRunning}})
		if err != nil {
			return err
		}
		running = len(live)
		waiting, err := st.Jobs(ctx, tx, store.JobFilter{States: []model.JobState{model.JobQueued}})
		if err != nil {
			return err
		}
		queued = len(waiting)
		return nil
	})
	if err != nil {
		return StatusJSON{}, &dbUnreadableError{path: p.DBPath, identity: identityName(owner), cause: err}
	}

	size := int64(0)
	if fi, statErr := os.Stat(p.DBPath); statErr == nil {
		size = fi.Size()
	}
	out.Database = &StatusDatabaseJSON{
		Path: p.DBPath, SizeBytes: size, SchemaVersion: schema, Integrity: integrity,
	}
	out.Jobs = &StatusJobsJSON{Running: running, Queued: queued}
	out.Identity = &StatusIdentityJSON{
		Owner: identityName(owner), UID: owner, StateDir: p.StateDir,
	}

	var uiURL string
	if haveRI {
		if ri.DaemonVersion != "" {
			// The row records the version of the daemon that last booted, which
			// is what a human wants to see when the installed binary has since
			// been replaced.
			out.Version = ri.DaemonVersion
			out.Commit = ri.DaemonCommit
		}
		if out.Running && ri.BootAt != nil {
			up := (env.now().UnixMilli() - *ri.BootAt) / 1000
			if up < 0 {
				up = 0
			}
			out.UptimeSec = &up
		}
		ui := &StatusUIJSON{}
		if ri.UIBindAddr != nil {
			ui.Bind = *ri.UIBindAddr
		}
		if ri.UIPort != nil {
			actual := int(*ri.UIPort)
			ui.Actual = &actual
		}
		if desired, ok := desiredPort(ctx, st); ok {
			ui.Desired = &desired
		}
		if ri.UIURLHint != nil {
			ui.URL = *ri.UIURLHint
		} else if ui.Actual != nil {
			ui.URL = "http://" + net.JoinHostPort("localhost", strconv.Itoa(*ui.Actual))
		}
		uiURL = ui.URL
		out.UI = ui

		if ri.SystemdScope != nil {
			s := string(*ri.SystemdScope)
			out.Identity.Scope = &s
		}
		if ri.SystemdControl != nil {
			c := string(*ri.SystemdControl)
			out.Identity.Control = &c
		}
	}

	out.Setup = setupBlock(p, claim, haveRow, steps, uiURL, owner)
	return out, nil
}

// setupBlock is §11.3's setup block and §2.2a step 3: before the claim is
// stamped, `status` prints the token — read FROM DISK, never from the database.
//
// When the file is unreadable by this uid the field is null and `token_hint`
// names who to run as, which is the honest answer: `status` never RECOVERS a
// token. Recovery is re-minting, and the only way to trigger it is to remove the
// file as root — the same privilege that could have read it.
func setupBlock(p paths, claim model.SetupClaim, haveRow bool,
	steps []model.WizardStepRow, uiURL string, owner int) StatusSetupJSON {

	out := StatusSetupJSON{Claimed: haveRow && claim.Claimed()}
	for _, s := range steps {
		if s.Step == model.StepDone && s.State == model.WizardComplete {
			out.Complete = true
		}
	}
	if out.Claimed {
		return out
	}

	if uiURL != "" {
		u := uiURL
		out.URL = &u
	}
	token, err := auth.ReadSetupTokenFile(auth.SetupTokenPath(p.StateDir))
	if err != nil {
		hint := "run as root or " + identityName(owner)
		if errors.Is(err, os.ErrNotExist) {
			hint = "no setup token on disk; restart the daemon to mint a new one"
		}
		out.TokenHint = &hint
		return out
	}
	out.Token = &token
	return out
}

func printStatus(env Env, st StatusJSON) {
	state := "not running"
	if st.Running {
		state = "running"
		if st.PID != nil {
			state += fmt.Sprintf(", pid %d", *st.PID)
		}
		if st.UptimeSec != nil {
			state += ", uptime " + humanDuration(time.Duration(*st.UptimeSec)*time.Second)
		}
	}
	fmt.Fprintf(env.Stdout, "llamaman %s (commit %s) — %s\n", st.Version, st.Commit, state)

	if ui := st.UI; ui != nil && ui.URL != "" {
		line := ui.URL
		switch {
		case ui.Desired != nil && ui.Actual != nil:
			line += fmt.Sprintf("   (desired %d, actual %d)", *ui.Desired, *ui.Actual)
		case ui.Actual != nil:
			line += fmt.Sprintf("   (port %d)", *ui.Actual)
		}
		fmt.Fprintf(env.Stdout, "  UI            %s\n", line)
	}
	if db := st.Database; db != nil {
		fmt.Fprintf(env.Stdout, "  Database      %s  (%s, schema v%d, integrity %s)\n",
			db.Path, humanBytes(db.SizeBytes), db.SchemaVersion, db.Integrity)
	}
	if id := st.Identity; id != nil {
		line := fmt.Sprintf("%s (uid %d)", id.Owner, id.UID)
		if id.Scope != nil {
			line += ", systemd scope: " + *id.Scope
		}
		if id.Control != nil {
			line += ", control: " + *id.Control
		}
		fmt.Fprintf(env.Stdout, "  Identity      %s\n", line)
		fmt.Fprintf(env.Stdout, "  State dir     %s\n", id.StateDir)
	}
	if j := st.Jobs; j != nil {
		fmt.Fprintf(env.Stdout, "  Jobs          %d running, %d queued\n", j.Running, j.Queued)
	}

	switch {
	case st.Setup.Complete:
		fmt.Fprintf(env.Stdout, "  Setup         complete\n")
	case st.Setup.Claimed:
		fmt.Fprintf(env.Stdout, "  Setup         claimed — the wizard has not been finished\n")
	default:
		url := ""
		if st.Setup.URL != nil {
			url = " — open " + *st.Setup.URL
		}
		fmt.Fprintf(env.Stdout, "  Setup         NOT COMPLETE%s\n", url)
		switch {
		case st.Setup.Token != nil:
			fmt.Fprintf(env.Stdout, "                setup token  %s   (not needed from this machine)\n",
				*st.Setup.Token)
		case st.Setup.TokenHint != nil:
			fmt.Fprintf(env.Stdout, "                setup token  unavailable (%s)\n", *st.Setup.TokenHint)
		}
	}
}

// desiredPort reads `ui.port_desired` the way the daemon does: the override row
// when one exists, and the registry's built-in default when it does not — which
// is the ordinary state, because §2.1's `settings` rows are ABSENT until somebody
// changes them.
//
// It is what makes §11.3's "(desired 5526, actual 5526)" line honest on a host
// where the walk had to move: the desired port is a SETTING, not the flag the
// unit carries, and `runtime_info.ui_port_flag` records only a divergence between
// the two (§11.1 step 6b).
func desiredPort(ctx context.Context, st *store.Store) (int, bool) {
	var out int
	err := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		row, err := st.Setting(ctx, tx, "ui.port_desired")
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(row.Value), &out)
	})
	if err == nil && out > 0 {
		return out, true
	}
	if def, ok := settings.NewRegistry().Lookup("ui.port_desired"); ok {
		if n, ok := def.Default.(int64); ok {
			return int(n), true
		}
	}
	return 0, false
}

func writeJSON(env Env, v any) error {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// humanBytes renders a byte count the way §11.3's sample output does. The wire
// form is always a plain number of bytes (§3's DTO conventions); this is the
// human form, and it exists only here.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanDuration renders an uptime as "3d 4h", "4h 12m" or "12m".
func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
