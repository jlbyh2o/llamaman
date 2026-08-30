package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/systemd"
)

// The judge (DESIGN section 12.2): three checks and a rename.
//
// This is `llamaman update-verify --scope system|user`, and the binary it runs
// out of is `<prefix>/llamaman.prev` — the RETAINED PREVIOUS binary, never the
// installed one (D13). A judge that is itself the binary under judgment cannot
// run when the verdict is "it does not start".
//
// Its trigger is `OnFailure=llamaman-update-verify.service` on
// `llamaman.service` (D88), and its arming logic is entirely its unit's two
// `ConditionPathExists=` lines: `<prefix>/llamaman.prev` and
// `update/pending`. That is why this body can be three checks and a rename, and
// why it needs no timer, no clock and no deadline constant compiled into two
// binaries.
//
// What it deliberately does NOT do, each because the simpler protocol does not
// need it:
//
//   - It never stops anything. A unit in `failed` has no process left under it.
//   - It never touches the database, not even to read it: a root process that
//     opens llamaman.db creates a root-owned `-wal`/`-shm` beside it, which is a
//     database the service identity can never write again (section 11.3). This is
//     why it cannot be given a "refuse when the schema is ahead" check, and why
//     that question is answered by the daemon instead, at the one moment it has
//     the answer (D92).
//   - It never writes a marker and never notifies. It states facts in the
//     journal; the daemon owns the `notifications` row.

// VerifyOptions is what the judge needs. Everything is injectable because the
// revert's trigger truth table is a six-row test (section 15) and five of those
// rows are systemd states a fake has to be able to produce.
type VerifyOptions struct {
	// Scope comes from the unit — install-units renders it in BOTH topologies —
	// and selects only whether `systemctl` is addressed with `--user`. Because
	// this ExecStart runs the PREVIOUS binary, the argument is frozen across
	// versions in both directions. Missing or unparsable is a refusal, not a
	// guess.
	Scope model.SystemdScope

	// Layout supplies `<prefix>`. Prefix empty resolves it from this process's
	// own executable, which for the judge is `<prefix>/llamaman.prev` and
	// therefore already the right directory.
	Layout Layout

	// SelfExe is the path this process's own image is read from. Empty uses
	// /proc/self/exe, which is what check 1 fstats in production; a test points
	// it at a file it can control the mode of.
	SelfExe string

	// IsActive runs check 2. Nil uses the real exec of
	// `<systemd.SystemctlPath()> [--user] is-active llamaman.service`.
	//
	// It returns the trimmed STDOUT and an error only when the command could not
	// be run at all — never for a non-zero exit, because `is-active` exits 3 for
	// any unit that is not active while printing the state on stdout, and
	// treating that as an error inverts this check on the one input that matters.
	IsActive func(ctx context.Context, scope model.SystemdScope) (string, error)

	// Log receives the one structured line every outcome produces. Nil uses
	// slog.Default.
	Log *slog.Logger
}

// Verdict is what one judge run decided, for the journal line and for the test
// that drives all six rows of the trigger truth table.
type Verdict struct {
	// UnitState is what `is-active` printed on stdout, or "" when the exec
	// failed outright.
	UnitState string
	// Reverted reports whether the rename happened. It is true for exactly one
	// UnitState — `failed` — and false for every other input including an exec
	// that could not run.
	Reverted bool
	// Reason names why nothing was done, when nothing was done.
	Reason string
}

// ErrJudgeRefused is check 1 failing: the two files disagree about their owner,
// or this process's own image is group- or world-writable. It is defense in
// depth — `<prefix>` is root-only in system scope, so a planted `llamaman.prev`
// is already impossible — but it is two fstats, and it is the check that would
// have mattered when this file lived in the service-identity-owned `update/`
// directory (D89).
var ErrJudgeRefused = errors.New("selfupdate: update-verify refuses: the retained binary and the installed one do not share an owner, or the retained one is writable by others")

