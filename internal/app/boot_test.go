package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
	"github.com/jlbyh2o/llamaman/internal/supervisor"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// recordingNotifier captures the sd_notify sequence so a test can assert the
// one ordering section 11.1 makes a promise about: READY=1 is sent, and only
// after the listener is up.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *recordingNotifier) record(s string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
	return nil
}

func (n *recordingNotifier) Ready() error          { return n.record("READY=1") }
func (n *recordingNotifier) Status(s string) error { return n.record("STATUS=" + s) }
func (n *recordingNotifier) ExtendTimeout(time.Duration) error {
	return n.record("EXTEND_TIMEOUT_USEC")
}
func (n *recordingNotifier) Watchdog() error { return n.record("WATCHDOG=1") }
func (n *recordingNotifier) Stopping() error { return n.record("STOPPING=1") }

func (n *recordingNotifier) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.sent))
	copy(out, n.sent)
	return out
}

// seedLoopback pre-creates the database and pins the two settings the port walk
// reads, so the test binds a free loopback port instead of the default
// 0.0.0.0:5526 — which on a developer machine may be taken, and on CI is not
// something a test should claim.
//
// It goes through the real store and the real migration runner, which is also
// what makes the boot under test exercise the "database already exists and is
// already migrated" path rather than only the fresh one.
func seedLoopback(t *testing.T, dir string) int {
	t.Helper()

	port := freePort(t)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatalf("open the seed store: %v", err)
	}
	defer st.Close()

	if _, err := st.Migrate(ctx, store.MigrateOptions{}); err != nil {
		t.Fatalf("seed migrate: %v", err)
	}
	err = st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, s := range []model.Setting{
			{Key: "ui.bind", Value: `"127.0.0.1"`, UpdatedBy: model.UpdatedByAdmin},
			{Key: "ui.port_desired", Value: itoa(port), UpdatedBy: model.UpdatedByAdmin},
		} {
			if err := st.PutSetting(ctx, tx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	return port
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe a free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestRunBootsServesAndShutsDown is the end-to-end boot test: the sequence of
// section 11.1 runs against a real SQLite file, the listener answers /healthz,
// canceling the context drains and returns cleanly, and the runtime_info row
// describes the boot that just happened.
func TestRunBootsServesAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	port := seedLoopback(t, dir)

	notifier := &recordingNotifier{}
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:           quiet(),
			Notifier:         notifier,
			StateDirOverride: dir,
			Getenv:           func(string) string { return "" },
			ReadyHook:        func(addr string) { ready <- addr },
		})
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("Run returned before it was listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon never became ready")
	}

	if _, p, _ := net.SplitHostPort(addr); p != itoaPlain(port) {
		t.Errorf("listening on %s, want the seeded port %d", addr, port)
	}

	// /healthz over the real listener.
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("/healthz status = %q, want ok", health.Status)
	}

	// A session route on an unclaimed host routes the SPA to the wizard.
	metaResp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + addr + "/api/v1/meta")
	if err != nil {
		t.Fatalf("GET /api/v1/meta: %v", err)
	}
	defer metaResp.Body.Close()
	var meta struct {
		SetupComplete bool `json:"setup_complete"`
		UIPort        int  `json:"ui_port"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		t.Fatalf("decoding /api/v1/meta: %v", err)
	}
	if meta.SetupComplete {
		t.Error("meta.setup_complete is true on a host nobody has claimed")
	}
	if meta.UIPort != port {
		t.Errorf("meta.ui_port = %d, want %d", meta.UIPort, port)
	}

	// The supervisor's boot reconciliation runs just after READY=1 and is the
	// one writer of the host-boot columns (D53, section 5.8). Waiting for that
	// write rather than sleeping is what keeps the assertion below
	// deterministic on a loaded machine.
	waitForHostBoot(t, dir)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want a clean shutdown", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the daemon did not shut down")
	}

	if sent := notifier.snapshot(); !contains(sent, "READY=1") {
		t.Errorf("sd_notify sequence = %v, want READY=1 in it", sent)
	}

	// The lock is released, so a second boot can take it.
	release, err := lockStateDir(dir)
	if err != nil {
		t.Fatalf("the state-directory lock outlived the daemon: %v", err)
	}
	_ = release()

	assertRuntimeInfo(t, dir, port)
}

// waitForHostBoot blocks until supervisor boot reconciliation has stamped
// `runtime_info.host_boot_id`, reading through a second, read-only handle on
// the live database the way `llamaman status` does.
func waitForHostBoot(t *testing.T, dir string) {
	t.Helper()
	if _, err := supervisor.ProcHostFacts(); err != nil {
		t.Logf("no host boot identity on this machine; not waiting for the stamp: %v", err)
		return
	}

	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := store.OpenReadOnly(ctx, filepath.Join(dir, DatabaseFileName))
		if err == nil {
			var ri model.RuntimeInfo
			readErr := st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
				var err error
				ri, err = st.RuntimeInfo(ctx, tx)
				return err
			})
			st.Close()
			if readErr == nil && ri.HostBootID != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor never stamped runtime_info.host_boot_id (D53)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertRuntimeInfo(t *testing.T, dir string, port int) {
	t.Helper()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer st.Close()

	var ri model.RuntimeInfo
	err = st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		ri, err = st.RuntimeInfo(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("read runtime_info: %v", err)
	}

	if ri.UIPort == nil || int(*ri.UIPort) != port {
		t.Errorf("runtime_info.ui_port = %v, want %d", ri.UIPort, port)
	}
	if ri.StateDir == nil || *ri.StateDir != dir {
		t.Errorf("runtime_info.state_dir = %v, want %q", ri.StateDir, dir)
	}
	if ri.BootID == nil || *ri.BootID == "" {
		t.Error("runtime_info.boot_id was not written")
	}
	if ri.ListenerContinuity == nil || *ri.ListenerContinuity != model.ContinuityNone {
		t.Errorf("runtime_info.listener_continuity = %v, want %q (no fd store yet)",
			ri.ListenerContinuity, model.ContinuityNone)
	}
	if ri.SchemaVersion == nil || *ri.SchemaVersion < 1 {
		t.Errorf("runtime_info.schema_version = %v, want the applied version", ri.SchemaVersion)
	}
	// D53: the host boot columns have exactly one writer, and it is not the
	// boot sequence — it is supervisor boot reconciliation step 1 (section
	// 5.8), which runs a moment after READY=1.
	//
	// So what this asserts is that the ONE writer ran and wrote the host's real
	// boot identity. `writeRuntimeInfo` only ever carries the previous value
	// forward, and the previous value on a fresh database is NULL, so a value
	// here that matches /proc can have come from nowhere else.
	host, hostErr := supervisor.ProcHostFacts()
	switch {
	case hostErr != nil:
		t.Logf("skipping the host-boot assertion: %v", hostErr)
	case ri.HostBootID == nil:
		t.Error("runtime_info.host_boot_id is NULL; the supervisor's boot reconciliation did not run (D53)")
	case *ri.HostBootID != host.ID:
		t.Errorf("runtime_info.host_boot_id = %q, want the host's own %q", *ri.HostBootID, host.ID)
	}
}

// TestRunRefusesASecondDaemon is F11 through the public entry point.
func TestRunRefusesASecondDaemon(t *testing.T) {
	dir := t.TempDir()
	release, err := lockStateDir(dir)
	if err != nil {
		t.Fatalf("pre-lock: %v", err)
	}
	defer release()

	err = Run(context.Background(), Options{
		Logger:           quiet(),
		StateDirOverride: dir,
		Getenv:           func(string) string { return "" },
	})
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("Run = %v (%T), want *LockedError", err, err)
	}
}

// TestEnforceDBMode is step 3's "enforce mode 0600": a database restored from a
// backup or copied by an operator can arrive wider than it should be, and it
// holds session secrets and the sealed HF token.
func TestEnforceDBMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "llamaman.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := enforceDBMode(path); err != nil {
		t.Fatalf("enforceDBMode: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func itoaPlain(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
