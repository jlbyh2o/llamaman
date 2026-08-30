package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/hf/cache"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The startup sweep (DESIGN section 7.4).
//
// "A startup sweep removes `.incomplete` files under OUR OWN repo directories
// with no matching `download_tasks` row — and leaves every other `.incomplete`
// file alone, because it may belong to a concurrent `hf download`."
//
// Both halves of that sentence are load-bearing, and getting either wrong is a
// data-loss bug rather than a tidiness one:
//
//   - "our own repo directories" means a repository this daemon has a `models`
//     row for. A cache shared with `huggingface_hub` holds repositories this
//     product has never heard of, and their partials are somebody else's work in
//     progress.
//   - "no matching `download_tasks` row" counts rows in EVERY state, not the
//     live ones. A paused download's partial is precisely the file the pause
//     exists to keep, and a sweep that filtered on state would delete it on the
//     next boot — turning a pause into a silent restart of a 40 GB transfer.
//
// The sweep additionally takes the D27 lock before removing anything. A partial
// that belongs to no row of ours can still be open in another process right now;
// removing it under that writer leaves it appending to an unlinked inode, which
// is the same forty gigabytes lost by a different route.

// SweepResult is what one sweep did.
type SweepResult struct {
	// Removed is how many orphan partials were deleted.
	Removed int
	// Bytes is what they occupied.
	Bytes int64
	// Skipped is how many were left alone because another process held their
	// lock or because a task row names them.
	Skipped int
}

// Sweep removes orphaned `.incomplete` files under one hub directory.
//
// It is called once at boot, after migrations and before the queue starts, so a
// download that was interrupted by a crash finds its own partial exactly where
// it left it and every other one is gone.
func (s *Service) Sweep(ctx context.Context, hub string) (SweepResult, error) {
	var (
		res   SweepResult
		known map[string]struct{}
		repos map[string]struct{}
	)
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		known, err = s.store.KnownIncompletePaths(ctx, tx)
		if err != nil {
			return err
		}
		repos, err = s.ownRepoDirs(ctx, tx, hub)
		return err
	}); err != nil {
		return res, err
	}

	for dir := range repos {
		blobs := filepath.Join(dir, cache.BlobsDirName)
		entries, err := os.ReadDir(blobs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// One unreadable repository does not stop the sweep: a shared cache
			// legitimately holds directories this identity cannot read, and
			// refusing to boot over one of them would be a worse answer.
			s.log.Warn("hf/download: could not read a blobs directory", "path", blobs, "error", err)
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), cache.IncompleteSuffix) {
				continue
			}
			path := filepath.Join(blobs, e.Name())
			if _, ours := known[path]; ours {
				res.Skipped++
				continue
			}
			info, err := e.Info()
			if err != nil {
				res.Skipped++
				continue
			}

			// Take the lock the writer would hold. A refusal means somebody is
			// writing this file right now, and it is not ours to remove.
			etag := strings.TrimSuffix(e.Name(), cache.IncompleteSuffix)
			repoID, ok := cache.RepoIDFromFolder(filepath.Base(dir))
			if !ok {
				res.Skipped++
				continue
			}
			lock, err := cache.Acquire(cache.LockPath(hub, repoID, etag))
			if err != nil {
				res.Skipped++
				continue
			}
			removeErr := os.Remove(path)
			_ = lock.Release()
			if removeErr != nil && !os.IsNotExist(removeErr) {
				s.log.Warn("hf/download: could not remove an orphan partial",
					"path", path, "error", removeErr)
				res.Skipped++
				continue
			}
			res.Removed++
			res.Bytes += cache.AllocatedBytes(info)
		}
	}
	if res.Removed > 0 {
		s.log.Info("hf/download: swept orphaned partial downloads",
			"removed", res.Removed, "bytes", res.Bytes)
	}
	return res, nil
}

// ownRepoDirs is the "under our own repo directories" half. It reads the
// repositories this daemon has `models` rows for under this hub directory, and
// nothing else.
func (s *Service) ownRepoDirs(ctx context.Context, tx store.Tx, hub string) (map[string]struct{}, error) {
	root, err := s.store.PrimaryCacheRoot(ctx, tx)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(root.Path) != filepath.Clean(hub) {
		// A sweep of a non-primary root is not something this design asks for:
		// downloads only ever write to the primary (section 7.2a), so a partial
		// under any other root is by construction not ours.
		return map[string]struct{}{}, nil
	}

	rows, err := s.store.Downloads(ctx, tx, store.DownloadFilter{})
	if err != nil {
		return nil, err
	}
	layout := cache.NewLayout(hub)
	out := map[string]struct{}{}
	for _, d := range rows {
		m, err := s.store.LocalModel(ctx, tx, d.ModelID)
		if err != nil {
			continue
		}
		out[layout.RepoDir(m.RepoID)] = struct{}{}
	}
	return out, nil
}

// SweepPrimary is Sweep against whatever the primary root currently is — the
// form the composition root calls at boot, since it holds no hub directory of
// its own.
func (s *Service) SweepPrimary(ctx context.Context) (SweepResult, error) {
	var hub string
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := s.store.PrimaryCacheRoot(ctx, tx)
		if err != nil {
			return err
		}
		hub = root.Path
		return nil
	}); err != nil {
		return SweepResult{}, fmt.Errorf("hf/download: reading the primary cache root: %w", err)
	}
	return s.Sweep(ctx, hub)
}
