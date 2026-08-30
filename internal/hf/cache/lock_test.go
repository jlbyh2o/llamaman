package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"golang.org/x/sys/unix"
)

func TestAcquireIsExclusiveAndReleasable(t *testing.T) {
	hub := t.TempDir()
	path := cache.LockPath(hub, "org/name", "deadbeef")

	l, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A second acquisition through a SEPARATE descriptor is refused. (flock is
	// per open file description, so this is the same test another process would
	// run.)
	if _, err := cache.Acquire(path); !errors.Is(err, cache.ErrLocked) {
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	defer l2.Release()
}

// TestReleaseLeavesTheFile pins section 7.2a's file handling: huggingface_hub
// leaves the lock file too, and unlinking one under a concurrent acquirer is a
// classic race — the other process would hold a lock on an unnamed inode and the
// next acquirer would create a fresh file and lock nothing.
func TestReleaseLeavesTheFile(t *testing.T) {
	hub := t.TempDir()
	path := cache.LockPath(hub, "org/name", "deadbeef")

	l, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the lock file was removed on release: %v", err)
	}
}

// TestAcquireIsFlockNotFcntl is the test D27 asks for in prose: POSIX record
// locks and BSD file locks are independent mechanisms in the kernel, so a
// correct path taken with the wrong syscall interlocks with nothing and still
// passes every path test.
//
// The assertion is the discriminating one. A held flock blocks another flock;
// it does NOT block an fcntl(F_SETLK) on the same file. If Acquire ever moved to
// fcntl, the first check below would fail — which is exactly the failure mode
// that would otherwise be invisible.
func TestAcquireIsFlockNotFcntl(t *testing.T) {
	hub := t.TempDir()
	path := cache.LockPath(hub, "org/name", "deadbeef")

	l, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	other, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open the lock file again: %v", err)
	}
	defer other.Close()

	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("a second flock(2) = %v, want EWOULDBLOCK — the lock is not a BSD file lock", err)
	}

	// And the converse, which documents WHY the syscall had to be pinned: an
	// fcntl record lock sails straight through a held flock. Anyone who
	// "fixed" the primitive to fcntl would be interlocking with nothing.
	fl := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(other.Fd(), unix.F_SETLK, &fl); err != nil {
		t.Fatalf("fcntl(F_SETLK) over a held flock = %v, want success — "+
			"the two mechanisms are independent and this is why the syscall is pinned", err)
	}
}

func TestAcquireWaitTimesOutAndReportsWaiting(t *testing.T) {
	hub := t.TempDir()
	path := cache.LockPath(hub, "org/name", "deadbeef")

	held, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	waited := 0
	// A zero timeout means "try once and give up", which exercises the
	// notification without making the test sleep.
	_, err = cache.AcquireWait(context.Background(), path, 0, func() { waited++ })
	if !errors.Is(err, cache.ErrLocked) {
		t.Fatalf("AcquireWait = %v, want ErrLocked", err)
	}
	if waited != 1 {
		t.Fatalf("the waiting callback ran %d times, want exactly 1 — "+
			"a message that repeats every second is a log, not a status", waited)
	}
}

func TestAcquireWaitHonorsContext(t *testing.T) {
	hub := t.TempDir()
	path := cache.LockPath(hub, "org/name", "deadbeef")

	held, err := cache.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.AcquireWait(ctx, path, time.Hour, nil); !errors.Is(err, cache.ErrLocked) {
		t.Fatalf("AcquireWait on a canceled context = %v, want ErrLocked", err)
	}
}

func TestSweepStaleLocks(t *testing.T) {
	hub := t.TempDir()

	stale := cache.LockPath(hub, "org/old", "aaa")
	fresh := cache.LockPath(hub, "org/new", "bbb")
	heldPath := cache.LockPath(hub, "org/busy", "ccc")

	for _, p := range []string{stale, fresh, heldPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{stale, heldPath} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// A held lock is never swept, however old the file is: the holder is a
	// concurrent `hf download`, and removing its lock would let a second writer
	// into the same blob.
	held, err := cache.Acquire(heldPath)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	n, err := cache.SweepStaleLocks(hub, time.Now())
	if err != nil {
		t.Fatalf("SweepStaleLocks: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed %d locks, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale lock survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a lock younger than the threshold was removed")
	}
	if _, err := os.Stat(heldPath); err != nil {
		t.Error("a HELD lock was removed — a concurrent writer would lose its interlock")
	}
}

func TestSweepStaleLocksOnAnEmptyHub(t *testing.T) {
	n, err := cache.SweepStaleLocks(filepath.Join(t.TempDir(), "nothing-here"), time.Now())
	if err != nil || n != 0 {
		t.Fatalf("SweepStaleLocks on a missing hub = %d, %v; want 0, nil", n, err)
	}
}
