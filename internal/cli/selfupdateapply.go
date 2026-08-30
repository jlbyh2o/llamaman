package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/selfupdate"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// `llamaman selfupdate-apply --scope system` — the root oneshot of DESIGN
// section 12.2, started by `llamaman-selfupdate.service` and by nothing else.
//
// One branch and one refusal. Section 12.2 step 0 parses `--scope`, reads
// `update/pending`, cross-checks its `binary_path` against this process's own
// os.Executable(), re-verifies the tarball's sha256 and its ed25519 signature
// against the compiled-in key — never trusting the unprivileged staging step,
// because this is a genuine privilege boundary — and checks `<prefix>` for room.
//
// **Any failure writes nothing, deletes nothing, STOPS NOTHING** (this actor
// never stops a unit at all), logs one structured journald line and exits
// non-zero. The daemon's section 12.3 gate is what raises the notification,
// since this process must never open `llamaman.db` (section 11.3).
//
// It refuses to run in user scope, where the daemon performs the same sequence
// in process (section 5.2a item 2).

// SelfupdateApply is that actor.
func SelfupdateApply(env Env, args []string) error {
	rest, err := unitOnly(env, "selfupdate-apply", args)
	if err != nil {
		return err
	}

	scope, err := parseScopeFlag("selfupdate-apply", env, rest)
	if err != nil {
		// A missing or unparsable --scope is a REFUSAL, not a guess. The actor
		// leaves everything in place and exits non-zero; the repair line reaches
		// the human through the F24 card the daemon raises, which carries this
		// unit's journal tail (D77).
		fmt.Fprintf(env.Stderr, "llamaman selfupdate-apply: %v\n", err)
		fmt.Fprintf(env.Stderr, "repair: %s\n", repairUnitsLine)
		return err
	}

	log := actorLogger(env)

	// The one refusal this ENTRY POINT owns, and the reason it is here rather
	// than inside selfupdate.Apply: Apply is section 12.2's swap sequence, and
	// section 5.2a item 2 has the user-scope daemon run exactly that sequence in
	// process. What must not exist in user scope is this actor — install-units
	// writes no `llamaman-selfupdate.service` there, so being summoned at all
	// means something is wrong with the installation.
	if scope == model.ScopeUser {
		log.Error("self-update swap refused",
			"error", selfupdate.ErrUserScope, "scope", string(scope), "touched", "nothing")
		fmt.Fprintf(env.Stderr, "llamaman selfupdate-apply: %v\n", selfupdate.ErrUserScope)
		fmt.Fprintf(env.Stderr, "repair: %s\n", repairUnitsLine)
		return selfupdate.ErrUserScope
	}

	stateDir := app.ResolveStateDir(scope, "", env.getenv())

	res, err := selfupdate.Apply(context.Background(), selfupdate.ApplyOptions{
		Scope:   scope,
		Layout:  selfupdate.Layout{StateDir: stateDir},
		Restart: execRestarter{scope: scope},
		Log:     log,
	})
	if err != nil {
		// One structured line, and nothing else: no undo, no stop, no database.
		log.Error("self-update swap refused",
			"error", err,
			"scope", string(scope),
			"state_dir", stateDir,
			"touched", "nothing")
		fmt.Fprintf(env.Stderr, "llamaman selfupdate-apply: %v\n", err)
		return err
	}

	log.Info("self-update swap complete",
		"from_version", res.FromVersion, "target_version", res.TargetVersion,
		"retained_sha256", res.RetainedSHA256, "installed_sha256", res.InstalledSHA256,
		"restarted", res.Restarted)
	return nil
}

// repairUnitsLine is what a refusal prints. The identity is left literal because
// it is an installer-time decision this process must not guess at.
const repairUnitsLine = "sudo llamaman install-units --identity <user>"

// ErrScopeRequired is DESIGN section 12.2's "a missing or unparsable `--scope`
// is a refusal, not a guess".
//
// Both privileged actors run with the daemon stopped and can learn the topology
// no other way: they never open the database, `runtime_info.systemd_scope` is
// unreachable to them, section 11.1's bus probe would interrogate the very unit
// they are about to restart, and section 5.4 rejects the euid as a signal. So
// the argument install-units renders into the unit is the whole channel, and its
// absence leaves everything in place, logs one line and exits non-zero.
var ErrScopeRequired = errors.New("--scope is required and was not given: " +
	"this actor runs with the daemon stopped and can learn the topology no other way")

// parseScopeFlag reads the `--scope` argument install-units renders into the
// unit. It is deliberately strict: this argument is how a privileged actor
// learns the topology, it can learn it no other way (it never opens the
// database), and section 12.2 requires a missing or unparsable value to be a
// refusal rather than a default.
func parseScopeFlag(name string, env Env, args []string) (model.SystemdScope, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	scope := fs.String("scope", "", "systemd scope this actor runs under: system|user (written by install-units)")
	fs.Usage = unitOnlyUsage(env, name, "[--force] --scope system|user")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *scope == "" {
		return "", ErrScopeRequired
	}
	s := model.SystemdScope(*scope)
	if !s.Valid() {
		return "", fmt.Errorf("--scope must be %q or %q, got %q",
			model.ScopeSystem, model.ScopeUser, *scope)
	}
	return s, nil
}

// actorLogger is the structured journald line the two privileged actors leave
// behind, and it is the ONLY thing they leave behind: they write no marker, no
// row and no notification, because neither may open `llamaman.db` (§11.3). It
// goes to stderr, which systemd captures into the journal for a oneshot, and the
// F24 card the daemon raises carries that tail (D77).
func actorLogger(env Env) *slog.Logger {
	return slog.New(slog.NewTextHandler(env.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// execRestarter is section 12.2 step 4: `systemctl restart --no-block
// llamaman.service`.
//
// Exec rather than the bus, and `--no-block` rather than a wait, for the reason
// section 5.3 gives: this call ends by restarting the very process that would be
// waiting on the job. The path comes from systemd.SystemctlPath(), the one
// producer of a systemctl path in this design — never a PATH search, because a
// component that has to work when everything else is broken must not depend on
// the environment it inherits.
type execRestarter struct{ scope model.SystemdScope }

func (r execRestarter) RestartNoBlock(ctx context.Context, unit string) error {
	bin, err := systemd.SystemctlPath()
	if err != nil {
		return err
	}
	args := []string{}
	if r.scope == model.ScopeUser {
		args = append(args, "--user")
	}
	args = append(args, "restart", "--no-block", unit)
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", bin, args, err, string(out))
	}
	return nil
}
