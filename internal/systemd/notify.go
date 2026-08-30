package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sd_notify(3), in about as many lines as D9 promised: datagrams written to
// $NOTIFY_SOCKET, no cgo and no dependency.
//
// $NOTIFY_SOCKET is set by the service manager, not by a user, so reading it is
// not a configuration file and does not breach SPEC section 3.9 — the same
// argument D72 makes for $STATE_DIRECTORY.

// Notifier writes sd_notify datagrams. A nil *Notifier is valid and every method
// on it is a no-op, which is what a hand-run `llamaman serve` gets: the daemon
// does not branch on whether it was started by systemd.
type Notifier struct {
	mu   sync.Mutex
	conn *net.UnixConn
	addr string
}

// NewNotifier connects to $NOTIFY_SOCKET.
//
// ok is false, with no error, when the variable is unset: that is not a failure,
// it is a daemon that was not started by a Type=notify unit.
func NewNotifier(getenv func(string) string) (n *Notifier, ok bool, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	addr := getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil, false, nil
	}

	name := addr
	// A leading '@' names the abstract namespace, where the first byte of the
	// address is NUL. Go's net package spells that with a literal NUL rather
	// than an '@'.
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return nil, false, fmt.Errorf("systemd: dial %s: %w", addr, err)
	}
	return &Notifier{conn: conn, addr: addr}, true, nil
}

// Close releases the socket.
func (n *Notifier) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn == nil {
		return nil
	}
	err := n.conn.Close()
	n.conn = nil
	return err
}

