package systemd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// notifySocket stands in for systemd's end of $NOTIFY_SOCKET.
type notifySocket struct {
	conn *net.UnixConn
	path string
}

func newNotifySocket(t *testing.T) *notifySocket {
	t.Helper()

	// The path must stay under sun_path's 108-byte limit, which a deep temp
	// directory can exceed — a failure that looks like "invalid argument" and
	// has nothing to do with the code under test.
	path := filepath.Join(t.TempDir(), "notify.sock")
	if len(path) > 100 {
		t.Skipf("temp path %q is too long for a unix socket", path)
	}

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &notifySocket{conn: conn, path: path}
}

// receiveMessage reads one datagram.
func (s *notifySocket) receiveMessage(t *testing.T) string {
	t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, err := s.conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

func (s *notifySocket) getenv(key string) string {
	if key == "NOTIFY_SOCKET" {
		return s.path
	}
	return ""
}

// TestNotifyMessages pins the wire format of every state string the boot
// sequence sends.
func TestNotifyMessages(t *testing.T) {
	t.Parallel()

	sock := newNotifySocket(t)
	n, ok, err := NewNotifier(sock.getenv)
	if err != nil || !ok {
		t.Fatalf("NewNotifier = (%v, %v)", ok, err)
	}
	defer n.Close()

	tests := []struct {
		name string
		send func() error
		want string
	}{
		{"ready", n.Ready, "READY=1"},
		{"watchdog", n.Watchdog, "WATCHDOG=1"},
		{"stopping", n.Stopping, "STOPPING=1"},
		{
			name: "status carries the resolved URL",
			send: func() error { return n.Status("http://192.168.1.20:5526") },
			want: "STATUS=http://192.168.1.20:5526",
		},
		{
			// A state string is newline-delimited key/value pairs, so a newline
			// inside a status would be read as a second assignment.
			name: "a multi-line status is flattened",
			send: func() error { return n.Status("line one\nline two") },
			want: "STATUS=line one line two",
		},
		{
			name: "extend timeout is microseconds",
			send: func() error { return n.ExtendTimeout(30 * time.Second) },
			want: "EXTEND_TIMEOUT_USEC=30000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := sock.receiveMessage(t); got != tc.want {
				t.Errorf("datagram = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNotifyExtendTimeoutIgnoresNonPositive: a zero or negative extension would
// otherwise tell systemd the deadline is now.
func TestNotifyExtendTimeoutIgnoresNonPositive(t *testing.T) {
	t.Parallel()

	sock := newNotifySocket(t)
	n, _, err := NewNotifier(sock.getenv)
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}
	defer n.Close()

	if err := n.ExtendTimeout(0); err != nil {
		t.Fatalf("ExtendTimeout(0): %v", err)
	}
	if err := n.ExtendTimeout(-time.Second); err != nil {
		t.Fatalf("ExtendTimeout(-1s): %v", err)
	}

	// Nothing was sent, so a real send afterwards is the first datagram.
	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := sock.receiveMessage(t); got != "READY=1" {
		t.Errorf("first datagram = %q; a non-positive extension was sent", got)
	}
}

// TestNotifyWithoutSocket: a daemon started outside systemd gets no notifier and
// no error, and every method on the nil value is a no-op — so the boot sequence
// does not branch on how it was started.
func TestNotifyWithoutSocket(t *testing.T) {
	t.Parallel()

	n, ok, err := NewNotifier(func(string) string { return "" })
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}
	if ok {
		t.Fatal("NewNotifier reported a socket where there is none")
	}
	if n != nil {
		t.Fatal("NewNotifier returned a notifier with no socket")
	}

	for name, call := range map[string]func() error{
		"Ready":         n.Ready,
		"Watchdog":      n.Watchdog,
		"Stopping":      n.Stopping,
		"Status":        func() error { return n.Status("x") },
		"ExtendTimeout": func() error { return n.ExtendTimeout(time.Second) },
		"StoreFD":       func() error { return n.StoreFD("ui", 0) },
		"Close":         n.Close,
	} {
		if err := call(); err != nil {
			t.Errorf("nil notifier %s() = %v, want nil", name, err)
		}
	}
}

// TestNotifyAbstractSocket: a '@' prefix names the abstract namespace, where the
// first byte of the address is NUL. Dialing the literal '@' path would fail with
// a confusing ENOENT on the hosts that use it.
func TestNotifyAbstractSocket(t *testing.T) {
	t.Parallel()

	name := "@llamaman-test-" + strconv.Itoa(os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: "\x00" + name[1:], Net: "unixgram"})
	if err != nil {
		t.Skipf("abstract unix sockets unavailable: %v", err)
	}
	defer conn.Close()

	n, ok, err := NewNotifier(func(k string) string {
		if k == "NOTIFY_SOCKET" {
			return name
		}
		return ""
	})
	if err != nil || !ok {
		t.Fatalf("NewNotifier = (%v, %v)", ok, err)
	}
	defer n.Close()

	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 64)
	nRead, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:nRead]); got != "READY=1" {
		t.Errorf("datagram = %q", got)
	}
}

