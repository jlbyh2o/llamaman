// Package models is the local model library service (DESIGN sections 2.6, 3.7,
// 7.2, 7.2a).
//
// It owns the CATALOG: which models this host has, where they are, what their
// GGUF headers say, which instances are using them, and what removing one would
// cost. The filesystem half lives in internal/hf/cache and the SQL in
// internal/store (D49 invariant 1); this package is the guarded transitions
// between the two.
//
// # What it does
//
//   - Scans a hub cache and reconciles it: shards grouped into one logical
//     model, projectors paired, one `models` row per SNAPSHOT DIRECTORY, and
//     rows whose files vanished marked `missing` rather than deleted — a disk
//     may have been unplugged, and the row is how the user finds out which one.
//   - Keeps §7.2a's four representations of the cache location in agreement.
//     SetPrimaryRoot is the ONLY writer of `hf_cache_roots`,
//     `settings['hf.hub_dir']`, the derived `settings['hf.home']` and
//     `runtime_info.hf_hub_dir`/`hf_home`, and it writes all four in one
//     transaction.
//   - Accounts for disk, per cache root, separating what the catalog holds from
//     what the filesystem reports.
//   - Refuses to delete a model an instance is still using, and previews what a
//     delete would free before executing it (D28).
//
// # Two rules run through nearly every method
//
// D69: a model's resolved path is a `config_hash` input. In the SAME transaction
// that writes `snapshot_dir`, `primary_file` or `state`, this service calls
// `instances.RecomputeConfigHash` for every non-deleted instance referencing that
// model. Without it, "queue the download, then configure the instance" ends with
// an instance whose stored hash describes a path that never existed.
//
// §7.2: deleting a model NEVER issues a SQL DELETE. The row moves
// `deleting → deleted` and stays, so `instances.model_id`'s ON DELETE RESTRICT
// is never exercised on that path and a soft-deleted instance keeps a readable
// record of what it once pointed at. The cache-root DETACH of §7.2a is the one
// documented exception, and it carries its own, wider guard.
package models
