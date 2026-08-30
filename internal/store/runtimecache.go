package store

import (
	"context"
	"fmt"
)

// SetRuntimeCachePaths refreshes `runtime_info.hf_hub_dir` and `hf_home` — the
// two DISPLAY copies of the cache location (§7.2a).
//
// They exist so `llamaman status` and `llamaman doctor` can print the path
// without an HTTP call, and they are a derived cache and never an input: the
// authority chain is `hf_cache_roots` ← `settings['hf.hub_dir']` ← these two.
// This is a narrow UPDATE rather than a PutRuntimeInfo because §7.2a's
// SetPrimaryRoot writes all four representations in ONE transaction, and
// rewriting the whole singleton there would make a cache relocation clobber
// whatever the boot sequence had just learned about polkit, the journal or the
// port walk.
//
// hfHome is nil whenever the hub directory does not end in `/hub`, which rule 1
// of §7.2 routinely produces. It is stored as SQL NULL rather than as an empty
// string so a reader can tell "there is no HF_HOME for this layout" from "it has
// not been resolved yet".
//
// It reports false when no `runtime_info` row exists yet, which is the state
// before the boot sequence has written one and is not an error: the next boot
// writes both columns from the primary root anyway.
func (s *Store) SetRuntimeCachePaths(ctx context.Context, tx Tx, hubDir string, hfHome *string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE runtime_info SET hf_hub_dir = ?, hf_home = ? WHERE id = 1`, hubDir, hfHome)
	if err != nil {
		return false, fmt.Errorf("set runtime cache paths: %w", err)
	}
	return rowsChanged(res)
}
