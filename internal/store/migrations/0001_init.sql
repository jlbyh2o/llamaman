-- 0001_init: the complete v1 schema of DESIGN section 2.
--
-- Every table is STRICT, ids are TEXT ULIDs, singletons are `id INTEGER PRIMARY KEY CHECK (id = 1)`
-- and timestamps are INTEGER Unix milliseconds, UTC. The DDL below is the DDL of section 2,
-- verbatim, including the comments that carry the reasoning; only the ORDER of the blocks differs
-- from the document's, so that every REFERENCES target exists before the table that names it and
-- the file applies cleanly under `foreign_keys=ON`. The section each block comes from is named
-- above it, and blocks that move (build_lease from 2.3, bench_lease from 2.10) say why.
--
-- The migration runner applies this file inside one transaction and records its version, name and
-- sha256 in `schema_migrations`; a later checksum mismatch is a fatal boot error.

------------------------------------------------------------------------------------------------
-- 2.1 Meta, settings, runtime facts
------------------------------------------------------------------------------------------------

CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at INTEGER NOT NULL
) STRICT;

-- Rows are absent until changed; defaults live in internal/settings, so a fresh DB is a working
-- install. This is the whole of SPEC §3.9's "no config file, ever".
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL CHECK (json_valid(value)),
  updated_at INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT 'admin' CHECK (updated_by IN ('admin','system','wizard'))
) STRICT;

-- Facts about this daemon/host that the CLI must read without an HTTP call.
CREATE TABLE runtime_info (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  daemon_version    TEXT NOT NULL,
  daemon_commit     TEXT NOT NULL,
  pid               INTEGER,
  boot_id           TEXT,                 -- ULID per daemon start; job lease owner
  boot_at           INTEGER,
  host_boot_id      TEXT,                 -- /proc/sys/kernel/random/boot_id, verbatim (D53).
                                          -- WRITTEN IN EXACTLY ONE PLACE: supervisor boot
                                          -- reconciliation step 1 (§5.8). §11.1 step 9 only READS it.
  host_boot_at      INTEGER,              -- the host boot instant: /proc/stat `btime` × 1000 (D74).
                                          -- The anchor for the external→autostart relabel; boot_at
                                          -- above is the DAEMON start and is never used for it.
                                          -- Written beside host_boot_id, by the same single writer.
  ui_bind_addr      TEXT,
  ui_port           INTEGER,              -- ACTUAL port after the walk (§11)
  ui_port_flag      INTEGER,              -- `serve --port N` from the unit, NULL when absent (§11.1)
  ui_url_hint       TEXT,
  service_user      TEXT,
  service_uid       INTEGER,
  service_group     TEXT,
  service_gid       INTEGER,
  systemd_scope     TEXT CHECK (systemd_scope IN ('system','user')),
                                          -- decided by ONE rule, §11.1 step 1: the `serve --scope`
                                          -- flag `install-units` rendered into the unit, else the
                                          -- bus the connection succeeded on. Never guessed per call
                                          -- site; §5.2a, §5.3, §11.1 step 1 and §12 all read it.
  systemd_control   TEXT CHECK (systemd_control IN ('dbus','exec','unavailable')),
  journal_read      TEXT CHECK (journal_read IN ('ok','denied','unavailable')),
                                          -- D77: can this identity actually read the journal?
                                          -- Probed once at boot (§11.1 step 6); 'unavailable' means
                                          -- journalctl itself is absent, 'denied' means it ran and
                                          -- returned nothing for a unit that has messages.
  polkit_ok         INTEGER CHECK (polkit_ok IN (0,1)),         -- NULL in user scope: not
                                                                 -- asked, not denied (§5.2a)
  polkit_detail     TEXT,
  polkit_unit_files INTEGER CHECK (polkit_unit_files IN (0,1)),  -- manage-unit-files granted? (§5.2)
                                                                 -- NULL in user scope, as above
  listener_continuity TEXT CHECK (listener_continuity IN ('fdstore','none')),  -- D58/§9.4
  binary_path       TEXT,                 -- filepath.EvalSymlinks(os.Executable()) — never hardcoded
  hf_hub_dir        TEXT,                 -- read-only mirror of the primary hub directory (§7.2a)
  hf_home           TEXT,                 -- `hf_hub_dir` minus a trailing `/hub`, else NULL (§7.2a)
  state_dir         TEXT,
  schema_version    INTEGER,              -- MAX(schema_migrations.version) after boot migration
  last_heartbeat_at INTEGER
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.2 Identity, sessions, secrets
------------------------------------------------------------------------------------------------

CREATE TABLE admin_account (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  password_hash   TEXT NOT NULL,          -- argon2id encoded string
  password_set_at INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
) STRICT;

-- One-time setup token (D38/D59). Created on first boot when admin_account is empty.
-- The DATABASE never holds the plaintext; the plaintext lives in `token_path` until the claim is
-- stamped, and that file is what `llamaman status` and `install.sh` print (§2.2a).
CREATE TABLE setup_claim (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  token_hash      TEXT NOT NULL,          -- sha256 of a 32-byte base58 token
  token_path      TEXT,                   -- '<state>/setup-token' while it exists; NULL once removed
  created_at      INTEGER NOT NULL,
  claimed_at      INTEGER,
  claimed_from_ip TEXT
) STRICT;

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,          -- ULID; the cookie's public half
  token_hash   TEXT NOT NULL UNIQUE,      -- sha256 of the 32-byte secret half
  csrf_secret  TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  revoked_at   INTEGER,
  ip           TEXT,
  user_agent   TEXT
) STRICT;
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);

CREATE TABLE login_attempts (
  id      TEXT PRIMARY KEY,
  at      INTEGER NOT NULL,
  ip      TEXT NOT NULL,
  success INTEGER NOT NULL CHECK (success IN (0,1)),
  reason  TEXT                            -- bad_password|locked|ok|no_account|bad_setup_token
) STRICT;
CREATE INDEX idx_login_attempts_ip_at ON login_attempts(ip, at);

CREATE TABLE lockouts (
  ip           TEXT PRIMARY KEY,
  locked_until INTEGER NOT NULL,
  strikes      INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL
) STRICT;

