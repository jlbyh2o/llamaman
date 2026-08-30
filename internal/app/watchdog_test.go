package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/store"
)

// The watchdog of DESIGN section 5.4a: `WATCHDOG=1` every WatchdogSec/2, GATED
// ON A LIVE `SELECT 1`.
//
// The gate is the property worth testing. An unconditional ping proves only that
// the process still schedules goroutines; the point of `WatchdogSec=30` on this
// unit is that a daemon wedged on its database is killed and restarted rather
// than left `active` while accepting requests it cannot serve.

// pingCounter is a Notifier that counts watchdog pings and nothing else.
type pingCounter struct {
	mu    sync.Mutex
	pings int
}

func (p *pingCounter) Ready() error                      { return nil }
func (p *pingCounter) Status(string) error               { return nil }
func (p *pingCounter) ExtendTimeout(time.Duration) error { return nil }
func (p *pingCounter) Stopping() error                   { return nil }

func (p *pingCounter) Watchdog() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pings++
	return nil
}

func (p *pingCounter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pings
}

// watchdogEnv is the environment systemd sets for a unit with WatchdogSec=, at
// the period a test can afford to wait for.
func watchdogEnv(usec int) func(string) string {
	return func(key string) string {
		switch key {
		case "WATCHDOG_USEC":
			return strconv.Itoa(usec)
		case "WATCHDOG_PID":
			// $WATCHDOG_PID exists because the variables are inherited: a
			// subprocess pinging on its parent's behalf would keep a wedged
			// daemon alive.
			return strconv.Itoa(os.Getpid())
		default:
			return ""
		}
	}
}

func TestWatchdogPingsWhileTheDatabaseAnswers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	notifier := &pingCounter{}
	d := &daemon{
		log:   quiet(),
		store: st,
		opts:  withDefaults(Options{Logger: quiet(), Notifier: notifier, Getenv: watchdogEnv(20_000)}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.watchdog(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for notifier.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the watchdog sent %d pings in 10 s against a live database", notifier.count())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestWatchdogWithholdsThePingWhenTheDatabaseIsGone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, DatabaseFileName))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// A closed pool is this test's stand-in for a database that will not
	// answer: `SELECT 1` fails, and the ping must NOT be sent — withholding it
	// is what escalates to systemd.
	if err := st.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}

	notifier := &pingCounter{}
	d := &daemon{
		log:   quiet(),
		store: st,
		opts:  withDefaults(Options{Logger: quiet(), Notifier: notifier, Getenv: watchdogEnv(20_000)}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.watchdog(ctx)

	if got := notifier.count(); got != 0 {
		t.Fatalf("the watchdog sent %d pings while the database could not answer", got)
	}
}

// TestWatchdogIsSilentWithoutAUnit is the hand-run daemon: no WATCHDOG_USEC
// means nothing is listening, and a ping into a closed socket is noise.
func TestWatchdogIsSilentWithoutAUnit(t *testing.T) {
	t.Parallel()

	notifier := &pingCounter{}
	d := &daemon{
		log:  quiet(),
		opts: withDefaults(Options{Logger: quiet(), Notifier: notifier, Getenv: func(string) string { return "" }}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	d.watchdog(ctx) // returns immediately

	if got := notifier.count(); got != 0 {
		t.Fatalf("the watchdog sent %d pings with no WATCHDOG_USEC in the environment", got)
	}
}
