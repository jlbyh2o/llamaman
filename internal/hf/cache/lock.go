package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// The interop lock (D27, DESIGN section 7.2a).
//
// The path is half the contract and Layout.LockPath owns it. This file owns the
// other half, and it is `flock(2)`:
//
//	`huggingface_hub`'s WeakFileLock is built on `filelock`, whose Unix backend
//	calls Python's `fcntl.flock` — which is flock(2), NOT fcntl(F_SETLK).
//
// POSIX record locks and BSD file locks are entirely independent mechanisms in
// the kernel: a process holding one does not block a process taking the other,
// on the same file, ever. A correct path taken with the wrong syscall interlocks
// with nothing and still passes every path test, which is why the syscall is
// pinned here in prose as well as in code. This package never calls fcntl
// locking, and never will.

// ErrLocked is returned by Acquire when another process holds the lock. It is a
// distinct value rather than an error string because the caller's response to it
// is a documented behavior and not a failure: section 7.2a moves the download
// task to `running` with `last_error='waiting_for_lock'`, the UI says "another
// tool is downloading this file", and the worker retries.
var ErrLocked = errors.New("hf/cache: the interop lock is held by another process")

// LockRetryEvery and LockTimeout are section 7.2a's acquisition policy: try
// LOCK_EX|LOCK_NB, and on EWOULDBLOCK retry every second for up to thirty
// minutes before failing the task with `lock_timeout` — resumable, nothing
// discarded.
//
// Blocking flock is never used. It would hold a worker slot invisibly, and the
// UI message above would be unimplementable: there would be no moment at which
// the code knows it is waiting.
const (
	LockRetryEvery = 1 * time.Second
	LockTimeout    = 30 * time.Minute
)

// Lock is a held interop lock. Release it exactly once; the zero value is not
// usable.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes the lock at path, which must be the one Layout.LockPath built.
//
// File handling follows `huggingface_hub` so that two tools running as the same
// user can both open the file: the `.locks/<repo_folder>/` directory is created
// 0755 and the lock file is opened O_CREAT|O_RDWR mode 0644. The mode is
// requested rather than enforced — a file another tool already created keeps its
// own mode, and umask applies to the one we create — which is correct: the lock
// is an interlock, not a permission boundary.
//
// It returns ErrLocked when another process holds it, so the caller can apply
// the retry policy rather than treating a busy lock as an error.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, fmt.Errorf("hf/cache: create the lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, FileMode)
	if err != nil {
		return nil, fmt.Errorf("hf/cache: open the lock file: %w", err)
	}
	// LOCK_NB, always. See the constants above.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("hf/cache: flock %s: %w", path, err)
	}
	return &Lock{f: f, path: path}, nil
}

// AcquireWait applies section 7.2a's policy: try, and on ErrLocked retry every
// LockRetryEvery until the context is done or timeout elapses.
//
// waiting, when non-nil, is called once — on the first refusal, before any
// sleep — so the caller can write `last_error='waiting_for_lock'` and let the UI
// say who it is waiting for. It is called at most once per acquisition, because
// a message that repeats every second is a log, not a status.
func AcquireWait(ctx context.Context, path string, timeout time.Duration, waiting func()) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	notified := false
	for {
		l, err := Acquire(path)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		if !notified {
			notified = true
			if waiting != nil {
				waiting()
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s: waited %s", ErrLocked, path, timeout)
		}
		t := time.NewTimer(LockRetryEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w: %s: canceled while waiting", ErrLocked, path)
		case <-t.C:
		}
	}
}

// Path is the lock file this Lock holds.
func (l *Lock) Path() string { return l.path }

// Release drops the lock and closes the descriptor.
//
// The lock FILE is deliberately left on disk. `huggingface_hub` leaves it too,
// and unlinking a lock file under a concurrent acquirer is a classic race: the
// other process would be holding a lock on an inode with no name, and the next
// acquirer would create a fresh file and lock nothing. The nightly maintenance
// pass is what removes `.lock` files older than seven days with no holder.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Closing the descriptor releases the flock; doing it explicitly first keeps
	// the release ordered even if Close is somehow deferred by the runtime.
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("hf/cache: unlock %s: %w", l.path, err)
	}
	return cerr
}

// StaleLockAge is how old an unheld `.lock` file must be before the nightly
// maintenance pass removes it (section 7.2a).
const StaleLockAge = 7 * 24 * time.Hour

// SweepStaleLocks removes `.lock` files under `<hub>/.locks` that are older than
// StaleLockAge AND that nothing currently holds, and reports how many it
// removed. It is the maintenance half of "the file is not removed on release".
//
// "Nothing holds it" is established the only way it can be: by taking the lock
// non-blockingly and removing the file while still holding it. A file another
// process is using is therefore never removed, and a file this pass removes
// cannot be acquired by anyone in between.
func SweepStaleLocks(hub string, now time.Time) (int, error) {
	locks := NewLayout(hub).LocksDir()
	entries, err := os.ReadDir(locks)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("hf/cache: read %s: %w", locks, err)
	}

	removed := 0
	var errs []error
	for _, repo := range entries {
		if !repo.IsDir() {
			continue
		}
		dir := filepath.Join(locks, repo.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != LockSuffix {
				continue
			}
			info, err := f.Info()
			if err != nil || now.Sub(info.ModTime()) < StaleLockAge {
				continue
			}
			path := filepath.Join(dir, f.Name())
			l, err := Acquire(path)
			if err != nil {
				// Held, or unopenable. Either way it is not ours to remove.
				continue
			}
			if err := os.Remove(path); err == nil {
				removed++
			}
			l.Release()
		}
	}
	return removed, errors.Join(errs...)
}