-- Ciphertext only; the key is <state_dir>/secret.key (0600). See D46 for the honest limit.
-- Both members are reachable: 'hf_token' through GET/PUT/DELETE /api/v1/hf/token and the wizard's
-- `hf` step, 'github_token' through GET/PUT/DELETE /api/v1/github/token and /settings → Builds
-- (§3.6, §6.2). A value in this CHECK with no endpoint behind it would be dead schema.
CREATE TABLE secrets (
  name         TEXT PRIMARY KEY,          -- 'hf_token' | 'github_token'
  nonce        BLOB NOT NULL,
  ciphertext   BLOB NOT NULL,
  hint         TEXT,                      -- 'hf_…AbC' for display; never the secret
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  valid        INTEGER CHECK (valid IN (0,1)),
  scope_json   TEXT CHECK (scope_json IS NULL OR json_valid(scope_json))
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.3 Jobs — the scheduling spine
------------------------------------------------------------------------------------------------

CREATE TABLE jobs (
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL CHECK (kind IN (
                     'llamacpp_install','llamacpp_activate','llamacpp_delete',
                     'model_download','model_verify','model_delete','cache_scan',
                     'bench_run','self_update','toolchain_probe','maintenance')),
  subject_type     TEXT NOT NULL CHECK (subject_type IN (
                     'llamacpp_version','model','download','cache_scan',
                     'bench_run','self_update','system')),        -- §2.3a
  subject_id       TEXT NOT NULL,
  state            TEXT NOT NULL CHECK (state IN (
                     'queued','leased','running','paused','interrupted',
                     'succeeded','failed','canceled')),
  priority         INTEGER NOT NULL DEFAULT 100,     -- lower runs first
  run_after        INTEGER NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 1,
  lease_owner      TEXT,                             -- runtime_info.boot_id
  lease_expires_at INTEGER,
  cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0,1)),
  idempotency_key  TEXT,                             -- denormalized record only; see idempotency_keys
  progress_json    TEXT CHECK (progress_json IS NULL OR json_valid(progress_json)),
  params_json      TEXT CHECK (params_json IS NULL OR json_valid(params_json)),
  error_code       TEXT,
  error_message    TEXT,
  created_at       INTEGER NOT NULL,
  started_at       INTEGER,
  finished_at      INTEGER
) STRICT;
CREATE INDEX idx_jobs_ready   ON jobs(state, run_after, priority);
CREATE INDEX idx_jobs_subject ON jobs(subject_type, subject_id);
-- At most one live job per subject, enforced by the DB rather than by convention (D39):
CREATE UNIQUE INDEX idx_jobs_one_live_per_subject
  ON jobs(subject_type, subject_id)
  WHERE state IN ('queued','leased','running','paused','interrupted');

-- Idempotency-Key replay window (D65). Deliberately NOT a unique index on jobs.idempotency_key:
-- that index is permanent and global, which cannot express a 10-minute window — after the window
-- the same key must be allowed to create a new job, and a client reusing one fixed key across days
-- would collide forever.
CREATE TABLE idempotency_keys (
  key        TEXT PRIMARY KEY,
  route      TEXT NOT NULL,           -- method + pattern; the same key on a different route is a 422
  request_fingerprint TEXT NOT NULL,  -- sha256 of the canonicalized request body
  job_id     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL         -- created_at + 600_000 ms
) STRICT;
CREATE INDEX idx_idem_expiry ON idempotency_keys(expires_at);

------------------------------------------------------------------------------------------------
-- 2.4 Toolchain and hardware
------------------------------------------------------------------------------------------------

CREATE TABLE toolchain_probes (
  id          TEXT PRIMARY KEY,
  at          INTEGER NOT NULL,
  ok_cpu      INTEGER NOT NULL CHECK (ok_cpu  IN (0,1)),
  ok_cuda     INTEGER NOT NULL CHECK (ok_cuda IN (0,1)),
  result_json TEXT NOT NULL CHECK (json_valid(result_json)),
  summary     TEXT
) STRICT;
CREATE INDEX idx_toolchain_at ON toolchain_probes(at DESC);

