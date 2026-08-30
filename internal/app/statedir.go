package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"golang.org/x/sys/unix"
)

// DefaultStateDir is the DEFAULT state directory, not a constant. DESIGN
// section 11.1 step 1 is explicit about the difference: `systemd --user`
// resolves StateDirectory=llamaman to ~/.local/state/llamaman and exports
// $STATE_DIRECTORY accordingly, so a hardcoded literal disagrees with the
// manager, with the unit's own WorkingDirectory= and ReadWritePaths=, and with
// the D2 topology's judge path — the --user-units install simply could not
// start (D72).
const DefaultStateDir = "/var/lib/llamaman"

// LockFileName is the flock file of step 2. It lives inside the RESOLVED state
// directory, never at a literal /var/lib/llamaman/llamaman.lock: under
// `systemd --user` that literal path is neither created by
// `install.sh --user-units` nor writable by the service identity, so locking it
// first made the very first boot step fail on a topology that had not even
// started.
const LockFileName = "llamaman.lock"

// DatabaseFileName is <state_dir>/llamaman.db (section 2).
const DatabaseFileName = "llamaman.db"

// VersionsDirName is <state_dir>/versions, the root of section 6.1's on-disk
// layout: one directory per llama.cpp build, plus the `active` and `previous`
// symlinks. It is named here because two callers resolve a build directory —
// the launcher, through the symlink, and the instance service, through the
// row's `dir_name` — and a literal in either would be a second answer to a
// question D72 says has one.
const VersionsDirName = "versions"

// resolveScope implements step 1's scope rule: the flag when present, else a
// probe of the user bus.
//
// The probe itself is NOT implemented here. Section 11.1 step 1 says the scope
// is `user` when a connection to the user bus succeeds "*and* that manager
// reports `llamaman.service` as a known unit" — which is a D-Bus question, and
// D49's second invariant says only internal/systemd may ask one. So this
// function takes the answer as a callback: internal/systemd supplies it, and
// until it does the fallback is `system`, which section 11.1 already names as
// the answer "in every other case, including no bus at all".
func resolveScope(flag string, probe func() (model.SystemdScope, bool)) (model.SystemdScope, error) {
	if flag != "" {
		s := model.SystemdScope(flag)
		if !s.Valid() {
			return "", fmt.Errorf("--scope must be %q or %q, got %q",
				model.ScopeSystem, model.ScopeUser, flag)
		}
		return s, nil
	}
	if probe != nil {
		if s, ok := probe(); ok && s.Valid() {
			return s, nil
		}
	}
	return model.ScopeSystem, nil
}

// resolveStateDir implements D72's chain, in section 11.1 step 1's order:
//
//  1. $STATE_DIRECTORY — the service manager's own answer, correct in both
//     scopes. Set by systemd rather than by a user, so reading it is not a
//     configuration file and does not breach SPEC section 3.9. When it names
//     several paths, the first wins.
//  2. $XDG_STATE_HOME/llamaman, then $HOME/.local/state/llamaman, when the
//     scope is `user` or the process is not running under a service manager.
//  3. /var/lib/llamaman otherwise.
//
// override short-circuits the chain and exists for tests and for a developer
// running the daemon out of a checkout; it is not a flag, because SPEC section
// 3.9 forbids one.
func resolveStateDir(scope model.SystemdScope, override string, env func(string) string) string {
	if override != "" {
		return override
	}
	if v := env("STATE_DIRECTORY"); v != "" {
		// systemd separates several directories with ':'.
		if i := strings.IndexByte(v, ':'); i >= 0 {
			v = v[:i]
		}
		if v != "" {
			return v
		}
	}

	underManager := env("INVOCATION_ID") != ""
	if scope == model.ScopeUser || !underManager {
		if v := env("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, "llamaman")
		}
		if v := env("HOME"); v != "" {
			return filepath.Join(v, ".local", "state", "llamaman")
		}
	}
	return DefaultStateDir
}

// ResolveStateDir is the exported form of D72's chain, for the subcommands that
// must find the same directory the daemon did without being the daemon —
// `status`, `doctor` and `reset-password` (§11.3). It is the SAME function the
// boot sequence uses, deliberately: a second implementation in internal/cli would
// be a second answer to "where is the database", and the two would drift.
func ResolveStateDir(scope model.SystemdScope, override string, env func(string) string) string {
	if env == nil {
		env = os.Getenv
	}
	return resolveStateDir(scope, override, env)
}

