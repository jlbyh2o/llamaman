package gateway

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Listener continuity across a daemon restart (DESIGN section 9.4, D58).
//
// SPEC §1 makes Llama Man the owner of the public inference ports and SPEC §3.8
// promises a self-update leaves running instances unaffected. Those are only
// compatible if a daemon restart does not close the gateway — and D12
// deliberately removed the FD-inheriting re-exec, so something has to hold the
// sockets open across `systemctl restart`. That something is systemd's
// file-descriptor store: no privileged write, no `.socket` unit per instance
// (which D3 and D57 forbid), and no re-exec.
//
// Two mechanics in this file are what make it work, and both are easy to get
// subtly wrong:
//
//   - PAUSING IS NOT CLOSING. Section 9.4 step 3 says "stop accepting new
//     connections on each listener, but keep the socket open", because the
//     kernel accept queue is what turns a restart into a pause rather than a
//     refusal. `http.Server.Shutdown` closes its listeners, so the server is
//     given a WRAPPER whose Close only pauses: it sets an accept deadline in the
//     past, which unblocks the accept loop without touching the socket.
//   - THE FD MUST NOT BE DUPPED CARELESSLY. `TCPListener.File()` dups and puts
//     the original into blocking mode; `SyscallConn().Control` hands the raw
//     descriptor to a callback and disturbs nothing. systemd dups it out of the
//     SCM_RIGHTS datagram, so borrowing it for the length of one call is exactly
//     what is wanted.

// fdNamePrefix is what §9.4 step 6 names a gateway socket:
// `FDNAME=gw-<instance_id>`. The management listener is `ui` and belongs to the
// composition root, not to this package.
const fdNamePrefix = "gw-"

// FDName renders the fd-store name for one instance's listener.
func FDName(instanceID string) string { return fdNamePrefix + instanceID }

// InstanceIDFromFDName recovers the instance id from a stored name, and reports
// whether the name is one of ours. A name that is not — `ui`, or anything a
// future release stores — is left alone rather than adopted and closed.
func InstanceIDFromFDName(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, fdNamePrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// InheritedFD is one descriptor the service manager handed this process through
// LISTEN_FDS/LISTEN_FDNAMES.
//
// It is declared here rather than taken from internal/systemd so that this
// package keeps no dependency on the one package allowed to speak to systemd
// (D49 invariant 2). The composition root converts; the conversion is two
// fields.
type InheritedFD struct {
	FD   int
	Name string
}

// FDStore is systemd's file-descriptor store as §9.4 uses it. internal/systemd's
// *Notifier satisfies it, and a nil FDStore is the documented "no NOTIFY_SOCKET"
// case that records `listener_continuity='none'`.
type FDStore interface {
	// StoreFD hands one descriptor to the manager under a name. The caller keeps
	// ownership of fd.
	StoreFD(name string, fd int) error
}

// pausable wraps a listening socket so that http.Server.Shutdown stops the
// accept loop WITHOUT closing the socket.
//
// Accept returns errPaused once paused, which is not a net.Error and therefore
// not "temporary": http.Server.Serve does not retry it. Serve still returns
// ErrServerClosed rather than this error, because it checks its own
// shutting-down flag first.
type pausable struct {
	inner  net.Listener
	paused atomic.Bool
}

// errPaused ends the accept loop. It is deliberately not a net.Error: a
// net.Error with Timeout() true would send http.Server into its retry-with-
// backoff path instead of returning.
var errPaused = errors.New("gateway: listener paused for drain")

func (l *pausable) Accept() (net.Conn, error) {
	if l.paused.Load() {
		return nil, errPaused
	}
	c, err := l.inner.Accept()
	if err != nil && l.paused.Load() {
		// The deadline fired. Report the pause rather than a timeout, so the
		// server stops instead of retrying.
		return nil, errPaused
	}
	return c, err
}

// Close pauses. It does NOT close the socket — that is the whole point — so the
// kernel keeps queueing connections that arrive during the restart gap.
func (l *pausable) Close() error {
	if l.paused.Swap(true) {
		return nil
	}
	// Unblock an accept that is already parked in the kernel. A deadline in the
	// past does that without closing anything; on a listener that does not
	// support deadlines there is nothing to unblock and nothing to do.
	if d, ok := l.inner.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now())
	}
	return nil
}

func (l *pausable) Addr() net.Addr { return l.inner.Addr() }

// closeSocket really closes the underlying socket. It is what a listener that is
// going away for good gets — a deleted instance, a changed port, a daemon that
// is stopping rather than restarting.
func (l *pausable) closeSocket() error {
	l.paused.Store(true)
	return l.inner.Close()
}

// controlFD borrows the raw descriptor for the length of fn, without dupping it
// and without changing its blocking mode.
//
// `TCPListener.File()` would also yield a descriptor, but it dups AND puts the
// original into blocking mode, which is a side effect on a socket the process is
// still serving from. systemd dups the descriptor out of the SCM_RIGHTS
// datagram, so borrowing it for the length of one call is all that is needed.
func (l *pausable) controlFD(fn func(fd int)) error {
	sc, ok := l.inner.(syscall.Conn)
	if !ok {
		return errNoRawConn
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	return raw.Control(func(fd uintptr) { fn(int(fd)) })
}

var errNoRawConn = errors.New("gateway: this listener has no raw descriptor to store")

// listen binds one public port. The bind address is `gateway.bind` (§9.1),
// which defaults to 0.0.0.0 — SPEC §1's trusted-LAN exposure, since a gateway
// reachable only from loopback would defeat the entire point of owning the
// public port. `ui.bind` governs the management UI only and the two are
// deliberately separate.
func listen(bind string, port int) (net.Listener, error) {
	addr := net.JoinHostPort(bind, fmt.Sprint(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", addr, err)
	}
	return ln, nil
}

// adopt turns one inherited descriptor into a listener.
//
// The *os.File takes ownership of the descriptor and net.FileListener dups it,
// so the file is closed immediately afterwards: leaking one descriptor per
// restart is exactly the kind of slow failure a long-lived daemon must not have.
func adopt(fd InheritedFD) (net.Listener, error) {
	f := os.NewFile(uintptr(fd.FD), fd.Name)
	if f == nil {
		return nil, fmt.Errorf("gateway: %s is not a valid descriptor", fd.Name)
	}
	defer f.Close()

	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("gateway: adopt %s: %w", fd.Name, err)
	}
	return ln, nil
}

// portOf reads the port a listener is bound to, or 0 when it is not a TCP
// listener — which is what a socketpair fake in a test gives, and which is why
// adoption matches on the NAME and verifies the port rather than deducing the
// instance from the address.
func portOf(ln net.Listener) int {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}