// Verify is the judge's whole body.
//
// It returns an error only for a refusal — check 1, an unparsable scope, or a
// rename that failed. Every other outcome is a Verdict and a nil error, because
// "the unit is `active`, so this is not my business" is a successful run that
// did nothing, and exiting non-zero for it would make `OnFailure=` retry a judge
// that correctly declined.
func Verify(ctx context.Context, opts VerifyOptions) (Verdict, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if !opts.Scope.Valid() {
		return Verdict{}, fmt.Errorf("selfupdate: --scope must be %q or %q, got %q",
			model.ScopeSystem, model.ScopeUser, opts.Scope)
	}

	l := opts.Layout
	if l.Prefix == "" {
		prefix, err := ResolvePrefix()
		if err != nil {
			return Verdict{}, err
		}
		l.Prefix = prefix
	}

	// --- check 1: fstat its own image and `<prefix>/llamaman`.
	self := opts.SelfExe
	if self == "" {
		self = "/proc/self/exe"
	}
	if err := checkImages(self, l.InstalledPath()); err != nil {
		return Verdict{}, err
	}

	// --- check 2: ask systemd for the unit state, by exec, and read the verdict
	// from STDOUT. Only the trimmed literal `failed` authorizes step 3.
	isActive := opts.IsActive
	if isActive == nil {
		isActive = execIsActive
	}
	state, err := isActive(ctx, opts.Scope)
	if err != nil {
		// An exec that failed outright is not a licence to revert. The judge does
		// nothing, logs what happened and exits 0.
		v := Verdict{Reason: "could not ask systemd for the unit state: " + err.Error()}
		log.Warn("self-update revert: doing nothing", "unit", DaemonUnit, "reason", v.Reason)
		return v, nil
	}
	if state != "failed" {
		v := Verdict{UnitState: state, Reason: unitStateReason(state)}
		log.Info("self-update revert: doing nothing",
			"unit", DaemonUnit, "state", state, "reason", v.Reason)
		return v, nil
	}

	// --- step 3: one atomic rename, same directory, consuming the retained
	// binary.
	if err := os.Rename(l.RetainedPath(), l.InstalledPath()); err != nil {
		return Verdict{UnitState: state}, fmt.Errorf(
			"selfupdate: revert %s over %s: %w (do it by hand: sudo mv %s %s && sudo systemctl reset-failed %s && sudo systemctl start %s)",
			l.RetainedPath(), l.InstalledPath(), err,
			l.RetainedPath(), l.InstalledPath(), DaemonUnit, DaemonUnit)
	}
	if err := fsyncDir(l.Prefix); err != nil {
		return Verdict{UnitState: state, Reverted: true}, err
	}

	log.Warn("self-update reverted: the previous binary is installed again",
		"unit", DaemonUnit, "state", state,
		"binary", l.InstalledPath(), "retained_binary_consumed", l.RetainedPath(),
		"reason", "the update was still unconfirmed when the unit reached `failed`")
	return Verdict{UnitState: state, Reverted: true}, nil
}

// unitStateReason says, in one phrase, why a state that is not `failed` means
// the judge has nothing to do. The four systemd reports and the "something
// else" case each have their own sentence, because the journal line is the only
// thing a human has when this component ran and declined.
func unitStateReason(state string) string {
	switch state {
	case "active":
		return "a daemon is running, and this is not the judge's business"
	case "activating":
		return "a start is still in progress"
	case "inactive":
		return "a human or a shutdown stopped the service deliberately"
	case "deactivating":
		return "the service is stopping"
	default:
		return "systemd reported a state this judge does not act on"
	}
}

// checkImages is check 1. Both files must be owned by the same uid, and this
// process's own image must not be group- or world-writable.
func checkImages(selfExe, installed string) error {
	selfInfo, err := os.Stat(selfExe)
	if err != nil {
		return fmt.Errorf("selfupdate: stat %s: %w", selfExe, err)
	}
	installedInfo, err := os.Stat(installed)
	if err != nil {
		return fmt.Errorf("selfupdate: stat %s: %w", installed, err)
	}

	if selfInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is mode %04o", ErrJudgeRefused, selfExe, selfInfo.Mode().Perm())
	}

	selfStat, ok1 := selfInfo.Sys().(*syscall.Stat_t)
	installedStat, ok2 := installedInfo.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		// A filesystem that does not report ownership. The mode check above has
		// already run, and refusing here would strand a host on a platform this
		// design supports for a fact it cannot learn.
		return nil
	}
	if selfStat.Uid != installedStat.Uid {
		return fmt.Errorf("%w: %s is owned by uid %d, %s by uid %d",
			ErrJudgeRefused, selfExe, selfStat.Uid, installed, installedStat.Uid)
	}
	return nil
}

// execIsActive runs check 2 for real.
//
// Exec rather than the bus, because this component runs when the daemon is gone
// and must carry no client library, no authorization step and no assumption
// about which bus exists. And never a PATH search: systemd.SystemctlPath() is
// the ONLY producer of a systemctl path in this design, shared with the
// install-units renderer and section 5.4a's drift check, so the three cannot
// disagree about which binary they mean.
func execIsActive(ctx context.Context, scope model.SystemdScope) (string, error) {
	bin, err := systemd.SystemctlPath()
	if err != nil {
		return "", err
	}
	args := []string{}
	if scope == model.ScopeUser {
		args = append(args, "--user")
	}
	args = append(args, "is-active", DaemonUnit)

	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	// The exit status is NOT the answer: `is-active` exits 3 for any unit that
	// is not active while printing the state on stdout. Only a failure to run
	// the command at all is an error here.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", err
	}
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		return "", err
	}
	return state, nil
}