// StateDirCandidates returns every directory D72's chain can resolve to, in
// order, with duplicates removed.
//
// §11.1 step 1 asks for exactly this list in one place: "a doctor check asserts
// the resolved directory is writable by the service identity and reports all
// three candidates when it is not". It is also what a CLI run by a human needs,
// because a hand-run `llamaman status` has neither $STATE_DIRECTORY nor
// $INVOCATION_ID and so cannot tell a system install from a user one by the
// environment alone — it looks for the database in each candidate instead.
func StateDirCandidates(env func(string) string) []string {
	if env == nil {
		env = os.Getenv
	}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, seen := range out {
			if seen == p {
				return
			}
		}
		out = append(out, p)
	}

	if v := env("STATE_DIRECTORY"); v != "" {
		if i := strings.IndexByte(v, ':'); i >= 0 {
			v = v[:i]
		}
		add(v)
	}
	if v := env("XDG_STATE_HOME"); v != "" {
		add(filepath.Join(v, "llamaman"))
	}
	if v := env("HOME"); v != "" {
		add(filepath.Join(v, ".local", "state", "llamaman"))
	}
	add(DefaultStateDir)
	return out
}

// LockedError is the F11 failure of step 2: a second daemon on the same state
// directory. cmd/llamaman turns it into exit status 70, which is what the
// design promises so that a hand-run `llamaman serve` can never race the unit.
type LockedError struct {
	Path string
	// PID is the holder, when the kernel would tell us. Zero when it would
	// not — the lock is still held either way, and refusing to start is still
	// the right answer.
	PID int
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another llamaman is already running (pid %d) and holds %s", e.PID, e.Path)
	}
	return fmt.Sprintf("another llamaman is already running and holds %s", e.Path)
}

// lockStateDir takes the exclusive advisory lock of step 2 and returns a
// release func.
//
// The lock is NON-blocking on purpose: a second daemon must fail immediately
// with F11's message naming the holder, not wait behind the first one until
// TimeoutStartSec= kills it with no explanation.
func lockStateDir(dir string) (release func() error, err error) {
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder := lockHolder(f)
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &LockedError{Path: path, PID: holder}
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// Record the holder so the NEXT daemon can name it. The kernel will not:
	// F_GETLK reports POSIX record locks, and flock(2) locks are a separate
	// mechanism it does not always answer for — which is why F11's "a message
	// naming the holding PID" is served from the file's own contents rather
	// than from an fcntl the answer may not be in.
	//
	// The file is only ever read by a process that failed to take the lock, so
	// a stale PID cannot be read as a live one: whoever holds the lock wrote
	// it, and the lock dies with them.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}

	return func() error {
		// Closing the descriptor releases the flock; unlocking first makes the
		// intent explicit and the ordering deterministic.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return f.Close()
	}, nil
}

// lockHolder reads the PID the holding daemon wrote into the lock file. Zero
// means the file said nothing usable, which is not a reason to start anyway —
// the lock is held either way, and refusing is still the right answer.
func lockHolder(f *os.File) int {
	var buf [32]byte
	n, err := f.ReadAt(buf[:], 0)
	if n == 0 && err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// StateDirChildren is section 6.1's layout under the state directory, and it is
// the same list `install.sh` creates as LM_STATE_CHILDREN.
//
// The two must agree, and the reason they have to BOTH exist is that only one of
// them runs on any given host: the installer creates them at install time, and
// this list creates them for the hand-run case and for a first boot on a host
// whose units have not been installed yet — which is precisely what
// ensureStateDir's own comment already claimed to cover.
//
// It did not, and the gap was not cosmetic. `GET /api/v1/llamacpp/plan` statfs's
// `<state_dir>/versions` to answer "is there room to install", so on a host the
// installer had not touched the probe failed with ENOENT, the plan rendered
// `free_bytes: 0` / `can_proceed: false`, and the wizard disabled the one button
// that finishes setup. Creating the directory the daemon is about to write into
// is the fix; reporting an unmeasurable probe honestly (see internal/llamacpp's
// PlanReport.FreeSpaceKnown) is the other half.
var StateDirChildren = []string{
	"versions", "src", "build", "logs", "db-backups", "update", "tmp",
}

// ensureStateDir creates the state directory and section 6.1's children if they
// are absent, with the 0750 mode section 2 specifies for the directory that
// holds a 0600 database.
//
// A child that cannot be created is NOT fatal. The units guarantee the parent
// exists (StateDirectory=llamaman), and a daemon that refused to start because
// it could not pre-create `db-backups/` would turn a cosmetic problem into an
// outage; the subsystem that needs a directory creates it again at the moment it
// writes, and reports its own failure with the context to act on.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}
	for _, child := range StateDirChildren {
		if err := os.MkdirAll(filepath.Join(dir, child), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(dir, child), err)
		}
	}
	return nil
}

// enforceDBMode is section 11.1 step 3's "enforce mode 0600" on the database
// file. A database created by an earlier release, restored from a backup, or
// copied by an operator can arrive with a wider mode; it holds session secrets
// and the sealed HF token.
func enforceDBMode(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() == 0o600 {
		return nil
	}
	return os.Chmod(path, 0o600)
}