// TestNotifyStoreFD is D58's half of listener continuity: the descriptor travels
// as SCM_RIGHTS ancillary data on the same datagram as FDSTORE=1, which is the
// only way to pass one over a unix socket.
func TestNotifyStoreFD(t *testing.T) {
	t.Parallel()

	sock := newNotifySocket(t)
	n, ok, err := NewNotifier(sock.getenv)
	if err != nil || !ok {
		t.Fatalf("NewNotifier = (%v, %v)", ok, err)
	}
	defer n.Close()

	// Any descriptor will do; a listener is what the real caller passes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	file, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer file.Close()

	if err := n.StoreFD("ui", int(file.Fd())); err != nil {
		t.Fatalf("StoreFD: %v", err)
	}

	msg, oob := make([]byte, 1024), make([]byte, 256)
	raw, err := sock.conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	if err := sock.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	var nMsg, nOob int
	var recvErr error
	if err := raw.Read(func(fd uintptr) bool {
		nMsg, nOob, _, _, recvErr = unix.Recvmsg(int(fd), msg, oob, 0)
		return recvErr != unix.EAGAIN && recvErr != unix.EWOULDBLOCK
	}); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if recvErr != nil {
		t.Fatalf("Recvmsg: %v", recvErr)
	}

	if got := string(msg[:nMsg]); got != "FDSTORE=1\nFDNAME=ui" {
		t.Errorf("datagram = %q, want FDSTORE=1 and FDNAME=ui", got)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:nOob])
	if err != nil {
		t.Fatalf("ParseSocketControlMessage: %v", err)
	}
	if len(scms) != 1 {
		t.Fatalf("control messages = %d, want 1 — the descriptor did not travel", len(scms))
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		t.Fatalf("ParseUnixRights: %v", err)
	}
	if len(fds) != 1 {
		t.Fatalf("descriptors = %d, want 1", len(fds))
	}
	unix.Close(fds[0])
}

// TestNotifyStoreFDRejectsABadName: FDNAME is delimited by ':' in
// LISTEN_FDNAMES and the state string is delimited by newlines, so neither may
// appear in a name.
func TestNotifyStoreFDRejectsABadName(t *testing.T) {
	t.Parallel()

	sock := newNotifySocket(t)
	n, _, err := NewNotifier(sock.getenv)
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}
	defer n.Close()

	for _, bad := range []string{"a:b", "a\nb"} {
		if err := n.StoreFD(bad, 0); err == nil {
			t.Errorf("StoreFD(%q) was accepted", bad)
		}
	}
}

// TestWatchdogInterval covers the $WATCHDOG_PID guard, which exists because the
// variables are inherited: a subprocess pinging the watchdog on its parent's
// behalf would keep a wedged daemon alive, which is the one outcome the watchdog
// exists to prevent.
func TestWatchdogInterval(t *testing.T) {
	t.Parallel()

	self := strconv.Itoa(os.Getpid())
	tests := []struct {
		name string
		env  map[string]string
		want time.Duration
		ok   bool
	}{
		{"unset", map[string]string{}, 0, false},
		{"set for this process", map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": self}, 30 * time.Second, true},
		{"set with no pid", map[string]string{"WATCHDOG_USEC": "30000000"}, 30 * time.Second, true},
		{"set for another process", map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": "1"}, 0, false},
		{"unparsable", map[string]string{"WATCHDOG_USEC": "soon"}, 0, false},
		{"zero", map[string]string{"WATCHDOG_USEC": "0"}, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := WatchdogInterval(func(k string) string { return tc.env[k] })
			if got != tc.want || ok != tc.ok {
				t.Errorf("WatchdogInterval = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestListenFDs is the adoption half of D58: descriptors start at 3, names come
// from LISTEN_FDNAMES in order, and a mismatched $LISTEN_PID means these
// descriptors belong to somebody else.
func TestListenFDs(t *testing.T) {
	t.Parallel()

	self := strconv.Itoa(os.Getpid())
	tests := []struct {
		name string
		env  map[string]string
		want []ListenFD
	}{
		{"unset", map[string]string{}, nil},
		{
			name: "two named listeners",
			env:  map[string]string{"LISTEN_PID": self, "LISTEN_FDS": "2", "LISTEN_FDNAMES": "ui:gw-8081"},
			want: []ListenFD{{FD: 3, Name: "ui"}, {FD: 4, Name: "gw-8081"}},
		},
		{
			name: "names may be missing",
			env:  map[string]string{"LISTEN_PID": self, "LISTEN_FDS": "2"},
			want: []ListenFD{{FD: 3}, {FD: 4}},
		},
		{
			name: "inherited by a child",
			env:  map[string]string{"LISTEN_PID": "1", "LISTEN_FDS": "2", "LISTEN_FDNAMES": "ui"},
			want: nil,
		},
		{
			name: "zero descriptors",
			env:  map[string]string{"LISTEN_PID": self, "LISTEN_FDS": "0"},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ListenFDs(func(k string) string { return tc.env[k] })
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ListenFDs (-want +got):\n%s", diff)
			}
		})
	}
}