CREATE TABLE gpus (
  id                 TEXT PRIMARY KEY,
  uuid               TEXT NOT NULL UNIQUE,   -- stable across reboots
  gpu_index          INTEGER NOT NULL,
  name               TEXT NOT NULL,
  vram_total_bytes   INTEGER NOT NULL,
  compute_capability TEXT,                   -- '8.9'
  pci_bus_id         TEXT,
  driver_version     TEXT,
  cuda_version       TEXT,
  first_seen_at      INTEGER NOT NULL,
  last_seen_at       INTEGER NOT NULL,
  present            INTEGER NOT NULL DEFAULT 1 CHECK (present IN (0,1))
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.5 llama.cpp versions
------------------------------------------------------------------------------------------------

CREATE TABLE llamacpp_versions (
  id                 TEXT PRIMARY KEY,      -- '<tag>-<backend>-<acq>' (D60), e.g. 'b10621-cuda-src'
  channel            TEXT NOT NULL CHECK (channel IN ('stable','nightly','custom')),
  tag                TEXT NOT NULL,         -- 'v0.3.0' | 'b10621' | 'fork-<short>'
  build_tag          TEXT,                  -- for stable: the b#### from nightly-tag.txt
  git_url            TEXT NOT NULL DEFAULT 'https://github.com/ggml-org/llama.cpp',
  git_ref            TEXT,
  resolved_commit    TEXT,                  -- 40-hex, filled after checkout
  acquisition        TEXT NOT NULL CHECK (acquisition IN ('prebuilt','source')),
  backend            TEXT NOT NULL CHECK (backend IN ('cpu','cuda')),
  build_options_json TEXT CHECK (build_options_json IS NULL OR json_valid(build_options_json)),
  cuda_arch_list     TEXT,
  host_cpu_flags     TEXT,                  -- for the GGML_NATIVE rebuild-recommended check
  gpu_uuids_json     TEXT CHECK (gpu_uuids_json IS NULL OR json_valid(gpu_uuids_json)),
  dir_name           TEXT NOT NULL UNIQUE,  -- == id (D60)
  superseded_by      TEXT REFERENCES llamacpp_versions(id) ON DELETE SET NULL,
                                            -- a failed_verification prebuilt points at the source
                                            -- build D18 enqueued in its place
  state              TEXT NOT NULL CHECK (state IN (
                       'pending','resolving','fetching','building','verifying',
                       'ready','failed','failed_verification','canceled','deleting','deleted')),
  is_active          INTEGER NOT NULL DEFAULT 0 CHECK (is_active IN (0,1)),
  activated_at       INTEGER,
  previous_active    INTEGER NOT NULL DEFAULT 0 CHECK (previous_active IN (0,1)),
  size_bytes         INTEGER,
  binaries_json      TEXT CHECK (binaries_json IS NULL OR json_valid(binaries_json)),
  devices_output     TEXT,                  -- llama-server --list-devices, verbatim (D19)
  help_flags_json    TEXT CHECK (help_flags_json IS NULL OR json_valid(help_flags_json)),
                                            -- the SET of long/short flag names parsed out of the
                                            -- `llama-server --help` capture at install time. The
                                            -- capture itself stays in manifest.json; this is the
                                            -- queryable projection, so the flag-churn guard (§5.7)
                                            -- is a pure function over ROWS and never reads a file.
  supports_fit       INTEGER NOT NULL DEFAULT 1 CHECK (supports_fit IN (0,1)),
                                            -- derived from help_flags_json at install time: does
                                            -- this build know `--fit`? RenderArgv reads THIS column
                                            -- to decide how `-ngl auto` renders (D51, §5.7), which
                                            -- is what keeps the renderer pure.
  log_path           TEXT,
  exit_code          INTEGER,
  error_code         TEXT,
  error_message      TEXT,
  failing_step       TEXT,                  -- preflight|space|fetch|configure|compile|install|verify
  created_at         INTEGER NOT NULL,
  started_at         INTEGER,
  finished_at        INTEGER
) STRICT;
CREATE UNIQUE INDEX idx_llamacpp_one_active ON llamacpp_versions(is_active)       WHERE is_active = 1;
CREATE UNIQUE INDEX idx_llamacpp_one_prev   ON llamacpp_versions(previous_active) WHERE previous_active = 1;
-- Identity is three-part (D60): the same upstream tag may legitimately exist on one host as a CPU
-- prebuilt, a CPU source build (the D18 fallback for that very prebuilt) and a CUDA source build.
CREATE UNIQUE INDEX idx_llamacpp_identity ON llamacpp_versions(tag, backend, acquisition);
-- Rollback depth is 0 or 1 by construction: SPEC §5.6 asks for "exactly the previous build", so
-- `llamacpp.keep_previous` is a BOOL, not a depth. The unique index and the single
-- `versions/previous` symlink are the schema-level statement of that. A retention list of N would
-- need both to go; it is deliberately not in v1.

CREATE TABLE release_cache (
  id           TEXT PRIMARY KEY,
  source       TEXT NOT NULL CHECK (source IN ('llamacpp','llamaman')),
  tag          TEXT NOT NULL,
  name         TEXT,
  prerelease   INTEGER NOT NULL DEFAULT 0 CHECK (prerelease IN (0,1)),
  published_at INTEGER,
  body_md      TEXT,
  body_html    TEXT,                        -- rendered once, server-side (D35)
  assets_json  TEXT CHECK (assets_json IS NULL OR json_valid(assets_json)),
  nightly_tag  TEXT,
  fetched_at   INTEGER NOT NULL,
  etag         TEXT,
  UNIQUE(source, tag)
) STRICT;

-- From 2.3, placed here because it REFERENCES llamacpp_versions.
-- Global build exclusivity (D70). `idx_jobs_one_live_per_subject` is per SUBJECT: two
-- `llamacpp_install` jobs on two different version ids are legal under it, so it cannot express
-- "one build at a time" and §6.5 must not credit it with doing so. This singleton can.
CREATE TABLE build_lease (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  job_id      TEXT REFERENCES jobs(id) ON DELETE SET NULL,
  version_id  TEXT REFERENCES llamacpp_versions(id) ON DELETE SET NULL,
  owner       TEXT,                    -- runtime_info.boot_id of the holding daemon
  acquired_at INTEGER,
  expires_at  INTEGER                  -- heartbeat horizon; a lapsed lease is reclaimable
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.6 Hugging Face cache, models, files
------------------------------------------------------------------------------------------------

CREATE TABLE hf_cache_roots (
  id            TEXT PRIMARY KEY,
  path          TEXT NOT NULL UNIQUE,       -- the HUB directory itself, whatever it is called.
                                            -- Usually <HF_HOME>/hub, but an HF_HUB_CACHE root is a
                                            -- hub dir with no /hub suffix at all (§7.2) — nothing
                                            -- may assume the suffix.
  is_primary    INTEGER NOT NULL DEFAULT 0 CHECK (is_primary IN (0,1)),
  writable      INTEGER NOT NULL DEFAULT 0 CHECK (writable IN (0,1)),
  symlinks_ok   INTEGER NOT NULL DEFAULT 1 CHECK (symlinks_ok IN (0,1)),  -- F17 probe
  detected_from TEXT CHECK (detected_from IN (
                  'HF_HUB_CACHE','HF_HOME','XDG_CACHE_HOME','default',
                  'legacy_env','dedicated_user','manual','setting')),
  fs_type       TEXT,
  total_bytes   INTEGER,
  free_bytes    INTEGER,
  last_scan_at  INTEGER,
  created_at    INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX idx_cache_one_primary ON hf_cache_roots(is_primary) WHERE is_primary = 1;
-- Exactly one primary root always exists (created on first boot from the detection chain). It is
-- the only root Llama Man ever WRITES to; every other root is read/scan/serve only. §7.2a is the
-- single service method that keeps this table, settings['hf.hub_dir'], the derived
-- settings['hf.home'] and runtime_info.hf_hub_dir/hf_home in agreement — nothing else may write any
-- of them. Note `path` is the hub dir itself and need NOT end in /hub (§7.2 rule 1).

-- One row per LOGICAL model: repo + revision + quant, possibly spanning shards.
CREATE TABLE models (
  id                  TEXT PRIMARY KEY,
  root_id             TEXT NOT NULL REFERENCES hf_cache_roots(id) ON DELETE CASCADE,
  repo_id             TEXT NOT NULL,
  revision            TEXT NOT NULL,        -- resolved commit sha, never 'main'
  ref_name            TEXT,
  quant_label         TEXT,
  kind                TEXT NOT NULL CHECK (kind IN ('text','embedding','mmproj','unknown')),
  state               TEXT NOT NULL CHECK (state IN (
                        'planned','downloading','incomplete','verifying','ready',
                        'corrupt','missing','deleting','deleted')),
  origin              TEXT NOT NULL CHECK (origin IN ('llamaman','scanned')),
  snapshot_dir        TEXT NOT NULL,
  primary_file        TEXT NOT NULL,        -- shard 1, or the single file
  shard_count         INTEGER NOT NULL DEFAULT 1,
  total_bytes         INTEGER NOT NULL DEFAULT 0,
  bytes_on_disk       INTEGER NOT NULL DEFAULT 0,
  mmproj_model_id     TEXT REFERENCES models(id) ON DELETE SET NULL,
  mmproj_auto         INTEGER NOT NULL DEFAULT 1 CHECK (mmproj_auto IN (0,1)),
  gguf_parsed_at      INTEGER,
  arch                TEXT,
  n_layer             INTEGER,
  n_ctx_train         INTEGER,
  n_embd              INTEGER,
  n_ff                INTEGER,
  n_head              INTEGER,
  n_head_kv_json      TEXT,                 -- scalar or per-layer array, verbatim (D30)
  head_dim_k          INTEGER,
  head_dim_v          INTEGER,
  n_vocab             INTEGER,
  n_expert            INTEGER,
  n_expert_used       INTEGER,
  swa_window          INTEGER,              -- {arch}.attention.sliding_window, NULL if absent
  swa_pattern         INTEGER,              -- {arch}.attention.sliding_window_pattern, default 1
  tokenizer_model     TEXT,                 -- for D34 draft validation
  file_type           TEXT,
  has_vision          INTEGER NOT NULL DEFAULT 0 CHECK (has_vision IN (0,1)),
  metadata_json       TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
  tensor_summary_json TEXT CHECK (tensor_summary_json IS NULL OR json_valid(tensor_summary_json)),
  hf_gguf_json        TEXT CHECK (hf_gguf_json IS NULL OR json_valid(hf_gguf_json)),
  card_fetched_at     INTEGER,
  last_verified_at    INTEGER,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  UNIQUE(root_id, repo_id, revision, primary_file)
) STRICT;
CREATE INDEX idx_models_state ON models(state);
CREATE INDEX idx_models_repo  ON models(repo_id);

CREATE TABLE model_files (
  id                TEXT PRIMARY KEY,
  model_id          TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  filename          TEXT NOT NULL,
  shard_index       INTEGER NOT NULL DEFAULT 1,
  shard_total       INTEGER NOT NULL DEFAULT 1,
  size_bytes        INTEGER NOT NULL,       -- lfs.size before download; stat after
  etag              TEXT,                   -- x-linked-etag; == sha256 hex for LFS objects
  blob_path         TEXT,
  link_path         TEXT,
  bytes_on_disk     INTEGER NOT NULL DEFAULT 0,
  state             TEXT NOT NULL CHECK (state IN (
                      'planned','downloading','paused','verifying','present','missing','corrupt')),
  checksum_verified INTEGER NOT NULL DEFAULT 0 CHECK (checksum_verified IN (0,1)),
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  UNIQUE(model_id, filename)
) STRICT;

CREATE TABLE stray_files (
  id            TEXT PRIMARY KEY,
  root_id       TEXT NOT NULL REFERENCES hf_cache_roots(id) ON DELETE CASCADE,
  path          TEXT NOT NULL UNIQUE,
  size_bytes    INTEGER NOT NULL,
  reason        TEXT NOT NULL,   -- outside_snapshot|orphan_blob|broken_symlink|unparsable
  first_seen_at INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL,
  dismissed_at  INTEGER
) STRICT;

CREATE TABLE cache_scans (
  id             TEXT PRIMARY KEY,
  root_id        TEXT NOT NULL REFERENCES hf_cache_roots(id) ON DELETE CASCADE,
  state          TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','canceled')),
  trigger        TEXT NOT NULL,  -- boot|wizard|manual|post_download
  dirs_seen      INTEGER NOT NULL DEFAULT 0,
  files_seen     INTEGER NOT NULL DEFAULT 0,
  models_found   INTEGER NOT NULL DEFAULT 0,
  models_added   INTEGER NOT NULL DEFAULT 0,
  models_missing INTEGER NOT NULL DEFAULT 0,
  strays_found   INTEGER NOT NULL DEFAULT 0,
  bytes_total    INTEGER NOT NULL DEFAULT 0,
  error_message  TEXT,
  started_at     INTEGER,
  finished_at    INTEGER,
  created_at     INTEGER NOT NULL
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.7 Downloads
------------------------------------------------------------------------------------------------

CREATE TABLE downloads (
  id             TEXT PRIMARY KEY,
  model_id       TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  state          TEXT NOT NULL CHECK (state IN (
                   'queued','resolving','running','paused','verifying',
                   'succeeded','failed','canceled')),
  priority       INTEGER NOT NULL DEFAULT 100,
  include_mmproj INTEGER NOT NULL DEFAULT 1 CHECK (include_mmproj IN (0,1)),
  bytes_total    INTEGER NOT NULL DEFAULT 0,
  bytes_done     INTEGER NOT NULL DEFAULT 0,
  bytes_at_start INTEGER NOT NULL DEFAULT 0,  -- resumed-from offset, for an honest ETA
  speed_bps      INTEGER NOT NULL DEFAULT 0,
  eta_sec        INTEGER,
  attempts       INTEGER NOT NULL DEFAULT 0,
  error_code     TEXT,
  error_message  TEXT,
  created_at     INTEGER NOT NULL,
  started_at     INTEGER,
  finished_at    INTEGER
) STRICT;
CREATE INDEX idx_downloads_state ON downloads(state, priority, created_at);

CREATE TABLE download_tasks (
  id              TEXT PRIMARY KEY,
  download_id     TEXT NOT NULL REFERENCES downloads(id) ON DELETE CASCADE,
  model_file_id   TEXT NOT NULL REFERENCES model_files(id) ON DELETE CASCADE,
  url             TEXT NOT NULL,            -- huggingface.co resolve URL, never a signed CDN URL
  state           TEXT NOT NULL CHECK (state IN (
                    'queued','running','paused','verifying','succeeded','failed','canceled')),
  bytes_total     INTEGER NOT NULL DEFAULT 0,
  bytes_done      INTEGER NOT NULL DEFAULT 0,
  etag            TEXT,                     -- BLOB NAME: x-linked-etag, de-quoted, W/ stripped.
                                            -- Never sent in a header. == sha256 hex for LFS objects.
  validator       TEXT,                     -- HTTP VALIDATOR: the byte-exact `ETag` header from the
                                            -- final response of the last successful transfer of this
                                            -- URL, quotes and any W/ prefix included. Sent as
                                            -- `If-Range` only when strong. NULL = do not send one.
  validator_host  TEXT,                     -- host that issued `validator`; a change invalidates it
  last_modified   TEXT,                     -- fallback `If-Range` validator, verbatim
  incomplete_path TEXT,                     -- blobs/<etag>.incomplete
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  started_at      INTEGER,
  finished_at     INTEGER,
  UNIQUE(download_id, model_file_id)
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.8 Instances
------------------------------------------------------------------------------------------------

CREATE TABLE instances (
  id                 TEXT PRIMARY KEY,
  name               TEXT NOT NULL
                       CHECK (length(name) BETWEEN 1 AND 32
                              AND name GLOB '[a-z0-9]*'
                              AND name NOT GLOB '*[^a-z0-9-]*'),      -- D11
  display_name       TEXT,
  description        TEXT,
  model_id           TEXT REFERENCES models(id) ON DELETE RESTRICT,
  mmproj_model_id    TEXT REFERENCES models(id) ON DELETE RESTRICT,
  draft_model_id     TEXT REFERENCES models(id) ON DELETE RESTRICT,
  public_port        INTEGER NOT NULL CHECK (public_port   BETWEEN 1024 AND 65535),
  internal_port      INTEGER NOT NULL CHECK (internal_port BETWEEN 1024 AND 65535),
  auth_mode          TEXT NOT NULL DEFAULT 'token' CHECK (auth_mode IN ('token','none')),
  autostart          INTEGER NOT NULL DEFAULT 0 CHECK (autostart IN (0,1)),  -- == unit enabled
  restart_policy     TEXT NOT NULL DEFAULT 'on-failure'
                       CHECK (restart_policy IN ('always','on-failure','never')),
  restart_max        INTEGER NOT NULL DEFAULT 5,
  restart_window_sec INTEGER NOT NULL DEFAULT 600,
  flags_json         TEXT NOT NULL CHECK (json_valid(flags_json)),   -- model.FlagSet (D41)
  extra_flags        TEXT NOT NULL DEFAULT '',
  config_hash        TEXT NOT NULL,         -- sha256 over rendered argv (listener identity elided —
                                            -- D52) + resolved model paths + active version id
  desired_state      TEXT NOT NULL DEFAULT 'stopped'
                       CHECK (desired_state IN ('running','stopped')),
  draft_validation   TEXT NOT NULL DEFAULT 'ok'
                       CHECK (draft_validation IN ('ok','deferred','mismatch')),  -- D34, §3.10a
  pending_trigger    TEXT CHECK (pending_trigger IS NULL OR pending_trigger IN (
                       'user','autostart','supervisor_restart','rolling','bench_restore',
                       'safe_start')),
                                            -- stamped by the daemon before StartUnit, consumed and
                                            -- cleared by instance-exec (§5.6 step 3)
  pending_override_json TEXT CHECK (pending_override_json IS NULL
                                    OR json_valid(pending_override_json)),
                                            -- D61: a TRANSIENT FlagSet patch for exactly the next
                                            -- start, written beside pending_trigger and cleared in
                                            -- the same transaction that consumes it. Never part of
                                            -- flags_json, never in config_hash, never a generation
                                            -- bump. Today its only producer is safe-start (§3.10b).
  unit_name          TEXT NOT NULL,
  generation         INTEGER NOT NULL DEFAULT 1,   -- optimistic concurrency for PATCH
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL,
  deleted_at         INTEGER
) STRICT;

-- Deletion is SOFT (D68), so uniqueness is scoped to live rows. A plain UNIQUE would consume the
-- name and both ports forever the first time an instance was deleted, with no documented way to get
-- any of them back — and a HARD delete would cascade instance_starts, instance_usage_daily and
-- token_usage_daily away, discarding accounting this document calls a product feature (§2.9).
CREATE UNIQUE INDEX idx_instances_name          ON instances(name)          WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_instances_public_port   ON instances(public_port)   WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_instances_internal_port ON instances(internal_port) WHERE deleted_at IS NULL;
CREATE INDEX        idx_instances_live          ON instances(deleted_at);

-- Observed reality, separated so high-frequency writes never touch the config row.
-- NOTE: `stale-version` and `inhibited` are NOT states — an instance that is serving traffic cannot
-- also be in a state that excludes it from every ready-gated behavior. Both are derived flags,
-- computed exactly like `restart_required`; see the derivation block after the transition table.
CREATE TABLE instance_status (
  instance_id         TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  state               TEXT NOT NULL CHECK (state IN (
                        'unknown','stopped','starting','loading','ready','degraded',
                        'stopping','failed','crash-looping')),
  systemd_active      TEXT,
  systemd_sub         TEXT,
  systemd_result      TEXT,
  main_pid            INTEGER,
  exe_version_id      TEXT,                 -- from readlink /proc/<pid>/exe (D25)
  applied_config_hash TEXT,
  ready_at            INTEGER,
  last_change_at      INTEGER NOT NULL,
  last_health_at      INTEGER,
  health_code         INTEGER,
  slots_total         INTEGER,
  slots_busy          INTEGER,
  ctx_size            INTEGER,
  requests_served     INTEGER,              -- from /metrics; NULL when metrics_endpoint is off (§2.9)
  rss_bytes           INTEGER,
  vram_bytes          INTEGER,              -- Σ over the attribution rows for MainPID (D17, §8.6)
  gpu_uuids_json      TEXT CHECK (gpu_uuids_json IS NULL OR json_valid(gpu_uuids_json)),
                                            -- GPU UUIDs this PID actually holds memory on; the
                                            -- bench exclusivity guard (§10) reads exactly this
  gpu_attribution     TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (gpu_attribution IN ('measured','declared','unknown')),
                                            -- how gpu_uuids_json was obtained (§8.6); the guard
                                            -- treats anything but 'measured' conservatively
  fit_report_json     TEXT CHECK (fit_report_json IS NULL OR json_valid(fit_report_json)),
  last_exit_code      INTEGER,
  last_error          TEXT,
  reconcile_backoff_until INTEGER,
  restart_window_reset_at INTEGER NOT NULL DEFAULT 0,
                                            -- D64: crash-loop counting ignores instance_starts rows
                                            -- at or before this instant. Stamped by
                                            -- POST /instances/{id}/reset-failed and by a start that
                                            -- served for >= 60 s.
  device_map_json     TEXT CHECK (device_map_json IS NULL OR json_valid(device_map_json))
                                            -- the uuid -> CUDA<n> map observed at the last ready
                                            -- transition; a change from the map recorded in
                                            -- flags_json raises F22 (§5.7)
) STRICT;

-- Every launch attempt: the forensic record and the input to restart policy (D7/D54).
-- The row is OPENED before preflight and CLOSED on every exit path — see §5.6 and the writer table.
CREATE TABLE instance_starts (
  id                 TEXT PRIMARY KEY,
  instance_id        TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  at                 INTEGER NOT NULL,
  trigger            TEXT NOT NULL CHECK (trigger IN (
                       'user','autostart','supervisor_restart','rolling','bench_restore',
                       'safe_start','external')),
                                            -- 'external': systemctl start by hand, or a boot start
                                            -- of an enabled unit that the daemon did not stamp
  config_hash        TEXT NOT NULL,         -- instances.config_hash at the moment of the attempt
  effective_config_hash TEXT,               -- hash of what was ACTUALLY rendered: == config_hash
                                            -- normally, and the override hash for a safe start
                                            -- (D61). NULL until argv rendering succeeds.
  override_json      TEXT CHECK (override_json IS NULL OR json_valid(override_json)),
                                            -- the consumed pending_override_json, so the history
                                            -- shows that this run was not the saved configuration
  argv_json          TEXT CHECK (argv_json IS NULL OR json_valid(argv_json)),
                                            -- NULL until §5.7 rendering succeeds, so a row exists
                                            -- for preflight failures that never got that far
  llamacpp_version_id TEXT REFERENCES llamacpp_versions(id),
  ready_at           INTEGER,               -- first /health 200. Stamped by the supervisor; it does
                                            -- NOT close the row (D63) — the run is still in flight.
  outcome            TEXT CHECK (outcome IN ('failed','inhibited','stopped')),
                                            -- NULL = run in flight. Written EXACTLY ONCE, at the
                                            -- end of the run. 'ready' is deliberately not a member:
                                            -- reaching ready is an event within a run, not the end
                                            -- of one (D63). 'inhibited' rows describe a REFUSAL to
                                            -- start rather than a run, so they are excluded from
                                            -- LAST_CLOSED and from the crash-loop count (§2.8, D64).
  exit_code          INTEGER,               -- launcher exit code (§5.6 table) or ExecMainStatus
  error_code         TEXT,                  -- 'model_missing','port_conflict','bad_flags',…
                                            -- on an 'inhibited' row it carries the inhibit_reason
                                            -- ('policy_never'|'crash_loop'|'clean_exit'), which is
                                            -- what makes the one-row-per-episode rule of §2.8 a
                                            -- query rather than a memory
  error_message      TEXT,
  detail_json        TEXT CHECK (detail_json IS NULL OR json_valid(detail_json)),
                                            -- e.g. {"internal_port":21001,"public_port":8081,
                                            --       "conflict_pid":9134} for exit 78 (F5)
  ended_at           INTEGER
) STRICT;
CREATE INDEX idx_instance_starts ON instance_starts(instance_id, at DESC);
-- UNIQUE, not a plain partial index (D40). Three passages read this index as the ENFORCEMENT of
-- at-most-one-open-row per instance — §2.8's THE_OPEN_ROW, `restart_required`, and §5.6's closing
-- rule — and `restart_required` is undefined the moment two open rows exist. That is reachable
-- whenever a launcher's row-closing UPDATE fails against a locked or unwritable DB (§5.6) and the
-- supervisor then opens a new run, or when a hand-run `systemctl start` races the supervisor. The
-- database rejects it instead: `instance-exec` closes any surviving open row inside its own step-3
-- transaction before inserting (§5.6), so the insert can never be the thing that fails.
CREATE UNIQUE INDEX idx_instance_starts_open ON instance_starts(instance_id) WHERE outcome IS NULL;
-- The crash-loop query (D64) reads exactly this index:
CREATE INDEX idx_instance_starts_failed
  ON instance_starts(instance_id, at DESC) WHERE outcome = 'failed';

CREATE TABLE flag_presets (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  description TEXT,
  flags_json  TEXT NOT NULL CHECK (json_valid(flags_json)),
  extra_flags TEXT NOT NULL DEFAULT '',
  builtin     INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0,1)),
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.9 Tokens and accounting
------------------------------------------------------------------------------------------------

CREATE TABLE api_tokens (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  prefix        TEXT NOT NULL,             -- 'lm_' + first 6 chars, for display and log correlation
  token_hash    TEXT NOT NULL UNIQUE,      -- sha256(secret) — D37
  scope         TEXT NOT NULL DEFAULT 'global' CHECK (scope IN ('global','instances')),
  state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','disabled','revoked')),
  rate_limit_rpm INTEGER,
  expires_at    INTEGER,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  revoked_at    INTEGER,
  last_used_at  INTEGER,
  last_used_ip  TEXT,
  request_count INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE token_instances (
  token_id    TEXT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  PRIMARY KEY (token_id, instance_id)
) STRICT;

-- Gateway traffic, counted for EVERY proxied request including auth_mode='none' (D56).
-- This is the table SPEC §3.3's "requests served" and the per-instance dashboard read from.
CREATE TABLE instance_usage_daily (
  instance_id       TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  day               TEXT NOT NULL,          -- 'YYYY-MM-DD' UTC
  auth_mode         TEXT NOT NULL CHECK (auth_mode IN ('token','none')),
  requests          INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  bytes_in          INTEGER NOT NULL DEFAULT 0,
  bytes_out         INTEGER NOT NULL DEFAULT 0,
  duration_ms       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (instance_id, day, auth_mode)
) STRICT;

-- The per-token BREAKDOWN of the same traffic. Written only when a credential was presented, so it
-- is a subset of instance_usage_daily, never the whole of it.
CREATE TABLE token_usage_daily (
  token_id          TEXT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
  instance_id       TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  day               TEXT NOT NULL,          -- 'YYYY-MM-DD' UTC
  requests          INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  bytes_in          INTEGER NOT NULL DEFAULT 0,
  bytes_out         INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER,                -- NULL = not reported by the upstream (D37 note)
  completion_tokens INTEGER,
  duration_ms       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (token_id, instance_id, day)
) STRICT;

CREATE TABLE gateway_denials_daily (
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  day         TEXT NOT NULL,
  reason      TEXT NOT NULL,   -- missing|unknown|disabled|revoked|expired|scope|rate_limited
  count       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (instance_id, day, reason)
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.10 Benchmarks
------------------------------------------------------------------------------------------------

CREATE TABLE bench_runs (
  id                    TEXT PRIMARY KEY,
  name                  TEXT NOT NULL,
  state                 TEXT NOT NULL CHECK (state IN (
                          'draft','queued','preflight','running','succeeded',
                          'partial','failed','canceled')),
  model_id              TEXT REFERENCES models(id) ON DELETE SET NULL,
  model_label           TEXT NOT NULL,      -- denormalized: history outlives the model
  model_path            TEXT NOT NULL,
  quant_label           TEXT,
  llamacpp_version_id   TEXT REFERENCES llamacpp_versions(id) ON DELETE SET NULL,
  llamacpp_tag          TEXT NOT NULL,
  llamacpp_commit       TEXT,
  llamacpp_backend      TEXT NOT NULL,
  gpu_json              TEXT NOT NULL CHECK (json_valid(gpu_json)),
  host_json             TEXT NOT NULL CHECK (json_valid(host_json)),
  sweep_json            TEXT NOT NULL CHECK (json_valid(sweep_json)),
  repetitions           INTEGER NOT NULL DEFAULT 3,
  points_total          INTEGER NOT NULL DEFAULT 0,
  points_done           INTEGER NOT NULL DEFAULT 0,
  points_failed         INTEGER NOT NULL DEFAULT 0,
  stopped_instances_json TEXT CHECK (stopped_instances_json IS NULL
                                     OR json_valid(stopped_instances_json)),
  restore_done          INTEGER NOT NULL DEFAULT 0 CHECK (restore_done IN (0,1)),
  error_message         TEXT,
  notes                 TEXT,
  created_at            INTEGER NOT NULL,
  started_at            INTEGER,
  finished_at           INTEGER
) STRICT;

-- One row per cross-product cell, created BEFORE execution: exact progress, exact resume.
CREATE TABLE bench_points (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL REFERENCES bench_runs(id) ON DELETE CASCADE,
  ordinal       INTEGER NOT NULL,
  state         TEXT NOT NULL CHECK (state IN ('pending','running','succeeded','failed','skipped')),
  args_json     TEXT NOT NULL CHECK (json_valid(args_json)),
  n_gpu_layers  INTEGER, n_batch INTEGER, n_ubatch INTEGER, n_threads INTEGER,
  flash_attn    INTEGER, type_k TEXT, type_v TEXT,
  split_mode    TEXT, tensor_split TEXT, n_depth INTEGER,
  started_at    INTEGER, finished_at INTEGER,
  error_message TEXT,
  UNIQUE(run_id, ordinal)
) STRICT;

CREATE TABLE bench_results (
  id           TEXT PRIMARY KEY,
  point_id     TEXT NOT NULL REFERENCES bench_points(id) ON DELETE CASCADE,
  run_id       TEXT NOT NULL REFERENCES bench_runs(id) ON DELETE CASCADE,
  test_kind    TEXT NOT NULL CHECK (test_kind IN ('pp','tg','pp+tg')),
  n_prompt     INTEGER NOT NULL DEFAULT 0,
  n_gen        INTEGER NOT NULL DEFAULT 0,
  n_depth      INTEGER NOT NULL DEFAULT 0,
  avg_ts       REAL NOT NULL,
  stddev_ts    REAL NOT NULL DEFAULT 0,
  avg_ns       INTEGER NOT NULL DEFAULT 0,
  stddev_ns    INTEGER NOT NULL DEFAULT 0,
  samples_json TEXT CHECK (samples_json IS NULL OR json_valid(samples_json)),
  raw_json     TEXT NOT NULL CHECK (json_valid(raw_json)),   -- llama-bench object, unmodified
  created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_bench_results_run ON bench_results(run_id, test_kind);

-- Global bench exclusivity (D75), the exact counterpart of `build_lease` and for the exact same
-- reason: `idx_jobs_one_live_per_subject` is per SUBJECT, and a bench's subject is its own
-- `bench_runs.id`, so two `bench_run` jobs on two different runs are perfectly legal under it. The
-- index therefore says nothing about "one bench at a time", and §6.6 step 1's "refuse while a bench
-- is live" had no single row to read.
CREATE TABLE bench_lease (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  job_id      TEXT REFERENCES jobs(id)       ON DELETE SET NULL,
  run_id      TEXT REFERENCES bench_runs(id) ON DELETE SET NULL,
  owner       TEXT,                    -- runtime_info.boot_id of the holding daemon
  acquired_at INTEGER,
  expires_at  INTEGER                  -- heartbeat horizon; a lapsed lease is reclaimable
) STRICT;

------------------------------------------------------------------------------------------------
-- 2.11 Self-update, events, notifications, fit calibration, wizard
------------------------------------------------------------------------------------------------

CREATE TABLE self_updates (
  id            TEXT PRIMARY KEY,
  from_version  TEXT NOT NULL,
  to_version    TEXT NOT NULL,
  channel       TEXT NOT NULL,
  -- Eight states, and every one of them is written by a named step of §12 that can reach this
  -- database: the daemon writes the first five, a confirmation gate writes `succeeded`/`failed`,
  -- and `POST /jobs/{id}/cancel` writes `canceled` — accepted only before the `staged` commit,
  -- refused `409 selfupdate_not_cancelable` at or after it (D96, §3.14). There is no state a
  -- privileged actor would have to report through a file, because no privileged actor in this
  -- design has anything to report (D87).
  state         TEXT NOT NULL CHECK (state IN (
                  'planned','downloading','verifying','staged','swapping',
                  'succeeded','failed','canceled')),
  asset_url     TEXT,
  asset_sha256  TEXT,
  signature_ok  INTEGER CHECK (signature_ok IN (0,1)),
  db_backup_path TEXT,                      -- the D14 snapshot taken before this update; restored
                                            -- ONLY by `llamaman restore-db` (§12.4, D90)
  binary_path   TEXT,                       -- the resolved <prefix>/llamaman being replaced (D15)
  error_message TEXT,                       -- the paired job carries the error_code (§2.3a)
  created_at    INTEGER NOT NULL,
  finished_at   INTEGER
) STRICT;

CREATE TABLE events (
  id           TEXT PRIMARY KEY,           -- ULID; also the SSE Last-Event-ID cursor
  at           INTEGER NOT NULL,
  level        TEXT NOT NULL CHECK (level IN ('debug','info','warn','error')),
  category     TEXT NOT NULL,              -- llamacpp|model|download|instance|token|bench|auth|
                                           -- update|system|gateway
  subject_type TEXT,
  subject_id   TEXT,
  action       TEXT NOT NULL,
  from_state   TEXT,
  to_state     TEXT,
  actor        TEXT NOT NULL CHECK (actor IN ('admin','system','systemd','wizard','cli')),
  message      TEXT NOT NULL,
  detail_json  TEXT CHECK (detail_json IS NULL OR json_valid(detail_json))
) STRICT;
CREATE INDEX idx_events_at      ON events(at DESC);
CREATE INDEX idx_events_subject ON events(subject_type, subject_id, at DESC);

-- Things that need a human: a failed canary, a denied polkit grant, a rebuild recommendation.
CREATE TABLE notifications (
  id           TEXT PRIMARY KEY,
  at           INTEGER NOT NULL,
  severity     TEXT NOT NULL CHECK (severity IN ('info','warn','error')),
  code         TEXT NOT NULL,              -- maps to a UI remediation card (§17)
  title        TEXT NOT NULL,
  body         TEXT NOT NULL,
  subject_type TEXT, subject_id TEXT,
  action_json  TEXT CHECK (action_json IS NULL OR json_valid(action_json)),
  dismissed_at INTEGER
) STRICT;

-- Learned corrections for the fit calculator (§8.6).
CREATE TABLE fit_observations (
  id                   TEXT PRIMARY KEY,
  at                   INTEGER NOT NULL,
  arch                 TEXT NOT NULL,
  llamacpp_tag         TEXT NOT NULL,
  backend              TEXT NOT NULL,
  gpu_name             TEXT,
  n_layer INTEGER, n_embd INTEGER, n_head INTEGER, n_head_kv INTEGER, n_vocab INTEGER,
  n_ctx INTEGER, n_batch INTEGER, n_ubatch INTEGER, n_parallel INTEGER,
  flash_attn INTEGER, type_k TEXT, type_v TEXT, n_gpu_layers INTEGER,
  predicted_bytes      INTEGER NOT NULL,
  actual_weights_bytes INTEGER,
  actual_kv_bytes      INTEGER,
  actual_compute_bytes INTEGER,
  actual_total_bytes   INTEGER,
  oom                  INTEGER NOT NULL DEFAULT 0 CHECK (oom IN (0,1)),
  source               TEXT NOT NULL CHECK (source IN ('instance_start','bench','fit_flag'))
) STRICT;
CREATE INDEX idx_fit_obs ON fit_observations(arch, backend, llamacpp_tag);

CREATE TABLE wizard_steps (
  step       TEXT PRIMARY KEY CHECK (step IN (
               'password','toolchain','llamacpp','hf','models','instance','done')),
  state      TEXT NOT NULL CHECK (state IN ('pending','active','skipped','complete')),
  data_json  TEXT CHECK (data_json IS NULL OR json_valid(data_json)),
  updated_at INTEGER NOT NULL
) STRICT;

------------------------------------------------------------------------------------------------
-- Seed rows
------------------------------------------------------------------------------------------------

-- The two singleton leases. §2.3's acquire is an `UPDATE build_lease SET … WHERE id=1 AND (…)`
-- whose "zero rows changed means another build holds it" reading requires the row to exist from the
-- first boot; §2.10's bench_lease is its exact counterpart. Both are seeded unheld — every nullable
-- column NULL — which is the state "no owner, reclaimable by anyone".
INSERT INTO build_lease (id) VALUES (1);
INSERT INTO bench_lease (id) VALUES (1);

-- `settings` is deliberately NOT seeded. §2.1: "Rows are absent until changed; defaults live in
-- internal/settings, so a fresh DB is a working install. This is the whole of SPEC §3.9's 'no
-- config file, ever'." Seeding the registry's defaults here would make every default a stored
-- decision, so a changed default in a later release could never reach an existing install.
-- `wizard_steps`, `runtime_info` and `setup_claim` are likewise unseeded: each is written by the
-- named boot or wizard step that owns it (§11.1 steps 6, 8 and §11.2).
