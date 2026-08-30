package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jlbyh2o/llamaman/internal/app"
	"github.com/jlbyh2o/llamaman/internal/model"
	"golang.org/x/sys/unix"
)

// Finding the installation from a shell (DESIGN section 11.3).
//
// A hand-run `llamaman status` has neither `$STATE_DIRECTORY` nor
// `$INVOCATION_ID`: systemd sets both, and nobody typing at a prompt has them. So
// the CLI cannot ask D72's chain for THE answer the way the daemon can — it walks
// the same candidates and takes the first that actually holds a database, which
// is the one fact that distinguishes a system install from a user one after the
// fact. When none does, the chain's own answer is used, and that is the state
// §11.3 calls "not initialized — the daemon has not run yet".

// paths is where this installation lives, as far as a CLI can tell.
type paths struct {
	// StateDir is the directory chosen; DBPath is `<StateDir>/llamaman.db`.
	StateDir string
	DBPath   string
	// Candidates is every directory D72's chain could have meant, for the
	// message a failed lookup prints.
	Candidates []string
	// Exists reports whether DBPath is a file that exists. §11.3's first case —
	// "file absent" — is a NORMAL, successful outcome for `doctor` and the state
	// `install.sh` step 8 runs in, so it is a field rather than an error.
	Exists bool
}

// resolvePaths walks the candidates and returns where this installation is.
func resolvePaths(env Env) paths {
	getenv := env.getenv()
	p := paths{Candidates: app.StateDirCandidates(getenv)}

	for _, dir := range p.Candidates {
		db := filepath.Join(dir, app.DatabaseFileName)
		if fi, err := os.Stat(db); err == nil && fi.Mode().IsRegular() {
			p.StateDir, p.DBPath, p.Exists = dir, db, true
			return p
		}
	}

	// No database anywhere: fall back to what the daemon WOULD choose, so the
	// "not initialized" message names the directory the next boot will use. The
	// scope passed is `system` because that is what §11.1 step 1's fallback
	// resolves to without a bus probe, and because the chain only consults it
	// when the environment already points somewhere else.
	p.StateDir = app.ResolveStateDir(model.ScopeSystem, "", getenv)
	p.DBPath = filepath.Join(p.StateDir, app.DatabaseFileName)
	return p
}

// dbAccess is §11.3's three-case table for a root-invocable subcommand that must
// create nothing.
type dbAccess int

const (
	// dbAbsent: the file does not exist. No open is attempted at all.
	dbAbsent dbAccess = iota
	// dbReadable: the sidecars exist, or this caller owns the database, so a
	// read-only open creates nothing.
	dbReadable
	// dbSidecarsMissing: the file is there, its `-shm` is not, and this caller
	// is not the owner. A read-only open of a WAL database whose `-shm` does not
	// exist would have to CREATE it, and a root-created sidecar beside a 0600
	// database is a database the service identity can never write again. This is
	// the one case where root deliberately does less than it could.
	dbSidecarsMissing
)

// classify decides which of §11.3's three cases this invocation is in, and
// returns the owner's uid for the message.
func classify(p paths) (dbAccess, int, error) {
	if !p.Exists {
		return dbAbsent, 0, nil
	}

	fi, err := os.Stat(p.DBPath)
	if err != nil {
		return dbAbsent, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Not Linux, or a filesystem that does not report ownership. Treating
		// it as readable is the honest default: the open below will fail with a
		// real error if it is not.
		return dbReadable, 0, nil
	}
	owner := int(st.Uid)
	if os.Geteuid() == owner {
		return dbReadable, owner, nil
	}

	// The daemon is running, or has run and left its WAL sidecars behind: they
	// are already there, already owned by the service identity, and reading
	// through them creates nothing.
	if _, err := os.Stat(p.DBPath + "-shm"); err == nil {
		return dbReadable, owner, nil
	}
	return dbSidecarsMissing, owner, nil
}

// identityName renders a uid as something an operator can act on: the account
// name when /etc/passwd knows it, the number otherwise.
func identityName(uid int) string {
	if name := lookupUsername(uid); name != "" {
		return name
	}
	return "uid " + strconv.Itoa(uid)
}

// lookupUsername reads /etc/passwd directly rather than using os/user, whose cgo
// path is unavailable in this binary (CGO_ENABLED=0 is a hard constraint,
// DESIGN section 16.1) and whose pure-Go fallback reads the same file anyway.
func lookupUsername(uid int) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == want {
			return fields[0]
		}
	}
	return ""
}

// daemonPID reports the pid of the daemon holding this state directory, and
// whether one is running.
//
// It reads the pid the boot sequence wrote into `llamaman.lock` and then asks the
// kernel whether that process is alive AND is a llamaman — the second half
// matters because a pid is reused, and "running" printed against somebody else's
// process would be worse than printing nothing.
func daemonPID(p paths) (int, bool) {
	b, err := os.ReadFile(filepath.Join(p.StateDir, app.LockFileName))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := unix.Kill(pid, 0); err != nil && !errors.Is(err, unix.EPERM) {
		return 0, false
	}
	// EPERM means the process exists and belongs to somebody else, which is the
	// ordinary case for a human inspecting a system-scope daemon.
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// No procfs: the signal probe already said the pid is live, and the
		// lock file is written by the daemon that holds the lock.
		return pid, true
	}
	if !strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), "llamaman") {
		return 0, false
	}
	return pid, true
}