// send writes one datagram. State strings are newline-separated `KEY=value`
// pairs.
func (n *Notifier) send(msg string) error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn == nil {
		return nil
	}
	if _, err := n.conn.Write([]byte(msg)); err != nil {
		return fmt.Errorf("systemd: sd_notify %q: %w", firstLine(msg), err)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Ready sends READY=1.
//
// It is sent only after the database is open, PRAGMA integrity_check has
// passed, migrations are applied, the HTTP listener is bound AND the self-update
// confirmation gate has run — that last one so a daemon which signals readiness
// has provably already resolved update/pending and cannot leave the judge armed
// against a version that booted (D92, section 11.1 steps 10-12).
func (n *Notifier) Ready() error { return n.send("READY=1") }

// Status sends STATUS=, which is what `systemctl status llamaman` shows. The
// daemon publishes the resolved URL through it, so the port the walk actually
// landed on is discoverable from the host (D9/D24).
//
// Newlines are stripped rather than escaped: a state string is newline-delimited
// key/value pairs, so a message containing one would be read as a second
// assignment.
func (n *Notifier) Status(s string) error {
	return n.send("STATUS=" + strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
}

// Watchdog sends WATCHDOG=1.
//
// The caller sends it every WatchdogInterval()/2 and GATES IT on a live
// `SELECT 1`, so a daemon wedged on its database is killed and restarted rather
// than left accepting requests it cannot serve (section 5.4a).
func (n *Notifier) Watchdog() error { return n.send("WATCHDOG=1") }

// ExtendTimeout sends EXTEND_TIMEOUT_USEC=, which pushes the current
// TimeoutStartSec=/TimeoutStopSec= deadline out by d from now.
//
// This is what makes a legitimately slow start safe under a 45 s
// TimeoutStartSec=: the boot sequence sends it every 10 s while PRAGMA
// integrity_check or a migration is running, so the case that would otherwise
// worry this design — a healthy but merely slow daemon being killed and reverted
// — cannot arise (D88, section 5.4).
func (n *Notifier) ExtendTimeout(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	return n.send("EXTEND_TIMEOUT_USEC=" + strconv.FormatInt(d.Microseconds(), 10))
}

// Stopping sends STOPPING=1, so the manager knows a clean shutdown is under way
// rather than a crash.
func (n *Notifier) Stopping() error { return n.send("STOPPING=1") }

// StoreFD hands a file descriptor to systemd's file-descriptor store (D58).
//
// This is the mechanism that keeps every public gateway listener and the
// management listener OPEN across `systemctl restart`: the fds are stored with a
// name before exit and re-adopted from LISTEN_FDS/LISTEN_FDNAMES on the next
// start (section 9.4). Socket activation cannot be used — a .socket unit per
// instance would need a privileged runtime write, which D3 and D57 forbid — and
// the unit's FileDescriptorStoreMax=256 is what bounds it.
//
// The descriptor travels as SCM_RIGHTS ancillary data on the same datagram,
// which is the only way to pass one over a unix socket. The caller keeps
// ownership of fd; systemd dups it.
//
// Nothing calls this yet: the listener half is section 9.4's, and until it lands
// every boot rebinds and records listener_continuity='none'.
func (n *Notifier) StoreFD(name string, fd int) error {
	if n == nil {
		return nil
	}
	if strings.ContainsAny(name, "\n:") {
		return fmt.Errorf("systemd: FDNAME %q may not contain a newline or a colon", name)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn == nil {
		return nil
	}

	payload := []byte("FDSTORE=1\nFDNAME=" + name)
	rights := unix.UnixRights(fd)

	raw, err := n.conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("systemd: FDSTORE: %w", err)
	}
	var sendErr error
	ctrlErr := raw.Control(func(sock uintptr) {
		sendErr = unix.Sendmsg(int(sock), payload, rights, nil, 0)
	})
	if ctrlErr != nil {
		return fmt.Errorf("systemd: FDSTORE: %w", ctrlErr)
	}
	if sendErr != nil {
		return fmt.Errorf("systemd: FDSTORE %q: %w", name, sendErr)
	}
	return nil
}

// WatchdogInterval reports the WatchdogSec= the unit declared, and whether this
// process is the one it applies to.
//
// $WATCHDOG_PID exists because the variables are inherited by children: a
// subprocess that pinged the watchdog on its parent's behalf would keep a wedged
// daemon alive, which is the one outcome the watchdog exists to prevent.
func WatchdogInterval(getenv func(string) string) (time.Duration, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	usec := getenv("WATCHDOG_USEC")
	if usec == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	if pid := getenv("WATCHDOG_PID"); pid != "" {
		want, err := strconv.Atoi(pid)
		if err != nil || want != os.Getpid() {
			return 0, false
		}
	}
	return time.Duration(n) * time.Microsecond, true
}

// ListenFD is one descriptor systemd passed in, with the name it was stored
// under.
type ListenFD struct {
	FD   int
	Name string
}

// ListenFDs returns the descriptors systemd handed this process through
// LISTEN_FDS/LISTEN_FDNAMES — the other half of StoreFD, and how section 9.4
// re-adopts listeners after a restart.
//
// Descriptors start at SD_LISTEN_FDS_START (3). $LISTEN_PID is checked for the
// same reason $WATCHDOG_PID is: the variables are inherited, and a child that
// believed them would adopt descriptors it does not have.
func ListenFDs(getenv func(string) string) []ListenFD {
	if getenv == nil {
		getenv = os.Getenv
	}
	if pid := getenv("LISTEN_PID"); pid != "" {
		want, err := strconv.Atoi(pid)
		if err != nil || want != os.Getpid() {
			return nil
		}
	}
	count, err := strconv.Atoi(getenv("LISTEN_FDS"))
	if err != nil || count <= 0 {
		return nil
	}

	var names []string
	if v := getenv("LISTEN_FDNAMES"); v != "" {
		names = strings.Split(v, ":")
	}

	const listenFDsStart = 3
	out := make([]ListenFD, 0, count)
	for i := range count {
		fd := ListenFD{FD: listenFDsStart + i}
		if i < len(names) {
			fd.Name = names[i]
		}
		out = append(out, fd)
	}
	return out
}
