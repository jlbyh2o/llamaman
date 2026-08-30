package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/store"
	"golang.org/x/sys/unix"
)

// `llamaman reset-password` (DESIGN section 11.3), for the operator who locked
// themselves out of the web UI.
//
// It is one of exactly TWO subcommands deliberately outside §11.3's "create
// nothing under the state directory" rule, because its whole purpose is to WRITE
// the database with the daemon down. The rule it obeys instead is the one stated
// beside that exception: the euid is checked against `stat`, and when the caller
// is root every file it may have created — a `-wal` or `-shm` it had to open — is
// chowned to the database's uid/gid and chmoded 0600 before it exits. A
// root-owned sidecar beside a 0600 database is a database the service identity
// can never write again.
//
// Authorization is filesystem access to that 0600 database — root or the service
// identity — exactly as SPEC §3.9 requires: no password, no network.

// ResetPassword resets the admin password from the host.
func ResetPassword(env Env, args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fromStdin := fs.Bool("stdin", false, "read the new password from stdin instead of prompting")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "Usage: llamaman reset-password [--stdin]\n\n")
		fmt.Fprintf(env.Stderr, "Run as root or as the database's owner. Prompts twice on a terminal;\n")
		fmt.Fprintf(env.Stderr, "refuses on a non-terminal without --stdin. Every session is revoked.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	p := resolvePaths(env)
	if !p.Exists {
		err := &notInitializedError{path: p.DBPath}
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return &ExitError{Code: 1, Err: err}
	}

	owner, gid, err := dbOwner(p.DBPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return err
	}
	// The euid is checked against stat rather than against "am I root", because
	// the service identity is the other legitimate caller and it is not root.
	if euid := os.Geteuid(); euid != 0 && euid != owner {
		err := fmt.Errorf("run as root or as %s (uid %d), the owner of %s",
			identityName(owner), owner, p.DBPath)
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return &ExitError{Code: 2, Err: err}
	}

	password, err := readNewPassword(env, *fromStdin)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return err
	}

	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman reset-password: open %s: %v\n", p.DBPath, err)
		return err
	}
	defer func() {
		st.Close()
		// After the pools are closed — and therefore after the WAL sidecars
		// have been created and possibly removed — hand every file back to the
		// database's owner. This is the whole of §11.3's exception clause, and
		// it runs whether the write succeeded or not.
		if os.Geteuid() == 0 {
			reownDatabase(env, p.DBPath, owner, gid)
		}
	}()

	svc, err := auth.New(auth.Config{Repo: st, StateDir: p.StateDir, Now: env.now})
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return err
	}

	if err := svc.ResetPassword(ctx, password); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = fmt.Errorf("this host has no admin account yet — claim it in the browser, or with the setup token from `llamaman status`")
		}
		fmt.Fprintf(env.Stderr, "llamaman reset-password: %v\n", err)
		return err
	}

	fmt.Fprintf(env.Stdout, "the admin password was reset; every session has been revoked\n")
	return nil
}

// readNewPassword prompts twice on a terminal, or reads one line from stdin with
// --stdin. It REFUSES on a non-terminal without --stdin, so a password can never
// be taken from a pipe by accident — which is what §11.3 asks for and what stops
// a shell history or a CI log from becoming a credential store.
func readNewPassword(env Env, fromStdin bool) (string, error) {
	if fromStdin {
		r := bufio.NewReader(env.stdin())
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read the password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if !env.Interactive {
		return "", errors.New("refusing to read a password from a non-terminal without --stdin")
	}

	first, err := promptSecret(env, "New admin password: ")
	if err != nil {
		return "", err
	}
	second, err := promptSecret(env, "Repeat the password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two entries do not match")
	}
	return first, nil
}

// promptSecret reads one line from the terminal with echo disabled.
//
// It manipulates the terminal attributes directly through x/sys/unix rather than
// adding a dependency for it: DESIGN section 14 lists `golang.org/x/sys/unix` for
// exactly this class of thing, and the whole of "turn echo off, read a line, turn
// it back on" is the twenty lines below.
func promptSecret(env Env, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())

	before, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", fmt.Errorf("read the terminal attributes: %w", err)
	}
	quiet := *before
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return "", fmt.Errorf("disable terminal echo: %w", err)
	}
	// Restoring must happen on every path, including a read error: a terminal
	// left with echo off is a terminal the operator has to blindly type `stty
	// sane` into.
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, before) }()

	fmt.Fprint(env.Stdout, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprintln(env.Stdout)
	if err != nil && line == "" {
		return "", fmt.Errorf("read the password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// dbOwner returns the uid and gid that own the database file.
func dbOwner(path string) (uid, gid int, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return os.Geteuid(), os.Getegid(), nil
	}
	return int(st.Uid), int(st.Gid), nil
}

// reownDatabase is §11.3's exception clause: when root wrote the database, every
// file it may have created is chowned back to the database's identity and
// chmoded 0600 before the command exits.
func reownDatabase(env Env, dbPath string, uid, gid int) {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Chown(path, uid, gid); err != nil {
			fmt.Fprintf(env.Stderr,
				"llamaman reset-password: could not chown %s to %d:%d: %v\n", path, uid, gid, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			fmt.Fprintf(env.Stderr,
				"llamaman reset-password: could not chmod %s to 0600: %v\n", path, err)
		}
	}
}
