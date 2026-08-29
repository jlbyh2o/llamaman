# Llama Man — v1 Engineering Design

Implementation contract for `SPEC.md`. Module path `github.com/jlbyh2o/llamaman`.
Go 1.27 (`CGO_ENABLED=0`), Node 24, one static binary with the React UI embedded via `go:embed`.
Instances are systemd template units. All state is in one SQLite file. No config files, no required
environment variables, no Docker.

This document is the merge of three competing proposals (state-model-first, API-first,
operations-first). The state-model-first proposal is the base; every decision below that came from
another proposal, or that departs from all three, is marked in the Decisions log.

**Design premise (inherited from the base proposal): the database is the program.** Every mutable
entity carries a `state` column drawn from a closed enum with a documented transition table; every
long-running action is a row in `jobs` leased by a worker, so a daemon restart resumes from the row;
handlers never write domain state directly — they call a service method that performs a guarded
transition inside one transaction, emits an `events` row, and publishes an SSE frame. The UI is a
pure projection. The systemd instance template is static and content-free: it runs
`llamaman instance-exec %i`, which reads the row and `execve`s `llama-server`, so configuration
lives in exactly one place and unit files can never go stale.

---

## 0. Decisions log

Numbering is append-only: a gap is a decision this design later **removed**, and the entry that removed it says so. D87 is the largest such removal — it retired nine rows of self-update rollback machinery at once.

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | systemd scope (default) | **System units** in `/etc/systemd/system` + a unit-name-scoped polkit rule | SPEC §3.7 names that path literally; avoids the `enable-linger` dependency and the boot race against `user@%U.service`. |
| D2 | systemd scope (alternate) | `install.sh --user-units`: same units rendered into `/etc/systemd/user`, `loginctl enable-linger`, **no polkit at all**, binary at `~<user>/.local/bin/llamaman`, self-update performed in-process by the daemon | Hosts with no usable polkit authority still work; one `Controller` interface, one variable (`systemd_scope`). A user unit runs *inside* `user@<uid>.service`, so it can neither order itself against it nor drive a root oneshot — §5.2a states the three concrete differences (ordering, self-update actor, how root enables a unit for another user) rather than pretending the topologies are identical. |
| D3 | Transient systemd units | **Never used.** Builds and benches are daemon child processes | polkit can only see a unit *name*, not its properties, so any transient-unit grant lets a compromised daemon start a unit with `User=root`. Removing the capability removes the escalation. |
| D4 | Build durability | Build directory is preserved; a daemon restart marks the job `interrupted`, and **Retry** re-runs `cmake --build` against warm objects | Recovers ~90% of the value of transient units with none of the privilege surface; self-update and UI-initiated restarts return `409 job_in_flight` while a build runs. |
| D5 | systemd control channel | `go-systemd/v22/dbus` with `NewSystemConnectionContext` (polkit-mediated); `systemctl` exec as a degraded fallback behind the same interface | Push-based sub-state signals and typed job results; `NewSystemdConnectionContext` is the private root-only socket and is asserted against in a test. |
| D6 | Journal reading | `journalctl -o json [--follow]` subprocess | `sdjournal` requires cgo, which forfeits the single static binary. |
| D7 | Instance restart policy | Template unit is **`Restart=no`**; the supervisor is the only restarter, driven by `instances.restart_policy` and the `instance_starts` ledger | *Diverges from all three proposals.* Makes `always`/`on-failure`/`never` exact, data-driven and testable without systemd, and removes the base proposal's "exit 0 means I refuse" lie in journald. |
| D8 | Crash-loop cutoff | Supervisor: more than `restart_max` starts in `restart_window_sec` → state `crash-looping`, requires an explicit Start | Follows from D7. `StartLimitIntervalSec`/`StartLimitBurst` are inert without `Restart=`; if ever re-added they belong in `[Unit]`, never `[Service]` (systemd v229+ silently ignores them there). |
| D9 | Daemon unit type | `Type=notify`, `WatchdogSec=30`, `STATUS=` carries the bound URL; sd_notify implemented in ~25 lines over `$NOTIFY_SOCKET` | A wedged daemon is restarted instead of accepting requests it cannot serve; `systemctl status llamaman` shows the port the walk actually landed on (SPEC §3.9). |
| D10 | Instance unit hardening | `PrivateDevices` deliberately **absent**; `LogRateLimitIntervalSec=0`; `ProtectHome=no`; `KillSignal=SIGINT` | `PrivateDevices` hides `/dev/nvidia*` and breaks every CUDA instance opaquely; journald's default rate limit eats exactly the bursty llama.cpp load output needed after a failure. |
| D11 | Instance name grammar | `^[a-z0-9][a-z0-9-]{0,31}$`, enforced in Go, by a SQL `CHECK` and by the polkit regex | The base proposal's `GLOB '[a-z0-9]*'` constrains only the first character, and that string becomes a systemd unit instance id. |
| D12 | Self-update | **Three actors, forward only** (§12): the unprivileged daemon stages and verifies → `llamaman-selfupdate.service` (root oneshot) re-verifies, retains the binary it is replacing and renames the new one into place → `llamaman-update-verify.service` puts the retained binary back if `llamaman.service` ever reaches the `failed` state with the update still unconfirmed. Listener continuity across the restart comes from D58, not from a re-exec | The process that must be judged cannot be the judge. Replaces the base proposal's FD-inheriting re-exec, and restores SPEC §3.8's literal "restarts `llamaman.service`" — with D58 supplying the part the re-exec was there for. |
| D13 | Update-verify executable | `llamaman-update-verify.service` execs **`<prefix>/llamaman.prev`**, the retained previous binary (D89), gated by `ConditionPathExists=` on that file **and** on `update/pending`, and started by `OnFailure=` on `llamaman.service` (D88) | The judge must not be the binary under judgment: if the new binary cannot start, neither can a verifier that is the new binary. Its verdict depends on no field of any file — only on the *existence* of `update/pending` and on the unit state systemd reports — so, unlike its predecessor, it cannot be disarmed by a format or a constant it does not understand. |
| D14 | Pre-update DB snapshot | `VACUUM INTO db-backups/llamaman-<version>-<ts>.db` before applying; the newest **7** kept, oldest deleted first, and **the newest snapshot is never deleted**, whatever the count is tuned to. **Nothing restores it automatically** — `llamaman restore-db <snapshot>` is the third step of the offline, human-confirmed downgrade procedure (D90, D94, §12.4) | Migrations are forward-only, so a downgrade across a schema bump needs the older schema back, and this file is the only thing that can supply it. Restoring it is nevertheless not something an actor may decide: overwriting a live WAL database out from under a running process corrupts it twice over, and discarding every instance, token, benchmark and event created since the snapshot is a judgment only the operator can make. **The protected file is the newest one, and the reason is arithmetic**: a snapshot is taken only immediately *before* an update and is labeled with the version being replaced, so the newest snapshot is always the one the version now at `<prefix>/llamaman.prev` left behind — exactly the schema a depth-one downgrade needs (SPEC §5 assumption 6). An earlier reading protected "the newest snapshot for the version currently installed" and was doubly wrong: in the steady state no such file exists (none is taken while a version is installed), and when one does exist it carries a schema the running binary can already open, which is of no use to a downgrade. |
| D15 | Binary location | `<prefix>/llamaman` (default `/usr/local/bin`), root-owned 0755, replaced only by `install.sh` and the root oneshot; the retained previous binary sits beside it as `<prefix>/llamaman.prev`, same owner and mode (D89); the path is threaded through `install-units --prefix` and re-derived at runtime from `os.Executable()`, never hardcoded | A service-user-writable file on root's `PATH` is a privilege-escalation trap for anyone who types `sudo llamaman` — and the same argument is why the *retained* binary, which a root unit execs and a root unit installs, lives in that root-only directory rather than in the service-identity-owned `update/`. Hardcoding `/usr/local/bin` in three places while advertising `--prefix` produces units that point at nothing. |
| D16 | GPU inventory — **explicit SPEC amendment (§3.2, §3.1)** | `nvidia-smi --query-gpu` CSV behind a `Prober` interface, 2 s cache; failure marks GPUs `unknown`, never zero. SPEC §3.2 names **NVML** as the live-VRAM source and §3.1 lists "driver + NVML" in the toolchain probe; both are amended to read *the NVIDIA driver's own reporting*, delivered by the `nvidia-smi` CLI the driver ships beside NVML. Scope and v2 posture are unchanged: NVIDIA-only VRAM detection in v1, with SPEC §6's ROCm/Vulkan detection still deferred and `hw.Prober` the seam it drops into | Drops the `purego`/NVML `dlopen` binding: hand-bound versioned C structs are untestable ABI risk for one 30 ms fork's worth of saving, and a `CGO_ENABLED=0` binary has no dynamic loader to service `dlopen`. The numbers the spec asks for are identical — `nvidia-smi` is an NVML client — so this is a change of mechanism, not of promise, and it is recorded here rather than left as a silent deviation. |
| D17 | Per-instance VRAM **and GPU identity** | `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_gpu_memory` matched against systemd `MainPID`, recorded with a `gpu_attribution` confidence (§8.6) | The base proposal declared `instance_status.vram_bytes` with no stated source — and a `pid,used_gpu_memory` query alone carries no GPU identity, so it cannot populate `gpu_uuids_json` and the SPEC §3.5 bench exclusivity guard would be unimplementable on a multi-GPU host. |
| D18 | Prebuilt acceptance | A tarball is `ready` only after `bin/llama-server --version` exits 0 **on this host**; `debug/elf` parses `.gnu.version_r` to say "requires GLIBC_2.38, host has 2.36", then falls back to a CPU source build | This is what makes SPEC §3.7's distro-agnostic promise true; the Ubuntu-built tarballs are exactly where it otherwise breaks. |
| D19 | CUDA build acceptance | `bin/llama-server --list-devices` must report ≥ 1 CUDA device, else `failed_verification` | A CUDA build that silently fell back to CPU is worse than no build. |
| D20 | Build parallelism | `N = min(NumCPU, max(2, MemAvailableGiB/2))`, with one automatic retry at `-j1` after an OOM kill, reason shown | `nvcc`/`cc1plus` on ggml-cuda units routinely peak above 2 GiB; an OOM-killed compile is the most common CUDA build failure on a workstation that is also serving models. |
| D21 | CUDA architectures | `CMAKE_CUDA_ARCHITECTURES` from detected compute capabilities (not `all`, not `native`), recorded in `manifest.json`; a later GPU-set change raises "rebuild recommended" | `native` produces a binary that silently will not run if the GPU set changes; `all` multiplies compile time. |
| D22 | Version self-containment | `-DCMAKE_INSTALL_RPATH='$ORIGIN/../lib'` with `-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON`; no `LD_LIBRARY_PATH` in the runtime environment | Each `versions/<id>/` is relocatable, which is what makes symlink activation and rollback safe. |
| D23 | Tools build flag | `-DLLAMA_BUILD_TOOLS=ON` (with `EXAMPLES=OFF`, `TESTS=OFF`, `CURL=OFF`, `BUILD_SHARED_LIBS=ON`) | `llama-bench` lives under `tools/` upstream; without this flag the headline SPEC §3.5 feature is simply absent from the build. |
| D24 | Rolling restart | Canary-gated with an **automatic** revert that is a *row* revert first and a symlink repair second: restart one instance, gate on `/health`, and on failure restore `is_active`/`previous_active`, re-run `RecomputeConfigHash`, repair both symlinks from the restored rows, and restore the canary before touching anything else (§6.6 step 5) | SPEC §3.1 asks for a rolling restart with confirmation; halting the roll and *offering* a rollback leaves the user to notice. Reverting only the symlink would have been worse than not reverting: §6.6's boot reconciliation makes the row win, so the next daemon start would re-point `active` at the failed build and restart every instance onto it — and the fleet would meanwhile carry a permanent false `restart_required` from the recompute (D69) that was never undone. |
| D25 | Version GC and staleness | Never delete a `versions/<id>` that `readlink /proc/<MainPID>/exe` resolves into; derive the `stale_version` **flag** (not a state — §2.8) from the same signal | The only honest way to know which build a live process is executing after a symlink flip; and a still-serving instance must stay `ready` while it wears the badge. |
| D26 | Downloads | One connection per file, up to 3 files in parallel; `Range` + `If-Range: <etag>` resume into hub's own `blobs/<etag>.incomplete` | Single-stream makes resume a one-variable problem *and* keeps the partial file resumable by `huggingface_hub` itself, which a striped `.part` file could never be. |
| D27 | HF cache interop lock | `flock` on `<hub>/.locks/<repo_folder>/<etag>.lock` — the path `huggingface_hub` itself uses — built by one function and asserted in a test | Locking the `.incomplete` file, or a `.lock` inside the repo directory, interlocks with nothing; SPEC §3.2's one-shared-cache promise depends on getting this path exactly right. |
| D28 | Blob deletion | Refcount blobs across every snapshot in the repo directory, then show a "will free N GB, N files" preview before executing | A blob shared by two revisions must never be removed out from under one of them. |
| D29 | Fit: weights | Per-layer bucketing on `^blk\.<i>\.`; `file_size / n_layer` averaging is forbidden | MoE and large output heads make the average wrong exactly where the answer matters. |
| D30 | Fit: KV | Per-layer `n_head_kv` arrays, `kv_ctx` padded to 256, a `-ctk`/`-ctv` bytes-per-element table, **plus** a sliding-window term keyed off `{arch}.attention.sliding_window` | Gemma3-class models are in v1 scope and are mis-sized by an order of magnitude without the SWA term. |
| D31 | Fit: compute | `CB_logits + CB_act + CB_attn + CB_moe`, where `CB_moe = X_used × U × F × 4` | MoE routing/expert scratch is a first-class term, not a fudge factor. |
| D32 | Fit: calibration | `fit_observations` rows parsed from llama.cpp's own logged buffer sizes, per `(arch, backend, runtime_tag)`, median ratio clamped to `[0.5, 2.0]`, ≥ 3 samples; API reports `confidence: calibrated\|modeled` | The compute-buffer term is the only genuinely empirical part; learning it from this host's own loads beats a hard-coded constant. |
| D33 | Fit: ground truth | llama-server's `--fit` projection is captured at load and shown beside the estimate, marked "unavailable" when `-ngl`/`--tensor-split` are pinned | SPEC §3.2 asks for exactly this, and upstream disables `--fit` in that case. |
| D34 | Draft model validation | Three-valued: both sides parsed → strict check, `422 draft_vocab_mismatch`; either side not yet parsed → accept with `draft_validation='deferred'` and a warning, re-checked when the metadata lands and again in the launcher's preflight | Otherwise the failure surfaces as garbage output at runtime instead of a form error — but a hard reject on NULL metadata would break the design's own "queue the download, then configure the instance" flow, since `tokenizer_model`/`n_vocab` only exist after a GGUF parse. |
| D35 | Model cards | Rendered **server-side** with goldmark + bluemonday | Model cards are attacker-controlled markdown containing raw HTML, rendered inside the origin that holds the admin session cookie. |
| D36 | Gateway transport | `DisableCompression=true`, `FlushInterval=-1`, no response timeout | Without it Go's Transport adds `Accept-Encoding: gzip` and transparently decompresses, so the bytes the client receives are not the bytes llama-server sent — breaking SPEC §3.4's pass-through in a way no test notices. |
| D37 | Token hashing | `sha256` for API tokens; argon2id for the admin password only | A 256-bit random secret has no dictionary to attack, and argon2 on every inference request would cost ~100 ms per call. |
| D38 | First-run protection | Loopback needs no token; every other origin needs the one-time setup token, which is readable from the host through the D59 token file (`install.sh`, journald, `llamaman status`) | Closes the LAN claim race without a clock-dependent second code path, and preserves "download → start → open browser → done" for the person at the console. |
| D39 | Job double-submit | Partial unique index on live jobs per subject **and** an `Idempotency-Key` header with a 10-minute replay window, backed by the D65 `idempotency_keys` table | The index makes two live jobs structurally impossible; the header makes a double-clicked Build return the *same* job instead of a 409. |
| D40 | Schema | All tables `STRICT`; enums as `TEXT CHECK`; JSON columns `CHECK(json_valid(x))`; timestamps `INTEGER` Unix **milliseconds** UTC | The database rejects an illegal state even when a code path is wrong. |
| D41 | Flag storage | One `flags_json` column typed in Go as `model.FlagSet`, not ~60 nullable columns | llama.cpp ships ~10 nightlies/day; a new upstream flag must be a struct field and a golden argv test, never a migration. |
| D42 | State machine helper | No generic `Apply(entity, event)` engine — transition tables stay as documentation plus SQL `CHECK`s, and each service method performs its own guarded transition | Eight hand-written guards are less machinery than an engine plus a generated reachability test. |
| D43 | API contract enforcement | `openapi.json` generated from the route registry, drift-checked in CI, **and** a response-conformance middleware enabled in integration tests | The drift check proves the document matches the routes; the middleware proves the handlers match the document. |
| D44 | Charts | uPlot (canvas, ~45 KB); comparison bars are hand-rolled SVG | A 512-point sweep is thousands of points; a React-component chart library stalls. |
| D45 | Frontend stack | React 19 + TypeScript 7 + Vite 8 — veto exercised: the owner's post-spec "build on latest stable" directive supersedes SPEC §5.4 assumption 4's original React 18 + Vite 6 default; majors resolved by live registry query at implementation time | The veto window is the user's, and the latest-stable directive is that veto being exercised. |
| D46 | Secrets at rest | HF token sealed with AES-GCM under `secret.key` (0600) inside the 0600 DB | Defends the artifacts we ourselves create — `db-backups/`, diagnostics bundles, user backups — not host compromise; that limit is stated rather than implied. |
| D47 | CI systemd | `systemd --user` inside `dbus-run-session` on a stock GitHub runner for the D-Bus suite, plus one `ubuntu-24.04` job that installs system-scope units via `install.sh` | Exercises the identical `StartUnit`/`JobRemoved`/`EnableUnitFiles` paths per-PR with no privileged container, keeping SPEC §1's no-Docker posture clean even in CI. |
| D48 | `install.sh` shape | Whole script wrapped in `main "$@"` invoked on the last line; artifacts fetched through `releases/latest/download/`; units and polkit files written by `llamaman install-units` | Defeats the truncated-`curl \| sh` partial-pipe hazard, dodges the 60/hour anonymous API limit, and gives the F16 repair path for free. |
| D49 | Package invariants | Only `internal/store` writes SQL; only `internal/systemd` speaks D-Bus; only `internal/instances` renders llama.cpp command lines — **both** of them (D62) | The third one is why the bench runner, the fit calculator and the "show me the command line" endpoint can never disagree about what a `FlagSet` means. |
| D50 | Support surface | `llamaman diagnostics --out FILE` (redacted bundle), `llamaman doctor`, `flock(llamaman.lock)` single-instance guard naming the holding PID, `PRAGMA integrity_check` at boot with restore from `db-backups/` | Turns "it broke" into one attachable artifact and makes two daemons impossible. |
| D51 | `-ngl auto` rendering | `auto` renders **no `-ngl` flag at all**, leaving llama.cpp's own `--fit` to choose the offload; the calculator's `max_n_gpu_layers` is advisory and one click pins it as `{"mode":"count"}` | The only reading that keeps `internal/instances` pure (no `fit`/`hw` import), keeps `config_hash` independent of live VRAM, and does not disable the `--fit` ground truth D33 is built on — upstream turns `--fit` off precisely when `-ngl` is pinned. |
| D52 | `config_hash` scope | sha256 over the rendered argv **with `--host` and its `--port` value elided**, plus the resolved model paths and the active version id | The internal port is an allocation detail the supervisor may reassign after an exit 78 (F5); including it would make `restart_required` flap for a change no user made, and would break the golden argv test's stability claim. |
| D53 | Autostart is the boot authority | On the first supervisor pass after a **host** boot, `desired_state` is written from `autostart`; within one host boot `desired_state` is the API's alone | Without the coupling the two fields fight: systemd starts an enabled unit at boot and the reconciler immediately stops it, while a disabled unit whose `desired_state` was left `running` is started by the reconciler anyway — autostart broken in both directions. |
| D54 | Start ledger opens before preflight | `instance-exec` inserts the `instance_starts` row immediately after it loads the instance row, and closes it with an outcome on **every** exit path; the supervisor closes the row for a start that reached `execve` | Otherwise a config that fails preflight records nothing, so crash-loop detection never fires and the exit-78 start history F5 promises does not exist. |
| D55 | Jobs ↔ domain rows | Exactly one `jobs` row per domain subject under a fixed `subject_type`/`subject_id` mapping (§2.3a); `jobs` carries scheduling, the domain row carries domain state, and one transaction writes both | Two independent state machines over one activity is the classic drift bug, and `idx_jobs_one_live_per_subject` (D39) means nothing until the subject of every kind is named. |
| D56 | Gateway accounting is instance-first | Every proxied request is counted in `instance_usage_daily`; `token_usage_daily` is the per-token breakdown written additionally when a credential was presented | `auth_mode='none'` is an explicit SPEC §3.4 feature, and a token-keyed primary key silently drops all of its traffic — including bytes and errors, which `/metrics` cannot supply. |
| D57 | Unit-affecting changes from the UI | Cache relocation needs **no** unit change (`ProtectSystem=full` + `ProtectHome=no` already leave every plausible cache root writable, and roots under a protected prefix are rejected at registration); service identity is installer-only, and the UI shows the exact `install-units` command rather than pretending to change it | SPEC §3.9 promises unit changes happen through the UI or installer flags — the honest way to keep that promise for a runtime-unprivileged daemon is to remove the unit's dependence on the mutable value and to name the one operation that stays installer-side. |
| D58 | Listener continuity across a daemon restart | **systemd file-descriptor store**: every public gateway listener and the management listener is handed to systemd with `sd_notify FDSTORE=1` + `FDNAME=` before exit and re-adopted from `LISTEN_FDS`/`LISTEN_FDNAMES` on the next start; `FileDescriptorStoreMax=256` in the unit | SPEC §1 makes Llama Man the owner of the public inference ports, so "self-update does not interrupt serving" (SPEC §3.8) is a claim about *those* ports, not only about `llama-server`. Socket activation cannot be used (a `.socket` unit per instance would need privileged writes at runtime, which D3/D57 forbid); the fd store is the one native mechanism that keeps the listening sockets open across `systemctl restart` with no privileged write and no FD-inheriting re-exec. §9.4 states exactly what is and is not preserved. |
| D59 | Setup-token hand-off | The plaintext token is written **once** to `<state_dir>/setup-token` (0600, service identity — the state directory resolved by D72, never a literal `/var/lib/llamaman`) and unlinked the moment the claim is stamped; the DB keeps only `sha256` | The DB is not a channel back to a human: `llamaman status` and `install.sh` must be able to *print* the token, and `setup_claim` deliberately stores only a hash so backups, `db-backups/` and diagnostics bundles never carry it. A 0600 file in the 0750 state directory has exactly the authorization the token needs — host access — and disappears on first use. |
| D60 | Version identity includes acquisition | `llamacpp_versions.id` and `dir_name` are `<tag>-<backend>-<acq>` with `acq ∈ {bin,src}` (`b10621-cpu-bin`, `b10621-cpu-src`, `b10621-cuda-src`) | D18's fallback enqueues a **source** build of the same tag and backend whose prebuilt just failed verification. With a two-part id that row cannot be inserted: the failed row already holds the primary key. Identity must carry the third axis, and the failed prebuilt row must survive as the record of why the source build exists. |
| D61 | Transient start overrides (safe start) | A nullable `instances.pending_override_json`, consumed and cleared by `instance-exec` in the same transaction as `pending_trigger`; never part of `config_hash`, never bumps `generation` | F3's Safe start must reach a content-free template unit that receives only `%i`. Writing the override into `flags_json` would persist it and move `config_hash`; a `StartTransientUnit` channel is banned by D3. A second hand-off column is the only mechanism consistent with both, and it makes the override visible in the start ledger. |
| D62 | Two renderers, one package | `instances.RenderArgv` (llama-server) and `instances.RenderBenchArgv` (llama-bench) live in `internal/instances` and share one `FlagSet` | `llama-bench` has an incompatible CLI (no `-c`, no `--alias`, no `--host/--port`, `-fa 0\|1` not `on\|off\|auto`, sweep-list syntax). One function cannot emit both. Keeping both in one package preserves the D49 invariant that flags are rendered in exactly one place, and §10.1 pins the `FlagSet` → llama-bench mapping so the two cannot drift. |
| D63 | Start-ledger closure is single-shot | `instance_starts.outcome ∈ {failed,stopped,inhibited}` is written **once**, at the end of the run; reaching `/health` 200 stamps `ready_at`, not `outcome` | Two rules closing one row with no precedence is an implementer's coin flip. With `ready` removed from `outcome`, "the most recent closed row" is unambiguously "the last completed run", `on-failure` keys off `outcome='failed'` rather than a NULL-prone `exit_code`, and an instance that is serving keeps exactly one open row. |
| D64 | Crash-loop counting is failure-only | The cutoff counts rows with `outcome='failed'` and `at > instance_status.restart_window_reset_at`; `stopped`, `inhibited` and rows that served for ≥ 60 s are never counted | Counting *every* start makes a healthy instance crash-loop after six user restarts, makes `inhibited` rows self-reinforcing (declining writes a row that pushes the count further over the line), and lets a rolling restart plus a bench stop/restore trip the cutoff during ordinary maintenance. |
| D65 | Idempotency keys are their own table | `idempotency_keys(key PK, job_id, expires_at)` with a 10-minute TTL, replacing the permanent `idx_jobs_idem` unique index | A global, permanent unique index cannot express a 10-minute replay window: after the window the same key must be able to create a new job, and a client with a fixed key would collide forever. |
| D66 | `--device` is the only device selector | `CUDA_VISIBLE_DEVICES` is **never** set by the launcher | Setting both renumbers the devices llama.cpp sees, so `--device CUDA1`, `--main-gpu` and `--tensor-split` silently address different physical GPUs than the user picked. Leaving the environment untouched is what makes `nvidia-smi` index ≡ `CUDA<n>` ≡ `gpus.gpu_index` a single stable mapping. |
| D67 | Degraded, not dead, without systemd | `systemd_control='unavailable'` is a **supported degraded mode**: the control plane is read-only, everything that is not instance supervision keeps working, and the daemon never spawns `llama-server` itself | The alternative reading ("refuse to serve") makes the whole models/downloads/bench/fit half of the product unusable on a container or a non-systemd host for no security or correctness gain. The clause that actually matters — no silent child-process fallback — is kept verbatim. |
| D68 | Instance deletion is **soft** | `DELETE /instances/{id}` stops the unit, disables it, closes the gateway listener and stamps `deleted_at`; rows and accounting survive. `name`, `public_port` and `internal_port` become **partial** unique indexes `WHERE deleted_at IS NULL`, so all three are immediately reusable. `?purge=true` is the explicit hard delete that cascades the history away | Eight rules elsewhere are already phrased over "non-deleted" instances, and `instance_usage_daily`/`token_usage_daily`/`instance_starts` all cascade — so a hard delete silently discards accounting this document calls a product feature. Soft deletion without partial indexes would instead consume the name and both ports forever. Exactly one reading had to win, and it is the one the rest of the document already assumes. |
| D69 | `config_hash` is **maintained**, not merely written at save | Two additional named writers recompute it through one store method `RecomputeConfigHash`: llama.cpp activation (every non-deleted instance, in the activation transaction) and the models service (when a model's resolved snapshot path changes). Neither bumps `generation` | `config_hash` folds in the resolved model paths and the active version id (D52), and both change without any `PATCH`. Left unmaintained the stored hash goes stale, `restart_required` never fires after a version flip, and `applied_config_hash` comparison stops meaning anything. Computing it on read was the alternative and was rejected: `instance_starts.config_hash` must record the value *at the moment of the attempt*, which only a stored column can supply. |
| D70 | One build at a time is a **lease**, not an index | A `build_lease` singleton row acquired by conditional UPDATE inside the worker's transaction; `idx_jobs_one_live_per_subject` is per subject and cannot express a global limit | Two `llamacpp_install` jobs on two *different* version ids are perfectly legal under a per-subject index, so a restart with two `interrupted` builds and two Retry clicks had no DB-level guard at all — only a process mutex that a second process (or a second boot) does not share. |
| D71 | Re-posting an existing version id | Live → `409 build_in_flight`; `ready` → `200` with `"reused":true` unless `force_rebuild`; any terminal-failure state → **reuse-and-reset** the row to `pending`; `ready` with different build options → `409 version_options_differ` naming them, `force_rebuild=true` overrides. Custom tags carry a `git_url` discriminator | `UNIQUE(tag, backend, acquisition)` (D60) is right, but it makes re-installing a failed tag, rebuilding with different `cmake_extra`, and two forks at the same short SHA all impossible with no documented outcome. Reuse-and-reset keeps identity three-part and still lets every ordinary operation happen. |
| D72 | The state directory is **resolved**, not hardcoded | `$STATE_DIRECTORY` (set by systemd from `StateDirectory=`, exactly like `$NOTIFY_SOCKET`) wins; else `/var/lib/llamaman` in system scope and `$XDG_STATE_HOME/llamaman` in user scope. Units use the `%S/llamaman` specifier so one template is correct in both | `StateDirectory=llamaman` under `systemd --user` resolves to `~/.local/state/llamaman`, which disagreed with a hardcoded `/var/lib/llamaman`, with `WorkingDirectory=` and with `ReadWritePaths=` — the D2 topology could not start. `$STATE_DIRECTORY` is set by the service manager, not by a user, so SPEC §3.9 is intact. |
| D74 | Host-boot instant is read from `/proc/stat` `btime` | `runtime_info.host_boot_at`; the `external → autostart` relabel keys off it, not off `boot_at` | `boot_at` is the *daemon* start time, so every ordinary daemon restart — including the one every self-update performs — made all prior `external` starts older than it and eligible for relabeling, rewriting a deliberate `systemctl start` as `autostart` in the ledger. `host_boot_id` identifies the host boot but timestamps nothing; `btime` is the missing clock. |
| D75 | Bench exclusivity is a **lease**, exactly like builds | A `bench_lease` singleton row (§2.10) acquired by conditional UPDATE inside the bench worker's transaction; held until the stop-and-restore finalizer has set `restore_done=1` | `jobs.subject_id` for a bench is `bench_runs.id`, so `idx_jobs_one_live_per_subject` is per *run* and cannot express "one bench at a time" — the identical hole D70 closed for builds. Two sweeps on one GPU would each stop the other's restored instances and each write `stopped_instances_json`/`restore_done` over an overlapping set. It also gives §6.6 step 1's "refuse while a bench is live" a single row to read instead of a fact the schema never made singular. |
| D77 | Journal read access is **granted at install time and probed at boot** | `install-units` adds the service identity to the `systemd-journal` group; the daemon probes readability once at boot into `runtime_info.journal_read`, exposes it in `GET /system/capabilities`, and raises F23 with the exact `usermod` command when it is denied | Every journal feature — `GET /system/journal`, instance log streaming, the §5.8 fit observation, F19's captured tail, `diagnostics` — runs `journalctl` as an unprivileged identity (D6). On the default topology journald's `SplitMode=uid` happens to make it work; on the `--dedicated-user` topology SPEC §5.1b describes, the account is `useradd --system` (uid < 1000) and its messages stay in the *system* journal, so a required SPEC §3.3 feature would silently return empty on a supported install. |
| D78 | A rebuild always installs into `versions/<id>.staging` and is renamed into place | Source builds adopt the prebuilt path's protocol (§6.4 step 2/4); a forced rebuild of the **active** id swaps directories with two renames at `publish`, and the launcher refuses to start while the `is_active=1` row is not `ready` (exit 69, `runtime_rebuilding`) | D71 lets `force_rebuild` reuse-and-reset a `ready` row, including the active one when no live process executes from it — and `versions/active` still resolves into that very directory, because activation is symlink-only and nothing re-points it. Installing directly into it meant an instance started mid-`cmake --install` would exit 69 or, worse, `execve` a half-written binary. Staging plus a row-state guard removes both without giving up the operation. |
| D79 | On the self-update path the daemon **waits to be restarted**; it does not exit | §9.4 step 7 branches: SIGTERM and `POST /system/restart` exit; §12.1 step 7 stops serving, hands the listeners to the fd store and then blocks on the signal the oneshot sends, with a 120 s failsafe | `llamaman.service` is `Restart=always`/`RestartSec=2`. A daemon that exited voluntarily after `StartNoWait llamaman-selfupdate.service` would be restarted by systemd — as the **old** binary — while `selfupdate-apply` was still verifying, producing a pointless intermediate boot in the middle of a swap. Waiting removes it; and if the 120 s failsafe ever does produce one, §12.3's *an actor is active* branch defers to the oneshot on a fact systemd owns (D91), so that boot is inert rather than destructive. |
| D87 | The self-update **rollback subsystem is deleted**; a forward pipeline plus one revert is the whole protocol | Removed outright: `POST /api/v1/update/rollback`; the markers `update/rollback-requested`, `update/rollback-info.json` and `update/rolled-back.json`; the rollback branch in both privileged actors; the gate's rollback, rolled-back and incoherent-set branches; the marker sweep's liveness constants and its four callers; `llamaman update-clear`; the `rolled_back` and `restarting` `self_updates` states with `rolled_back_at`, `rollback_reason` and `verify_mode`; and the incoherent-marker-set failure row (numbered F25 at the time — **that number has since been reassigned** to §17's digest-mismatch row, "the installed binary is not the one the daemon is running", which §11.3, §16.2 and §19 cite; the D-number convention of §0 does not extend to F-numbers, and this is the one reuse). What remains is §12: stage → verify → snapshot → swap → restart → confirm, with one judge that renames `<prefix>/llamaman.prev` back over `<prefix>/llamaman` when `llamaman.service` reaches `failed` with the update unconfirmed. Installing an older release is the ordinary flow pointed at an older tag (D90) | SPEC §3.8 asks for one-click **forward** self-update and nothing else; the rollback subsystem was design-invented, and it failed adversarial verification three times running. Each pass closed the reported holes and the next found new ones of the same class — a fact on disk outliving the process that gave it meaning, or two actors disagreeing about which state the other was in — because three markers, two actors with two branches each, five gate branches, two frozen deadline constants and a database restore inside the failure path is more protocol than can be shown correct, and every one of those parts had to work on the one path that matters most: the one that runs when the host will not boot. The replacement has one marker, one actor branch, one judge action and no clock of its own. What it gives up is reachable another way (D90) or was never asked for. |
| D88 | The revert's trigger **and** its deadline are systemd's, not ours | `llamaman.service` carries `OnFailure=llamaman-update-verify.service`, `TimeoutStartSec=45`, `RestartSec=2`, `StartLimitIntervalSec=600` and `StartLimitBurst=5`, so the judge is started exactly when the unit enters the `failed` state — at most `5 × (45 + 2) = 235 s` after the swap — and never on a healthy host. D93 is what keeps that limit counting only the starts that never became healthy, and D92 is what keeps the judge from restoring a binary into a database the new one has already migrated. The daemon sends `EXTEND_TIMEOUT_USEC=` every 10 s while `PRAGMA integrity_check` or a migration is running, so a genuinely slow start extends the deadline instead of being judged. There is no timer unit, no `OnActiveSec=`, and no compiled-in deadline constant anywhere in the protocol | The deleted design measured "was I summoned late" and "may I still defer" with two constants (300 s and 600 s) frozen across binaries and clocked from a timestamp the daemon wrote *before* a drain governed by a user-editable setting — so raising `gateway.drain_sec` past ~100 s disarmed the automatic revert silently and permanently, while the capability endpoint went on advertising it. `OnFailure=` states the trigger in the one vocabulary both halves of the protocol already share, and the deadline becomes an arithmetic property of the rendered unit that CI asserts rather than a number two releases must agree about. It also covers what a `+180 s` timer could not: a power loss in the middle of a revert leaves a host whose next boot fails again, and `OnFailure=` simply fires again. |
| D89 | The retained previous binary lives at `<prefix>/llamaman.prev`, and the privileged actor installs only what it extracted itself | Root copies `<prefix>/llamaman` to `<prefix>/llamaman.prev.tmp` and renames it into place, then extracts the new binary from the tarball **it re-verified** into `<prefix>/llamaman.new.tmp` and renames that over `<prefix>/llamaman`. Install and revert alike are one `rename()` between two names in one root-owned directory | Three problems at once. (a) `update/` is owned by the service identity, so a root unit whose `ExecStart` pointed there could be handed a binary of the daemon's choosing; the old design paid for that with an owner/mode/sha256 check and an entire second contract file (`rollback-info.json`) to carry the digest, where a root-only directory removes the attack and the file together. (b) `rename()` from `update/` (under `/var/lib`) to `<prefix>` (under `/usr/local`) is `EXDEV` wherever those are separate mounts — a very common layout on which the old swap was simply not atomic. (c) Renaming `update/llamaman.new` installed a file the unprivileged stager could have replaced *after* the signature check; extracting from the re-verified tarball closes that window instead of documenting it. |
| D90 | Installing an older release is an ordinary update; restoring a database is an explicit human act | `POST /update/apply {"tag": <older>}` runs the identical pipeline, and the stage-time probe requires the extracted binary to print **exactly** the requested tag, which is what makes a downgrade expressible. **Below a schema bump that is the whole story.** Across one, the older binary refuses to open the database (§11.1 step 4, §5.6a), D88's revert puts the newer binary back on its own, and completing the downgrade is then D94's five-command offline procedure — of which `llamaman restore-db` (§12.4) is one step, never the whole thing | A second mechanism for "install a different version" bought nothing the first one did not already do, and cost a marker, an actor branch, a fresh `self_updates` subject id and the closing pass that existed to clean up after that id. The honest consequence — that a downgrade across a schema bump needs the old database back, and that nothing automatic can decide whether discarding the intervening instances, tokens, benchmarks and events is acceptable — is now stated and given a procedure, rather than performed silently inside a rollback the user thought was a binary swap. |
| D92 | The revert is disarmed **before** the migration, not after the boot | §11.1 step 4: if `update/pending` exists when the daemon is about to apply its first migration, the marker is read into memory and **unlinked before that migration runs**; step 11's gate then resolves the in-memory copy exactly as if it had read the file. The gate itself also moves ahead of `READY=1` (§11.1 steps 11–12), so a daemon that ever signals readiness has already resolved the marker | `OnFailure=` fires whenever the unit reaches `failed` — including for a daemon that started, applied migrations, sent `READY=1` and only then began crashing. The judge's two conditions would both still hold, so it would rename `<prefix>/llamaman.prev` over a binary that can no longer open the migrated database; that binary refuses, the unit fails again, and this time the judge's own `ConditionPathExists=@PREFIX@/llamaman.prev` is false — so it is skipped, its `ExecStopPost=` does not run either, and the host is left with no daemon and (once the unit fully fails, `FileDescriptorStorePreserve=restart`) no public gateway ports. Applying a migration is the exact instant the retained binary stops being a valid thing to restore, so it is the exact instant the marker must go. **The disarm is deliberately conservative, and its price is two things, both stated.** (a) One mislabeled history row in a narrow crash window — see §12.3's stop-point table rows 11a and 11b. (b) The unlink is keyed to *about to apply* a migration, not to the first migration that *commits*, so a release whose first migration **fails** — a `UNIQUE` violation or a `NOT NULL` backfill this host's real data refuses, which is arguably the single most likely reason a newly installed binary will not boot — leaves the schema exactly where `<prefix>/llamaman.prev` left it and still loses the automatic revert, which there would have been correct. Keying the unlink to the first commit was the alternative and is rejected: it reopens a crash window between that commit and the unlink, and a crash *inside* that window is precisely the dark host this decision exists to prevent — a marker still arming the judge over a database that has already moved. Paying a lost revert in a case the operator can exit with one re-install (§12.3 row 11b: no database restore, no data discard) to close a window that ends with no daemon and no gateway ports is the right direction of trade, and §12's opening paragraphs say so rather than promising an automatic revert this design does not perform. A schema-*ahead* check inside the judge was the alternative and is forbidden: it would require a root process to open `llamaman.db`, whose root-created `-wal`/`-shm` is a database the service identity can never write again (§11.3). |
| D93 | A boot that stayed healthy clears the unit's start-limit counter | The daemon calls `ResetFailedUnit("llamaman.service")` once it has been continuously ready for **60 s** — the same threshold D64 already uses for "this start served" — and §12.1 step 7 issues the same call immediately before summoning the swap actor. `StartLimitBurst=5` / `StartLimitIntervalSec=600` (D88). `POST /system/restart` answers `429 restart_rate_limited` while this boot's counter has not been cleared, and `409 systemd_denied` on a host where the grant that call needs was withheld — the same grant `RestartUnit` needs, which is why the 429 has only the one reason (§3.3) | systemd's start rate limit counts **every** start attempt, not only failed ones, so `StartLimitBurst=3` made three ordinary restarts fatal: §3.4 wires four settings to a restart button, §13 step 11 restarts on every installer re-run, and a self-update contributes one more. The fourth would put the unit in `failed` with "start request repeated too quickly", where the judge is inert (no `update/pending`), nothing runs its `ExecStopPost=`, and the SPEC §3.8/§9.4 port-continuity promise dies with the unit. Clearing the counter after a demonstrably healthy boot makes the limit mean what §5.4 always claimed it meant — *consecutive starts that never became healthy* — so the revert deadline stays a property of the rendered unit while ordinary operation stops consuming it. The larger burst is the belt to that suspenders: on a host where `manage-units` is denied the reset cannot happen, and 5 in 600 s is the whole budget — a budget **no in-product guard can protect**, since the same action authorizes `RestartUnit` and the endpoint therefore refuses rather than rate-limits. That residual is stated in §11.1a and given failure row F26 rather than left implied. |
| D94 | Completing a schema-crossing downgrade is a **five-command procedure**, and `restore-db` alone is a no-op | `sudo systemctl stop llamaman.service` → re-install the older release with the service down (`install.sh --version <older> --no-start`) → `sudo llamaman restore-db <snapshot>` → `sudo systemctl reset-failed llamaman.service` → `sudo systemctl start llamaman.service`. §12.4 is the normative statement; the F24 card, the update dialog and `llamaman doctor` print those five lines rather than the `restore-db` line alone | The one-click downgrade self-corrects: the older binary refuses, the unit fails, and the judge renames `<prefix>/llamaman.prev` back — **consuming it**. At that instant the older binary's bytes exist nowhere on disk and the host is running the newer one again. Running `restore-db` *then* passes its own precondition trivially (the snapshot's schema is not newer than the running binary), succeeds, destroys every instance, token, benchmark run, download, event and notification created since the snapshot — and the newer binary migrates the restored database forward on its next start, so the downgrade still has not happened. The stop is first because a deliberate stop is not a failure and therefore leaves the judge unarmed; the re-install is second because the binary has to exist again before there is anything for the restore to serve; and `restore-db` prints a warning whenever the installed binary's schema is newer than the snapshot's, which is precisely the shape of the destructive no-op. **The fourth command is `reset-failed`, and it is what an earlier four-command reading omitted**: every state this procedure is printed for was reached by a unit that entered `failed` by exhausting `StartLimitBurst=5` in `StartLimitIntervalSec=600`, and `systemctl stop` clears the failed state but not the rate limit — so the final `start` was refused with "start request repeated too quickly" for up to the remainder of a 600 s window a fast-panicking binary leaves nearly whole, which is longer than the first three commands take. The design already pairs `reset-failed` before `start` in four other places (both actors' `ExecStopPost=`, §12.3 rows 12-14's manual line and F20's); this was the one that did not, and the claim "the commands work from any state" was false exactly where it mattered. |
| D95 | Unit templates carry a version stamp, and an older stamp is `stale`, not drift | `install-units` writes `# llamaman-units: <N>` into every unit it renders. §5.4a's check compares the stamp first: same `N` and a different hash is a hand-edit → **F16**; an older or absent stamp is `drift: stale`, reported at `info` with the `sudo llamaman install-units --identity <user>` line and **blocking nothing**. `self_update_revert` and the swap-unit clause are evaluated by reading the installed unit's own directives, never by "the drift check reports no drift" | Units are written once at install time and are not rewritten by a self-update (§12.2), while drift is measured against the *running* binary's templates. Without the stamp, any release that touches any of the five templates makes every self-updated host raise a permanent F16 and demand a manual repair for a change the user did not make — against SPEC §3.9's posture — and, if `self_update_revert` were implemented as "no drift", would refuse every subsequent update with `409 revert_unavailable`. Having the swap actor re-render `/etc` from the new binary was the alternative and was rejected: it breaks D89's "one rename, no other side effect" property on the one path that runs when the host may not come back. |
| D96 | A `self_update` may be canceled only **before** `staged` | `POST /jobs/{id}/cancel` on a live `self_update` job is honored while the row is `planned`/`downloading`/`verifying`: row and job move to `canceled` in one transaction and `update/` scratch is cleared. At or after the `staged` commit it answers `409 selfupdate_not_cancelable` | `self_updates` had no `canceled` state and §2.3a's cell was empty, so a Cancel click on an in-flight update either violated the domain `CHECK` or marked the job `canceled` while the swap proceeded anyway — the marker is already on disk and the oneshot is already systemd's. Naming the cut at the `staged` commit mirrors the `llamacpp_activate` rule that already lives in the same table, keeps the long, genuinely cancelable part (the download) cancelable, and refuses honestly for the part no endpoint owns. |
| D97 | Self-update exclusivity is **transactional**, not an index and not a lease | `POST /update/apply` evaluates its `409 job_in_flight` clause and inserts the `self_updates` row and its `self_update` job inside **one `BEGIN IMMEDIATE` transaction**, and `update/` is emptied only after that transaction commits | `jobs.subject_id` for a self-update is a fresh `self_updates.id`, so `idx_jobs_one_live_per_subject` cannot express "one at a time" — the same hole D70 closed for builds and D75 for benches. Two concurrent applies would otherwise both pass a read-then-write guard, and step 1 of the second would empty `update/` while the first was still downloading into it, leaving a surviving marker naming a tarball that no longer exists. A singleton lease row was the alternative and is unnecessary here: unlike builds and benches there is exactly one producer (this endpoint) inside exactly one process (`flock`, F11), so SQLite's own writer serialization under `BEGIN IMMEDIATE` is the whole mechanism. |
| D91 | "Is an actor still working?" is a question for **systemd**, not for a clock | The confirmation gate defers to an in-flight swap exactly while `llamaman-selfupdate.service` is active, and that unit carries `TimeoutStartSec=120`; the judge unit carries `TimeoutStartSec=60` and its own start limit. Every deferral in the protocol is therefore bounded by a property systemd enforces, and an `update/pending` that is unreadable or at an unknown `format` is swept like any other once no actor is active, rather than deferred to forever | A liveness predicate built from a marker timestamp and a compiled-in bound is two processes agreeing about a number neither can watch the other apply; its failure modes were a marker no mechanism could remove and a reader that "does nothing" about a file no other reader would remove either. The service manager already knows whether the actor process exists, already bounds it, and is already the only thing both halves of this protocol talk to. Asking it deletes the two constants, the `staged_at` clock, the four-caller sweep, `llamaman update-clear` and the incoherent-marker-set failure row along with them. |
| D98 | A host that may not clear its own start-limit counter gets a **defined refusal and a stated residual**, not a guard it cannot reach | `POST /api/v1/system/restart` answers **`409 systemd_denied`** when the name-scoped `manage-units` grant was refused, carrying `sudo systemctl restart llamaman.service` and `sudo llamaman install-units --repair-polkit`; that 409 **wins over** the D93 `429 restart_rate_limited`, which keeps exactly one reason (this boot has been ready for less than 60 s). `doctor` raises the start-limit check as a **warning** carrying `sudo systemctl reset-failed llamaman.service` on such a host, and the outage that remains is **F26** and §11.1a rather than an open question | `RestartUnit` and `ResetFailedUnit` on `llamaman.service` are authorized by the same polkit action on the same unit name (§5.2 branch (b) lists Start/Stop/Restart/ResetFailed together), so a host that refuses the reset refuses the restart: §3.3's 429 sub-reason "because `ResetFailedUnit` was refused" named a state the endpoint can never be in, while the endpoint had no documented polkit-denied response at all — the one shape it will actually return was the one shape undefined. Defining it also makes the honest accounting unavoidable: on that host nothing in the product spends the budget and nothing in the product can protect it either, so the `StartLimitBurst=5` exhaustion — unit `failed`, judge inert, `ExecStopPost=` unrun, no daemon, gateway ports released — is reachable only through a human's `systemctl restart` or an `install.sh` re-run, and belongs in the failure matrix with a remediation card rather than in a rationale as a reassurance. |

---

## 1. Go package layout

Rule: `internal/model` and the calculators are pure — no I/O, no clock, no DB — so they are trivially
testable; everything with side effects sits behind an interface owned by its consumer.

```
github.com/jlbyh2o/llamaman
├── cmd/llamaman/                 main(): subcommand dispatch only
├── internal/
│   ├── app/                      composition root: opens DB, migrates, constructs services,
│   │                             starts workers, wires HTTP, graceful shutdown
│   ├── buildinfo/                Version, Commit, Date, Channel — set via -ldflags
│   ├── model/                    PURE domain: entity structs, state enums, transition tables,
│   │                             validation, FlagSet, error codes. Imports only stdlib
│   ├── store/                    SQLite: pools, pragmas, migration runner, integrity check
│   │   ├── migrations/           embedded numbered .sql files (0001_init.sql …)
│   │   └── (queries)             one file per aggregate; methods take ctx + *Tx; no business logic
│   ├── jobs/                     job queue: enqueue, lease, heartbeat, progress, cancel, retry,
│   │                             orphan recovery on boot; Worker registry by kind
│   ├── events/                   append to `events`, in-process fan-out hub, SSE topic routing
│   ├── sse/                      SSE transport: subscription, heartbeat, backpressure drop
│   ├── api/                      stdlib http.ServeMux routes, handlers, DTOs, error envelope
│   │   ├── middleware/           session, CSRF, rate limit, idempotency, request log, recover
│   │   └── openapi/              spec generation from the route registry + conformance checker
│   ├── auth/                     argon2id, session mint/verify, lockout, CSRF, setup token
│   ├── secrets/                  AES-GCM box for the HF token; key file 0600
│   ├── mdrender/                 goldmark + bluemonday: model cards and changelogs → safe HTML
│   ├── web/                      go:embed of the built UI + SPA fallback + immutable caching
│   ├── settings/                 typed registry (key, type, default, validator), read-through cache
│   ├── llamacpp/                 llama.cpp lifecycle: version rows, activate, rollback, GC
│   │   ├── github/               Releases client: channel resolution, asset pick, ETag cache
│   │   ├── prebuilt/             tarball fetch, hardened extract, ELF/glibc check, verify
│   │   └── source/               git clone/worktree, cmake configure/build/install, log streaming
│   ├── toolchain/                probes: gcc/g++, cmake, ninja, git, make, nvcc, driver, glibc
│   ├── hw/                       Prober interface; nvidia-smi provider; /proc/meminfo; Statfs
│   ├── gguf/                     pure-Go GGUF reader over io.ReaderAt (file or HTTP Range)
│   ├── fit/                      fit calculator: pure formulas + calibration lookup
│   ├── hf/                       Hugging Face HTTP client: search, model, tree, readme, resolve
│   │   ├── cache/               hub cache layout: paths, locks, blobs, snapshots, scan, delete
│   │   └── download/            resumable downloader, verifier, queue worker
│   ├── models/                   local model service: shard grouping, mmproj pairing, scan
│   │                             reconciliation, disk accounting, delete guards
│   ├── instances/                instance service: validation, port allocation, **argv rendering**,
│   │                             config hashing, desired-state API
│   ├── supervisor/               reconcile loop, health probe, restart policy, fit observation
│   ├── systemd/                  Controller interface + dbusController + execController +
│   │                             unit/polkit rendering + journal reader + sdnotify
│   ├── gateway/                  per-instance public listeners, token auth, proxy, accounting
│   ├── tokens/                   mint/hash/verify, verified cache with an epoch counter
│   ├── bench/                    sweep expansion, llama-bench runner, JSON parse, compare, export
│   ├── selfupdate/              release check, download, ed25519+sha256 verify, staged swap
│   ├── setup/                    wizard step state machine and its service methods
│   ├── netutil/                  port walk, free-port probe, reserved-range allocator
│   ├── procx/                    exec helpers: ctx cancel → SIGTERM → SIGKILL, line streaming
│   ├── logx/                     slog handler tuned for journald
│   └── cli/                      status / reset-password / doctor / diagnostics / install-units /
│                                 instance-exec / selfupdate-apply / update-verify implementations
├── ui/                           Vite + React + TS source; ui/dist is the embed target
├── installer/install.sh
├── packaging/                    unit + polkit templates (embedded; §5)
├── testdata/                     GGUF fixtures, HF API fixtures, llama-bench JSON fixtures
├── api/openapi.json              generated, committed, drift-checked
├── Makefile
└── .github/workflows/            ci.yml, release.yml, nightly.yml
```

**Cross-cutting invariants (D49), enforced by an import-graph test in CI:**

1. Only `internal/store` contains SQL. Every other package takes a repository interface.
2. Only `internal/systemd` imports `go-systemd` or execs `systemctl`/`journalctl`.
3. Only `internal/instances` turns a `FlagSet` into a llama.cpp command line. It exports exactly two
   renderers (D62) — `RenderArgv` for `llama-server` and `RenderBenchArgv` for `llama-bench`, whose
   incompatible CLI is mapped field-by-field in §10.1 — and every consumer calls one of them: the
   launcher and `GET /instances/{id}/command` call `RenderArgv`, the bench runner calls
   `RenderBenchArgv`, and `internal/fit` reads the same `FlagSet` struct. No other package may
   construct a llama.cpp argument. The import-graph test asserts that no package outside
   `internal/instances` contains a string literal matching `^-{1,2}[a-z]` in an `exec` argument
   slice destined for a `versions/*/bin/*` binary.
4. Dependencies point inward: no domain package imports `internal/api`.
5. `internal/{model,fit,gguf}` import nothing outside stdlib.

**Subcommands** (`cmd/llamaman`) — this list is authoritative; every unit file's `ExecStart` names
one of them and a CI test asserts that correspondence: `serve`, `status`, `doctor`, `diagnostics`,
`reset-password`, `restore-db`, `install-units`, `instance-exec`, `selfupdate-apply`,
`update-verify`, `verify-release`, `version` — **twelve**, and `restore-db` is one of them because
D14, D90, D94 and §12.4 all name `llamaman restore-db <snapshot>` as a real subcommand a human runs.
`instance-exec`, `selfupdate-apply` and `update-verify` are unit-only entry points: they refuse to
run from an interactive TTY without `--force`, and say so in their help text. (`selfupdate-apply` is
the root oneshot of §12; `update-verify` is the revert of §12.2. `restore-db` is §12.4's, and is the
opposite kind: never started by a unit, only ever typed by an operator with the daemon stopped.)

Each command's `run` func returns an error, and `main` maps it to a process exit status. That
mapping is not "non-nil → 1": §5.6's launcher exit codes (64, 65, 69, 70, 72, 75, 78) are read back
into `instance_starts.exit_code` and drive the supervisor's restart policy, so a command that must
exit with a specific status returns a `cli.ExitError` carrying it and `main` unwraps that code.

---

## 2. SQLite schema

**Decisions.** Driver `modernc.org/sqlite` (pure Go, preserves `CGO_ENABLED=0`). One file,
`<state_dir>/llamaman.db` — the directory resolved by D72, `/var/lib/llamaman` by default and
`$XDG_STATE_HOME/llamaman` under `--user-units`, never a literal — mode 0600 inside a 0750 directory,
owned by the service identity.
Pragmas on every connection: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`,
`synchronous=NORMAL`, `temp_store=MEMORY`, `auto_vacuum=INCREMENTAL`. Two pools on the one file: a
read-write pool with `MaxOpenConns(1)` (WAL permits one writer; serializing in Go turns
`SQLITE_BUSY` into a queue) and a read-only pool sized to `GOMAXPROCS`. `busy_timeout` still matters
because `instance-exec` opens its own short-lived read-write connection from a *separate process* —
the Go-side serialization is per-process and the design does not pretend otherwise.

IDs are `TEXT` ULIDs (sortable by creation, readable in URLs, and they double as SSE cursors).
Singletons use `id INTEGER PRIMARY KEY CHECK (id = 1)`. Timestamps are `INTEGER` Unix milliseconds,
UTC. All tables are `STRICT`. Migrations are embedded numbered SQL files applied one per transaction,
forward-only; `schema_migrations` records version + sha256 checksum and a mismatch is a fatal boot
error.

### 2.1 Meta, settings, runtime facts

```sql
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
```

`runtime_info.hf_hub_dir` / `hf_home` are a **derived cache, never an input**: they are rewritten
from the primary `hf_cache_roots` row on every boot and on every change to that row, and exist only
so `llamaman status` and `doctor` can print the cache path without an HTTP call. The authority chain
is `hf_cache_roots` (locations) ← `settings['hf.hub_dir']` (the primary hub directory) ←
`runtime_info.hf_hub_dir` (display copy). `hf_home` is a courtesy projection that is `NULL` whenever
the hub directory is not literally `<something>/hub` — which is exactly the `HF_HUB_CACHE` case
(§7.2). §7.2a states the one service method that keeps all of them consistent.

Settings registry (key → type → default). Every one is editable in the UI; none is required.

| key | type | default |
|---|---|---|
| `ui.port_desired` | int | 5526 (seeded once from `serve --port N` when the row is absent — §11.1) |
| `ui.bind` | string | `0.0.0.0` (management UI only) |
| `ui.theme` | enum dark/light/system | `dark` |
| `security.session_ttl_hours` | int | 720 |
| `security.idle_timeout_hours` | int | 168 |
| `security.login_max_attempts` | int | 8 |
| `security.login_window_sec` | int | 300 |
| `security.lockout_sec` | int | 900 |
| `hf.endpoint` | string | `https://huggingface.co` |
| `hf.hub_dir` | string | detected by the §7.2 chain (`$HF_HUB_CACHE` → `$HF_HOME/hub` → …); the **authoritative** primary hub directory. Writing it re-points the primary cache root (§7.2a) |
| `hf.home` | string | derived: `hf.hub_dir` minus a trailing `/hub`, else `""`. Writing it is accepted as a convenience and stored as `hf.hub_dir = <value>/hub` (§7.2a) |
| `hf.download_concurrency` | int | 3 (files in parallel; one connection each — D26) |
| `hf.rate_limit_bytes_sec` | int | 0 (unlimited) |
| `hf.verify_checksums` | bool | true |
| `llamacpp.channel` | enum stable/nightly/custom | `stable` |
| `llamacpp.build_jobs` | int | 0 = auto, meaning `min(NumCPU, max(2, MemAvailableGiB/2))` |
| `llamacpp.cuda_arch_list` | string | `""` = auto-detect (D21) |
| `llamacpp.prefer_prebuilt_cpu` | bool | true |
| `llamacpp.extra_cmake_flags` | string | `""` |
| `llamacpp.keep_previous` | **bool** | true (SPEC §5.6 is "exactly the previous build" — depth is 0 or 1, never N; see §6.6) |
| `instances.internal_port_min` / `_max` | int | 21000 / 21999 (reserved: no `public_port` may fall inside — §2.8) |
| `instances.health_poll_sec` | int | 5 (1 s while `starting`/`loading`) |
| `instances.start_timeout_sec` | int | 900 |
| `gateway.bind` | string | `0.0.0.0` — the address every per-instance public listener binds (§9.1) |
| `gateway.request_timeout_sec` | int | 0 (never cap a generation) |
| `gateway.idle_timeout_sec` | int | 300 |
| `gateway.max_body_mb` | int | 256 (**request** bodies only — responses are never buffered; see `gateway.usage_parse_kb`) |
| `gateway.usage_parse_kb` | int | 64 — size of the tail ring buffer used to recover `usage` from a non-streamed response (§9.3); 0 disables usage parsing entirely |
| `gateway.drain_sec` | int | 20 — how long a restart drains in-flight proxied requests before closing (§9.4) |
| `gpu.poll_active_sec` / `_idle_sec` | int | 2 / 30 |
| `fit.margin_mib` | int | 1024 **per participating GPU** (matches llama.cpp `--fit-target`, which is a per-device margin) |
| `fit.use_calibration` | bool | true |
| `bench.exclusive_gpu` | bool | true |
| `bench.default_repetitions` | int | 3 |
| `update.channel` | enum stable/prerelease | `stable` |
| `update.auto_check` | bool | true |
| `update.check_interval_hours` | int | 24 |
| `retention.events_days` / `retention.events_rows` | int | 90 / 200000 |
| `log.level` | enum | `info` |

### 2.2 Identity, sessions, secrets

```sql
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
```

#### 2.2a The setup token, end to end (D38 / D59)

The token has to travel from the daemon that mints it to a human at a shell, and the database is the
one place it must *not* travel through: `setup_claim` stores only `sha256`, precisely so `db-backups/`,
`VACUUM INTO` snapshots and `llamaman diagnostics` bundles can never leak a live claim credential.
The hand-off is therefore a file, and the file is the single source every printer reads:

1. **Mint** (§11.1 step 8, first boot with an empty `admin_account`): generate 32 `crypto/rand`
   bytes, base58-encode them, insert `setup_claim` with `token_hash` and
   `token_path = <state_dir>/setup-token`, and write the plaintext to that path with
   `O_CREAT|O_EXCL|O_WRONLY`, mode **0600**, owned by the service identity, inside the 0750 state
   directory. The authorization to read it is therefore *host access as root or the service
   identity* — exactly the authorization claiming a host service should require, and exactly what
   §11.3 already demands of `reset-password`.
2. **Announce**: the same string is logged once to journald at `info`
   (`SETUP: open http://<ip>:<port> — setup token <token> (not needed from this machine)`). journald
   is a convenience, never the recovery path.
3. **Print**: `llamaman status` and `llamaman status --json` read `setup-token` **from disk** — not
   from the DB — whenever `setup_claim.claimed_at IS NULL` and the file is readable, and emit it as
   `setup.token`. If the file is unreadable (wrong uid) the field is `null` with
   `setup.token_hint: "run as root or <identity>"`. `install.sh` step 10 gets the token this way, and
   so does a user who lost the terminal scrollback.
4. **Verify**: `POST /api/v1/setup/password` with `X-Setup-Token` compares `sha256(presented)` against
   `token_hash` with `crypto/subtle.ConstantTimeCompare`. Loopback callers skip the header entirely.
5. **Burn**: in the same transaction that creates `admin_account`, `claimed_at`/`claimed_from_ip` are
   stamped and `token_path` is set to `NULL`; immediately after the commit the file is `unlink`ed and
   the state is idempotent (a missing file with a non-NULL `claimed_at` is normal). A crash between
   commit and unlink is repaired on the next boot, which removes any `setup-token` file whose row is
   already claimed.
6. **Rotate**: if the file is missing while `claimed_at IS NULL` (someone deleted it, or an
   `install.sh --purge` half-ran), the next boot mints a **new** token and replaces the row. A
   one-time credential nobody can read is worse than a fresh one.

`llamaman status` never *recovers* a token — recovery is re-minting, and the only way to trigger it
is to remove the file as root, which is the same privilege that could read it.

### 2.3 Jobs — the scheduling spine

```sql
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
```

**Acquiring the build lease.** The install worker, inside the same transaction that moves its job to
`running`, runs `UPDATE build_lease SET job_id=?, version_id=?, owner=?, acquired_at=?, expires_at=?
WHERE id=1 AND (job_id IS NULL OR owner=? OR expires_at < ?)`. Zero rows changed means another build
holds it: the job stays `queued` with `run_after = now + 15 s` and the UI says "waiting for the
running build", which is a queue, not an error. The lease is released when the job reaches a terminal
state, when a build is canceled, and at boot for any row whose `owner` is not the current `boot_id`
(the holding daemon is provably gone). The in-process mutex remains as a cheap first gate; the row is
what makes the guarantee survive a restart, a second boot and two simultaneous Retry clicks.

**Idempotency semantics.** The middleware runs inside the handler's transaction:
`SELECT … WHERE key = ? AND expires_at > now`. A hit whose `route` and `request_fingerprint` match
returns the recorded job with `200`; a hit whose fingerprint differs is `422 idempotency_key_reused`
(the client sent two different requests under one key, which is a bug, not a replay). A **miss**
first `DELETE`s any expired row with that key, then inserts the new one alongside the job — so the
same key is reusable the moment its window closes, and the primary key still makes a concurrent
double-submit impossible inside the window. The nightly `maintenance` job sweeps
`expires_at < now - 24h`. Behind all of it, `idx_jobs_one_live_per_subject` makes two live jobs on
one subject impossible regardless of what the client sends.

Lifecycle: `queued → leased → running → succeeded|failed|canceled`; `running ⇄ paused` (downloads
only); `failed → queued` while `attempts < max_attempts` with backoff
`run_after = now + min(60s, 2^attempts × 2s)`.

On boot, every row with `state IN ('leased','running')` and `lease_owner != boot_id` is triaged into
one of **three** outcomes, and the third is the one that keeps §2.3a's one-state-per-activity
invariant true:

| outcome | kinds | why |
|---|---|---|
| `queued` — re-run from the top | `model_download`, `cache_scan`, `toolchain_probe` | idempotent and resumable; the domain row returns to `queued` with it |
| `interrupted` — **a domain finalizer must resolve it, not the queue** | `llamacpp_install` (D4: the build directory is warm, Retry reuses it), `llamacpp_activate` (§6.6's boot reconciliation is the finalizer), `bench_run` (the stop-and-restore finalizer, §10), `self_update` (the §12.3 confirmation gate, called from §11.1 step 11 — including the branch that closes a row whose swap provably never took, and the closing pass that does the same for a row no marker names at all) | the activity left durable state outside the job row — object files, a symlink that may or may not have been flipped, a half-rolled instance fleet, stopped production instances, a swapped binary — that only its own subsystem knows how to settle. The **domain row keeps its state** (`llamacpp_versions.state='ready'` with whatever `is_active` the activation transaction committed, `bench_runs.state='running'` with `restore_done=0`, `self_updates.state` at whichever non-terminal value it held — `downloading` and `verifying` included, which is both an ordinary restart mid-stage and what a database restored from `db-backups/` looks like), because that state is precisely the finalizer's input |
| `failed` with `error_code='daemon_restarted'` | `llamacpp_delete`, `model_verify`, `model_delete`, `maintenance` | nothing durable is owed that the row does not already describe; the domain row is resolved in the same transaction (§2.3a). For `llamacpp_delete` that resolution is the §2.5 edge out of `deleting`: `ready` when the directory is still complete and `bin/llama-server` executes, `failed` when it is not — a half-removed tree is the one thing a delete can leave behind, and re-checking it is cheaper than guessing |

`paused` rows are left alone — a pause is a user decision that must survive a restart. `interrupted`
and `paused` both count as live for the unique index, so nothing else can start on the same subject
until the finalizer, the user or a retry resolves it.

**Why `bench_run`, `self_update` and `llamacpp_activate` are not in the third bucket**, which an
earlier reading put them in: marking the job `failed` also has to mark the domain row `failed`
(§2.3a), and that destroys the input of the recovery that follows. §10 restores bench-stopped
production instances from a run row left `running` with `restore_done=0` — a condition a `failed` row
can never satisfy, so a bench that stopped two serving instances would leave them down, which §10
names the worst possible outcome. §11.1 step 11 marks the `self_updates` row `succeeded` on a
confirmed update, which would contradict a job the same boot had just marked `failed`. And an
activation commits its `is_active`/`previous_active`/`config_hash` transaction *before* the symlink
flip and the canary roll (§6.6 steps 3–5), so a daemon restart can leave the row saying the new
version is active while the filesystem and the fleet have not caught up — the exact state §6.6's boot
reconciliation exists to repair, and the exact state a blanket `failed` would have overwritten with a
lie. `interrupted` is the state that already means "live, reusable work exists, nothing else may claim
this subject" — exactly what all three need.

**The `llamacpp_activate` finalizer** is §6.6's boot reconciliation, and it is two lines: repair
`versions/active` (and `versions/previous`) from the rows, then close the `interrupted` job —
`succeeded` when the row's `is_active` already equals the job's target version (the transaction
committed and the activation is complete but for a roll nobody is waiting on), `failed` with
`error_code='daemon_restarted'` when it does not (the transaction never committed, so nothing
happened and the version list is unchanged). Either way the version row stays `ready`, which is what
§2.3a's activate column asserts.

#### 2.3a Subjects, and the one-job-per-activity rule (D55)

`jobs` is the **scheduling** record — lease, retry, backoff, cancellation, progress — and the domain
row is the **domain** record. There is exactly one live `jobs` row per domain row, both written in
the same transaction by the same worker, which is what keeps them from drifting. The mapping is
closed and enforced by the `subject_type` `CHECK` above:

| `jobs.kind` | `subject_type` | `subject_id` | domain row (state authority) |
|---|---|---|---|
| `llamacpp_install`, `llamacpp_activate`, `llamacpp_delete` | `llamacpp_version` | `llamacpp_versions.id` | `llamacpp_versions.state` |
| `model_download` | `download` | `downloads.id` | `downloads.state` (itself a fold of `download_tasks` — §2.7) |
| `model_verify`, `model_delete` | `model` | `models.id` | `models.state` |
| `cache_scan` | `cache_scan` | `cache_scans.id` | `cache_scans.state` |
| `bench_run` | `bench_run` | `bench_runs.id` | `bench_runs.state` |
| `self_update` | `self_update` | `self_updates.id` | `self_updates.state` |
| `toolchain_probe` | `system` | the constant `'toolchain'` | `toolchain_probes` (append-only; no state) |
| `maintenance` | `system` | the constant `'maintenance'` | none — the job row *is* the record |

The two `system` subjects are fixed synthetic ids precisely so the unique index still means
something: at most one toolchain probe and at most one maintenance pass may be live at a time.

**Keeping the two states consistent.** The domain state is the one the UI reads and the one the API
returns; `jobs.state` is what the queue schedules on. The worker writes both in one transaction, and
an integration test asserts the invariant table below after every transition. A domain row may hold
finer states than the queue (`downloads.resolving`, `verifying`) — those all fold to `running`.

**The `self_updates` `interrupted` cell spans every non-terminal state, not just the two around the
swap.** A daemon restarted while an update is still `downloading` or `verifying` leaves exactly that
pairing, and so does a restored database: the D14 snapshot is taken *during* `verifying` (§12.1
step 4), so the first triage after an F12 recovery or a `llamaman restore-db` pairs an `interrupted`
job with a `verifying` row. Neither is an anomaly to assert against; both are the confirmation gate's
input, and §11.1 step 11's closing pass is what resolves them.

**The `llamacpp_versions` column is per `jobs.kind`.** Three kinds share that subject and they do not
move the row the same way, so one column would assert states two of them never reach. The install
column below is `llamacpp_install`; `llamacpp_activate` and `llamacpp_delete` get their own rows
underneath, and the integration test that "asserts the invariant table after every transition" is
parameterized by kind rather than by subject.

| `jobs.state` | `downloads.state` | `cache_scans.state` | `bench_runs.state` | `self_updates.state` | `llamacpp_versions.state` (kind = `llamacpp_install`) |
|---|---|---|---|---|---|
| `queued` | `queued` | `queued` | `queued` | `planned` | `pending` |
| `leased`/`running` | `resolving`/`running`/`verifying` | `running` | `preflight`/`running` | `downloading`/`verifying`/`staged`/`swapping` | `resolving`/`fetching`/`building`/`verifying` |
| `paused` | `paused` | — (scans are not pausable) | — | — | — |
| `interrupted` | — (downloads return to `queued` on boot) | — | `running`, `restore_done=0` — the finalizer's input (§2.3, §10) | any non-terminal state — `downloading`/`verifying`/`staged`/`swapping` — the confirmation gate's input (§11.1 step 11) | the build states, with the directory kept (D4) |
| `succeeded` | `succeeded` | `succeeded` | `succeeded`/`partial` | `succeeded` | `ready` |
| `failed` | `failed` | `failed` | `failed`/`partial` | `failed` | `failed`/`failed_verification` |
| `canceled` | `canceled` | `canceled` | `canceled` | `canceled` — accepted only while the row is `planned`/`downloading`/`verifying`; at or after the `staged` commit `POST /jobs/{id}/cancel` answers `409 selfupdate_not_cancelable`, because the marker is already on disk and the swap belongs to systemd (D96, §12.1) | `canceled` |

| `jobs.state` | `llamacpp_versions.state`, kind = `llamacpp_activate` | kind = `llamacpp_delete` |
|---|---|---|
| `queued` | `ready` | `ready` |
| `leased`/`running` | `ready` — activation never leaves the `ready` state; it moves `is_active`/`previous_active`, `config_hash` and the symlink, all of which a failed canary reverts together (§6.6 step 5) | `deleting` |
| `interrupted` | `ready` — a half-applied activation is repaired from the row at boot and the job is then closed `succeeded` or `failed` by that finalizer (§2.3, §6.6) | — (a delete owes nothing durable and is triaged straight to `failed` at boot — §2.3) |
| `succeeded` | `ready`, `is_active=1` | `deleted` |
| `failed` | `ready`, with `is_active`/`previous_active` **restored to their pre-activation values** — a failed canary is exactly how this job fails, and §6.6 step 5 reverts the rows, the symlinks and `config_hash` in one transaction before it restarts the canary | `ready` when the directory verified intact, else `failed` — both are §2.5 edges out of `deleting` |
| `canceled` | `ready`, flags unchanged (a cancel is only accepted before the step-3 transaction commits) | `ready` |

The activate column's `failed` row and its `leased`/`running` row are deliberately the *same*
statement read at two moments: the flags move inside the activation transaction, and the only thing
that can undo them is the revert transaction of §6.6 step 5. There is no reading in which a canary
failure leaves `is_active` pointing at the build whose canary just failed — which matters because
§6.6's boot reconciliation makes the **row** win over the symlink, so a row left un-reverted would
silently re-activate that build on the next daemon start and restart every instance onto it.

Pause/resume therefore moves both rows: `POST /downloads/{id}/pause` sets `jobs.state='paused'` and
`downloads.state='paused'` in one transaction, releasing the lease; resume returns both to
`queued`. This is why `paused` is a `jobs` state rather than a downloads-only concept — without it,
a paused download would either hold a lease forever or free its subject for a duplicate job.

### 2.4 Toolchain and hardware

```sql
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
```

Live VRAM/utilization samples are **not** persisted (write churn, and SPEC §5.8 forbids telemetry of
any kind — including to ourselves). They live in a per-GPU in-memory ring and stream over SSE; only
the last sample is exposed by the API. A failed `nvidia-smi` invocation marks every GPU `unknown`
and the API returns `null` fields — never zeros (F14).

### 2.5 llama.cpp versions

```sql
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
```

**Version transitions** (every one writes an `events` row):

| from | event | to | side effect |
|---|---|---|---|
| — | `POST /llamacpp/versions` | `pending` | insert row + `llamacpp_install` job |
| `pending` | worker leases | `resolving` | resolve channel → tag → asset, or `git ls-remote` the ref |
| `resolving` | prebuilt asset exists, backend=cpu, preference on | `fetching` | download tarball to `tmp/` |
| `resolving` | backend=cuda, custom, no asset, or prebuilt rejected by D18 | `building` | fetch + configure + compile |
| `fetching` | hardened extract ok | `verifying` | D18 execute-on-this-host check |
| `building` | compile + install exit 0 | `verifying` | D18 + D19 checks |
| `verifying` | ok | `ready` | write `manifest.json` (including the `llama-server --help` capture), `binaries_json`, `size_bytes`, **`help_flags_json` and `supports_fit`** |
| terminal failure (`failed`/`failed_verification`/`canceled`/`deleted`) | `POST /llamacpp/versions` for the same id, or `…/retry` | `pending` | **reuse-and-reset (D71)**: clear `error_code`/`error_message`/`failing_step`/`exit_code`/`superseded_by`/`finished_at`, rotate `logs/build/<id>.log` to `<id>.log.<n>`, enqueue a fresh `llamacpp_install` job. The prior failure survives in `events` and the rotated log |
| `verifying` | prebuilt fails to execute | `failed_verification` | auto-enqueue a CPU **source** build as a *new row* — id `<tag>-cpu-src` beside the failed `<tag>-cpu-bin` (D60) — link the two through `superseded_by`, carry the glibc diagnosis into the new row's `params_json`, and keep the failed row and its log as the record of why |
| `verifying` | CUDA build lists no CUDA device | `failed_verification` | terminal; log kept |
| any pre-`ready` | error | `failed` | keep log + `failing_step` |
| any pre-`ready` | cancel | `canceled` | SIGTERM process group, remove partial dirs |
| `ready` | activate | `ready`, `is_active=1` | prior active → `previous_active=1`; `RecomputeConfigHash`; symlink flip; canary roll |
| `ready`, `is_active=1` | **canary failed (§6.6 step 5)** | `ready`, `is_active` and `previous_active` restored to their pre-activation values | one transaction: revert both flags, re-run `RecomputeConfigHash` for every non-deleted instance, repair `versions/active`/`previous` from the restored rows, then restart the canary onto the old build and abort the roll |
| `ready` | delete (guard: not active, not previous, not `/proc/*/exe`) | `deleting` → `deleted` | remove dir |
| `deleting` | removal failed, or the daemon restarted mid-delete, and `bin/llama-server` is still present and executes | `ready` | the version is usable again and reappears in the list; the failure is in `events` and in the job row |
| `deleting` | removal failed and the directory is incomplete | `failed` | `failing_step='delete'`, `error_code='delete_incomplete'`; the UI offers Delete again, which retries the removal |

Symlink invariant: `versions/active` always points at the `is_active=1` row's directory; the flip is
`symlink(new, .active.tmp)` + `rename(.active.tmp, active)`, atomic, so a crash mid-activate leaves a
consistent pointer. On boot the row wins and the symlink is repaired from it.

**The row winning is why a canary revert must be a database write, not a filesystem operation.**
Flipping `versions/active` back while leaving `is_active=1` on the build whose canary just failed
does not undo the activation — it creates a disagreement that the next boot resolves *in favor of the
failed build*, re-pointing the symlink at it and restarting every instance onto it. §6.6 step 5
therefore reverts the rows first and repairs the symlinks from them, in that order, exactly as
activation does.

### 2.6 Hugging Face cache, models, files

```sql
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
```

Model transitions: `planned → downloading → verifying → ready`; `downloading ⇄ incomplete` (pause /
resume); `ready → missing` when a scan finds the files gone (never silently deleted — a disk may be
unplugged); `ready → corrupt` when verification fails; `ready|missing|corrupt → deleting → deleted`
under the in-use guard. A scan may insert directly as `ready` with `origin='scanned'`.

### 2.7 Downloads

```sql
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
```

`downloads.state` is a stored fold of its tasks (any running → `running`; all succeeded →
`verifying` → `succeeded`; any failed past retries → `failed`). It is stored so list queries stay
single-table; the fold function in `internal/hf/download` is the single writer and a property test
asserts stored state always equals the fold of the task rows.

There are therefore **three** layers over one download, and §2.3a fixes their relationship so they
cannot drift: exactly one `jobs` row (`subject_type='download'`, `subject_id=downloads.id`) carries
scheduling; `downloads.state` carries the domain state; `download_tasks` carry per-file state and are
folded upward. The download worker writes the job row and the domain row in the **same transaction**,
using the §2.3a mapping table, and pause/resume moves both (`jobs.state='paused'` exists for exactly
this reason). `POST /api/v1/downloads` creates all three in one transaction, so
`idx_jobs_one_live_per_subject` makes a second live job for the same download structurally
impossible; a repeat request returns `409 download_exists` naming the existing one.

### 2.8 Instances

```sql
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
```

`flags_json` is `model.FlagSet` (D41). `null` means "do not pass the flag", which is distinct from a
zero value:

```jsonc
{
  "ctx_size": 8192,                 // -c   (TOTAL context, shared across -np slots)
  "n_gpu_layers": {"mode":"all"},   // -ngl {"mode":"auto"|"all"|"none"|"count","count":N}
                                    //   all→`-ngl 999`, none→`-ngl 0`, count→`-ngl N`,
                                    //   auto→NO -ngl flag at all (D51/§5.7): llama.cpp's own
                                    //   --fit picks the offload, and stays enabled to do it
  "batch_size": 2048,               // -b
  "ubatch_size": 512,               // -ub
  "parallel": 4,                    // -np
  "threads": null,                  // -t
  "threads_batch": null,            // -tb
  "flash_attn": "auto",             // -fa on|off|auto
  "cache_type_k": "f16",            // -ctk
  "cache_type_v": "f16",            // -ctv
  "split_mode": "layer",            // -sm none|layer|row
  "tensor_split": [0.5, 0.5],       // -ts   indices are into the --device list, not nvidia-smi
  "main_gpu": 0,                    // -mg   likewise
  "device_filter": "CUDA0,CUDA1",   // --device — the ONLY device selector (D66); rendered verbatim
  "device_uuids": ["GPU-a1…","GPU-b2…"],
                                    // provenance for device_filter: the GPU UUIDs the user actually
                                    // picked, resolved to CUDA<n> at save time. Not rendered into
                                    // argv; the supervisor compares it against the live map and
                                    // raises F22 when the ordering changed under the instance.
  "mlock": false, "no_mmap": false, // --mlock / --no-mmap
  "cont_batching": true,            // -cb / -nocb
  "embedding": false,               // --embedding
  "pooling": null,                  // --pooling
  "rerank": false,                  // --reranking
  "alias": "qwen3-8b",              // --alias
  "chat_template": null, "chat_template_file": null, "jinja": true,
  "rope_scaling": null, "rope_freq_base": null, "rope_freq_scale": null,
  "yarn_ext_factor": null, "yarn_attn_factor": null,
  "n_keep": null, "n_predict": null, "defrag_thold": null, "cache_reuse": null,
  "numa": null, "cpu_mask": null, "prio": null,
  "slot_save_path": null,
  "draft": {"n_max":16,"n_min":0,"p_min":0.75,"ctx_size":null,"n_gpu_layers":null},
  "props_endpoint": true, "slots_endpoint": true, "metrics_endpoint": true,
  "log_verbosity": null
}
```

Anything not modeled here goes in `extra_flags` (SPEC §3.3's escape hatch), so no upstream flag is
ever unreachable and no upstream flag addition is a migration.

**Who writes `instances` and `instance_status`.** The rule is "the API writes config, the supervisor
writes status", with exactly **seven** named exceptions to the first half, each of which bumps
`updated_at`, emits an `events` row, and deliberately does **not** bump `generation` (so an admin's
in-flight `PATCH` is never rejected by a housekeeping write it could not have known about, and the
optimistic-concurrency contract keeps meaning "someone edited the config under you"):

| column | exceptional writer | why it is not a config edit |
|---|---|---|
| `internal_port` | supervisor, after an exit 78 | an allocation detail, invisible in `config_hash` (D52) and never user-chosen |
| `pending_trigger` | daemon before `StartUnit`; cleared by `instance-exec` | a hand-off channel, not configuration (§5.6) |
| `pending_override_json` | daemon before `StartUnit` (safe start); cleared by `instance-exec` | the same hand-off channel, carrying a one-shot argv patch (D61) |
| `desired_state` | supervisor, on the first pass after a host boot only | the autostart coupling of D53, below |
| `draft_validation` | models service, when a draft or primary model finishes GGUF parsing | a re-check of a validation that was deferred because the metadata did not exist yet (D34, §3.10a) |
| `config_hash` | **llama.cpp activation** (§6.6 step 3), for every non-deleted instance, inside the activation transaction | D52 folds the active version id into the hash; a version flip changes that input for everyone at once. Nobody edited a configuration — the runtime under it moved (D69) |
| `config_hash` | **models service**, when a model this instance references gains, loses or changes its resolved snapshot path (download completes, re-verify finds it `missing`, a rescan re-points it) | D52 also folds in the resolved model paths. Same argument: the input moved, the configuration did not |

#### `config_hash` is maintained, not merely stamped (D69)

`instances.config_hash` is a **stored** column over three inputs — the rendered argv with the
listener identity elided, the resolved model paths, and the active version id (D52) — and only the
first of those is user-edited. Leaving the other two unmaintained is a silent failure, not a cosmetic
one: after a version flip the stored hash still equals `applied_config_hash`, `restart_required` never
lights, the "restart to apply llama.cpp b10621" prompt never appears, and the whole comparison stops
carrying information. So there is exactly one recomputation path:

- **One method.** `instances.RecomputeConfigHash(tx, instanceIDs …)` re-renders argv through the pure
  `RenderArgv` (§5.7) for each instance, recomputes the hash, and writes it. It is the only writer of
  the column outside `POST`/`PATCH`.
- **Two callers.** `POST /llamacpp/versions/{id}/activate` and `POST /llamacpp/rollback` call it for
  **every non-deleted instance** in the same transaction that sets `is_active` (§6.6 step 3), *before*
  the symlink flip and any canary roll. The models service calls it for every non-deleted instance
  referencing the affected model, in the transaction that changes that model's `snapshot_dir` /
  `primary_file` / `state`.
- **`generation` is untouched**, exactly as for the other five exceptions: a user's open edit form
  must not be invalidated by a version flip they did not make.
- **`applied_config_hash` is untouched too**, which is the entire point — that is what makes
  `restart_required` become true for every running instance the moment a new llama.cpp version is
  activated, and what makes the canary roll's job visible ("4 of 7 still on b10604").

**`instance_status` has exactly three exceptional writers, all of them recovery actions the user
asked for synchronously.** The general rule stands — the supervisor owns the table — and the columns
that carry the correctness argument are supervisor-only without exception:

| column | who may write it besides the supervisor | why |
|---|---|---|
| `restart_window_reset_at` | `POST /instances/{id}/reset-failed`, `POST /instances/{id}/safe-start` | D64's crash-loop cutoff ignores rows at or before this instant. "Reset failed" that took effect on the *next* supervisor pass would be a button that changes a label and nothing else; the write has to land in the request's own transaction, because the very next thing the user does is press Start |
| `reconcile_backoff_until` | the same two endpoints (cleared to NULL) | F3's recovery is "try again **now**". Leaving a five-minute backoff in place would make Safe start appear to do nothing |
| `state`, for the single transition `crash-looping → stopped` | the same two endpoints | clearing the crash-loop latch *is* the operation. No other transition is reachable from the API — an API handler may not write `ready`, `failed`, `degraded`, `starting`, `loading`, `stopping` or `unknown`, ever |

Everything else in the table remains **supervisor-only, and one column emphatically so**:
`applied_config_hash`. `instance-exec` does *not* stamp it. It records the hash it actually rendered
in `instance_starts.effective_config_hash` and then `execve`s; the supervisor copies that value into
`instance_status.applied_config_hash` at the moment it stamps `instance_starts.ready_at` — i.e. when
`/health` first returns 200 and the configuration has demonstrably loaded. A launcher that reached
`execve` and then died during model load therefore leaves `applied_config_hash` untouched, so
`restart_required` does not clear for a configuration that never ran. That argument survives the three
exceptions above intact, because none of them touches the hash, the health columns, `exe_version_id`,
`main_pid`, `vram_bytes`, `gpu_uuids_json` or `device_map_json`.

**The row's lifecycle.** `instance_status` is `PRIMARY KEY REFERENCES instances(id)` with three NOT
NULL columns and no defaults for two of them, so it cannot spring into existence lazily. It is
**inserted by the instances service in the same transaction as the `instances` row** —
`state='unknown'`, `last_change_at=now`, `restart_window_reset_at=0`, everything else NULL — which is
the only non-supervisor *INSERT* into the table and is not an exception to the update rules above.
Every reader may therefore assume the row exists: `GET /instances` joins config ⋈ status with an
inner join, and the derived-flag block below reasons about a brand-new instance as `state='unknown'`
with no closed start row rather than about an absent row. A soft delete leaves the row in place (the
start history and the last observed state stay readable); `?purge=true` cascades it away with
everything else.

**Instance state, two axes kept deliberately separate.** *Desired* (`instances.desired_state`, set
by the API and — once per host boot — by the coupling rule below). *Actual*
(`instance_status.state`, set only by the supervisor):

| from | trigger | to |
|---|---|---|
| `stopped`/`failed`/`unknown` | supervisor starts the unit, or an **external** start is observed (`systemctl start` by hand, `llamaman-instances.target` at boot) | `starting` |
| `starting` | unit active, `/health` 503 `loading model` | `loading` |
| `starting`/`loading` | `/health` 200 | `ready` |
| `starting`/`loading` | `instances.start_timeout_sec` elapsed, or unit failed | `failed` |
| `ready` | 3 consecutive health failures, unit still active | `degraded` |
| `degraded` | `/health` 200 | `ready` |
| `ready`/`degraded`/`loading` | stop requested | `stopping` → `stopped` |
| **`ready`/`degraded`** | **the unit goes inactive or failed with no stop requested — `llama-server` exited on its own, cleanly or not** | **`failed`** |
| `stopping` | unit inactive | `stopped` |
| `failed` | > `restart_max` **failed** starts within `restart_window_sec`, counted per D64 (§5.8) | `crash-looping` |
| `failed`/`crash-looping` | stop requested (`desired_state := 'stopped'`) | `stopped` |
| `crash-looping` | `POST …/reset-failed` or `POST …/safe-start` | `stopped` — the only actual-state transition an API handler may write (see the writer table above) |
| `unknown` | unit properties readable again | whichever of `stopped`/`starting`/`loading`/`ready`/`failed` the properties and the health probe say — `unknown` is a *gap in observation*, so leaving it is a re-derivation, not a transition with its own rule |
| any | unit gone / systemd unreachable | `unknown` |

**The `ready → failed` row is load-bearing, not tidiness.** A `ready` instance whose `llama-server`
exits by itself — a segfault, an upstream `exit(0)` on an internal error, an operator's `kill` — had
no destination in an earlier reading, and two other rules depend on it having one. The §5.6 writer
table closes that run's ledger row `stopped` when the exit status is clean and no stop was requested,
and `inhibit_reason='clean_exit'` is defined as exactly `restart_policy='on-failure'` **and**
`LAST_CLOSED.outcome='stopped'` **and** `state IN ('failed','crash-looping')`. If a clean unrequested
exit from `ready` did not land in `failed`, that reason would be unreachable and `on-failure`'s
central promise — "a clean exit is not a failure, so we do not restart it, and we tell you why" —
would have no state to be visible from. An **unrequested** exit is `failed` regardless of exit code;
the exit *code* decides the ledger `outcome`, and the ledger `outcome` decides whether the supervisor
restarts. Those are two different questions and the table answers them separately on purpose.

**Derived flags — computed on read, never stored, never states.** All four are returned by
`GET /instances` and `GET /instances/{id}` as booleans beside `state`, and the UI renders them as
badges. Keeping them out of the `state` column is what lets an instance be simultaneously `ready`
(so `/health` returns 200, `restart_required` can be true, and degraded↔ready still has a path) and
flagged. Two named rows appear below and they are **not** the same row:

- `LAST_CLOSED` — "the `instance_starts` row for this instance with the greatest `at` among rows
  whose `outcome IS NOT NULL` **and `outcome != 'inhibited'`**", which, because `outcome` is written
  exactly once at the end of a run (D63), is unambiguously **the last completed run**. It describes
  something that is over.

  **The `inhibited` exclusion is load-bearing, not tidiness.** An `inhibited` row is a record of a
  *refusal to start*, not of a run: no `execve` happened, no `exit_code` exists, nothing ended. If it
  counted as `LAST_CLOSED`, the supervisor would destroy the very condition it just evaluated —
  `inhibit_reason='clean_exit'` is defined below as `LAST_CLOSED.outcome='stopped'`, so writing the
  refusal row would make that clause false on the next pass and the `inhibited` badge would vanish
  while the instance was still, demonstrably, inhibited. The §5.8 restart policy reads the same
  definition for the same reason. Refusals are visible in `GET /instances/{id}/starts`, where they
  belong; they are never the "previous start".
- `THE_OPEN_ROW` — the at-most-one row with `outcome IS NULL`, made at-most-one by the **unique**
  partial index `idx_instance_starts_open` rather than by convention, i.e. **the run that is
  happening now**. It is NULL when nothing is running.

```
restart_required = (instances.config_hash != instance_status.applied_config_hash
                    OR THE_OPEN_ROW.override_json IS NOT NULL)    -- running in safe mode (D61)
                   AND state IN ('ready','degraded')

stale_version    = instance_status.exe_version_id IS NOT NULL
                   AND exe_version_id != <the is_active=1 llamacpp_versions.id>
                   AND state IN ('ready','degraded','loading')      -- D25/F8: badge only, still serving

inhibited        = state IN ('failed','crash-looping')
                   AND instances.desired_state = 'running'
                   AND the supervisor is declining to restart, i.e.
                       restart_policy = 'never'
                       OR state = 'crash-looping'
                       OR (restart_policy = 'on-failure' AND LAST_CLOSED.outcome = 'stopped')

draft_unverified = instances.draft_validation = 'deferred'          -- D34, §3.10a
```

Two NULL hazards are closed by construction. `inhibited` no longer reads `exit_code`, which was NULL
for exactly the rows the old rule needed it on; it keys off `outcome`, which is never NULL on a
closed row and never `'ready'` (D63). And when there is **no** closed row at all — a brand-new
instance, whose `instance_status` row exists with `state='unknown'` — every clause above is false, so
a never-started instance is not `inhibited`, not `restart_required` and not `stale_version`.

**`restart_required` reads the open row, never the closed one, and that is a correction.** An earlier
reading also OR'd `LAST_CLOSED.override_json IS NOT NULL`, which latched the flag permanently: once a
safe start had ended and an ordinary start had taken over, the safe start *was* `LAST_CLOSED`, so the
clause stayed true forever — for an instance whose `config_hash == applied_config_hash` and whose
saved configuration was demonstrably the one running. It also contradicted §3.10b step 4's explicit
promise that the next start is the saved configuration again. The flag has to describe the run that
is happening now, so it reads `THE_OPEN_ROW`: true while the safe start is live, false the moment an
ordinary start replaces it, with no user action needed to clear it. (The clause was a leftover from
before D63 removed `ready` from `outcome`, when "the most recent closed row" could still describe a
run in flight.) The safe start remains permanently visible where it belongs — in
`GET /instances/{id}/starts`, which shows every override inline (§3.10b).

`inhibited` carries a machine-readable `inhibit_reason` (`policy_never` | `crash_loop` |
`clean_exit`) so the remediation card can say which of the three it is; the supervisor also writes
an `instance_starts` row with `outcome='inhibited'` and `error_code = <the inhibit_reason>` at the
moment it declines, so the refusal is in the history rather than only in the UI. Those rows are
**never counted** toward the crash-loop cutoff (D64) — a refusal that made itself more certain by
being recorded would be a state no user could leave.

**One row per refusal episode, not one per pass.** Declining is the supervisor's single corrective
action for that pass, and the reconciler runs every `instances.health_poll_sec` (default 5 s), so an
unconditional write would add ~17 000 rows a day against a 500-row-per-instance retention cap and
bury the actual start history of a `restart_policy='never'` instance within an hour. The rule is
therefore conditional: the supervisor writes an `inhibited` row **only when there is no existing
`inhibited` row for this instance whose `at` is greater than `LAST_CLOSED.at` and whose `error_code`
equals the current `inhibit_reason`**. A refusal that repeats indefinitely records exactly one row;
a *new* episode — a new completed run, or a changed reason — records a new one. The `events` row and
the derived badge are unconditional, because neither accumulates.

**Autostart ⇄ desired_state coupling (D53).** `instances.autostart` means "the unit is enabled in
`llamaman-instances.target`", which is a statement about *host boots*; `desired_state` is a
statement about *now*. They are joined at exactly one point, and nowhere else:

- **First supervisor pass after a host boot** — detected by `runtime_info.host_boot_id` differing
  from the value read from `/proc/sys/kernel/random/boot_id`, a comparison made **exactly once per
  daemon start, by supervisor boot reconciliation step 1, which is also the only writer of that
  column** (§5.8; §11.1 step 9 reads it and writes nothing) — every non-deleted instance gets
  `desired_state := autostart ? 'running' : 'stopped'`, written before the first reconciliation and
  logged as one `events` row per change. This is the *only* automatic write to `desired_state`.
  - It fixes `autostart=1, desired_state='stopped'`: systemd has already started the unit, the
    reconciler now agrees, and nothing is killed a second after boot.
  - It fixes `autostart=0, desired_state='running'`: systemd did not start the unit, the reconciler
    does not either, and "autostart off" actually means off.
- **Daemon restart within the same host boot** (`host_boot_id` unchanged) leaves `desired_state`
  untouched, so the D7 property still holds: an instance that crashed while the daemon was down is
  restarted when the daemon returns.
- `PUT /instances/{id}/autostart` only enables or disables the unit. It never starts or stops
  anything — but when it enables autostart on a stopped instance the response carries
  `"hint":"start_now"` and the UI offers a Start button, and when it is disabled on a running
  instance the UI notes the instance will not come back after a reboot.
- `POST /instances/{id}/stop` on an instance with `autostart=1` returns
  `"hint":"will_start_at_boot"`, so "stop" never silently means "until the next reboot".

**Port rules.** `UNIQUE` and the 1024–65535 `CHECK` are the floor, not the contract. Both ports are
validated in `internal/instances` on `POST /instances` and on every `PATCH` that touches them, and a
violation is `422 port_unavailable` with `{"port":N,"reason":…}` — an input error at save time
rather than F6's runtime bind banner:

| rule | applies to | reason code |
|---|---|---|
| not equal to `ui.port_desired`, nor to `runtime_info.ui_port` (the port the walk actually landed on) | public | `reserved_management` |
| outside `[instances.internal_port_min, instances.internal_port_max]` | public | `reserved_internal_pool` |
| inside that pool | internal | `outside_internal_pool` |
| not held by another instance (either column) | both | `in_use_by_instance` |
| a live bind probe on `gateway.bind`:port succeeds and is released | public | `bind_failed` |
| a live bind probe on `127.0.0.1`:port succeeds and is released | internal | `bind_failed` |

The probe is advisory — another process can take the port between the probe and the listen — which
is exactly why F6 still exists as a runtime fallback. `GET /ports/suggest` applies the same rules,
and `PATCH /settings` refuses a `ui.port_desired` that collides with an existing `public_port`
(`400 setting_invalid`, reason `in_use_by_instance`).

**The management-port walk obeys the same rules (§11.1 step 7).** Save-time validation is only half
the guarantee: the walk that runs when `ui.port_desired` is transiently occupied could otherwise land
the management UI on a port an instance owns, whereupon that instance's gateway listener fails to
bind and degrades to the F6 banner — the UI silently squatting on an inference port. So the walk is
not a bare "next free port". Its candidate set is `[desired, desired+20]` **minus** every port
appearing in `instances.public_port` or `instances.internal_port` for a non-deleted instance, minus
the `[internal_port_min, internal_port_max]` pool, and each remaining candidate must then bind. The
walk runs after the DB is open (step 3) and therefore has those rows available; it happens *before*
the gateway listeners open (step 10), which is precisely why it must consult the table rather than
the live socket state. If **every** candidate is excluded or occupied, the daemon binds an ephemeral
port (`:0`) rather than refusing to start — a management UI that cannot be reached is a host nobody
can repair — records it in `runtime_info.ui_port`, logs it at `warn`, publishes it through
`sd_notify STATUS=`, and raises a `ui_port_exhausted` notification naming the real URL and the
colliding instances.

### 2.9 Tokens and accounting

```sql
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
```

Token transitions: `active ⇄ disabled`; `active|disabled → revoked` (terminal, and the hash is
retained so a leaked secret can never be re-minted into validity). Every write bumps an in-memory
epoch that invalidates the gateway's verified-token cache within one request.

**Three counters, three sources, never conflated** — the UI labels each one, because they legitimately
disagree:

| number | source | covers |
|---|---|---|
| `instance_usage_daily` | the gateway itself | every request through the public port, both auth modes: requests, errors, bytes, wall duration |
| `token_usage_daily` | the gateway, keyed by credential | the per-token subset, plus `prompt_tokens`/`completion_tokens` when the upstream reported them |
| `instance_status.requests_served` | llama-server `/metrics` | requests the *model* served, including any the gateway did not see; **NULL when `metrics_endpoint` is off**, and the UI then says "metrics disabled" rather than showing 0 |

Disabling `--metrics` therefore costs token totals and slot-level truth, never the gateway's own
request, byte and error counts. `retention` treats both usage tables like `events`: rolled up
nightly, never auto-deleted below 400 days.

### 2.10 Benchmarks

```sql
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
```

### 2.11 Self-update, events, notifications, fit calibration, wizard

```sql
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
```

**Retention**, applied by the nightly `maintenance` job (subject `('system','maintenance')` — §2.3a),
all tunable: `events` 90 days or 200k rows; `login_attempts` 30 days; `instance_starts` 500 per
instance (**closed rows only** — an open row is never pruned out from under the supervisor);
`sessions` past `expires_at + 7d`; `fit_observations` 2000 rows; `db-backups/` **the newest 7, oldest
deleted first, and the newest snapshot is never deleted** whatever the count is tuned to — a snapshot
is taken only immediately *before* an update and is labeled with the version it replaces, so the
newest one is by construction the database as the version now at `<prefix>/llamaman.prev` left it,
which is exactly the file §12.4's downgrade procedure restores (D14); `llamaman.db.superseded-*`
30 days (§12.4);
`idempotency_keys` past `expires_at + 24h` (D65); `jobs` in a terminal state
(`succeeded`/`failed`/`canceled`) 90 days, never a live or `interrupted` row;
`notifications` dismissed + 30 days; `instance_usage_daily` and `token_usage_daily` 400 days.
**Benchmarks are never auto-deleted** — they are the product.

---

## 3. REST API

**Conventions.**

- Base path `/api/v1`. Router: stdlib `net/http.ServeMux` with Go 1.22+ method+pattern routes. No
  third-party router; middleware is ~150 lines of `http.Handler` wrapping.
- **Auth**: session cookie `lm_session` (HttpOnly, SameSite=Lax, Path=/, `Secure` only when the
  request arrived over TLS). Value `<session_id>.<secret>`; only `sha256(secret)` is stored.
- **CSRF**: double-submit. Login sets a non-HttpOnly `lm_csrf` cookie holding an HMAC of the
  session's `csrf_secret`; every non-GET must echo it in `X-CSRF-Token`. `Origin`/`Sec-Fetch-Site`
  are additionally checked when present.
- **Errors**: `{"error":{"code":"model_in_use","message":"…","details":{…}}}` with a matching status.
  Codes are a closed enum in `internal/model`, mirrored into TypeScript by the generated schema.
- **Lists**: `{"items":[…],"total":N,"next_cursor":"01J…"|null}`; ULID keyset pagination.
- **Timestamps** are RFC 3339 UTC strings on the wire; byte counts are plain JSON numbers of bytes;
  durations are integers with a `_ms` suffix. (Milliseconds are the storage form; the wire form is
  chosen for readability, and the conversion lives in the DTO layer only.)
- **Concurrency**: `PATCH` on instances and presets requires `"generation"`; a mismatch is
  `409 conflict_generation`.
- **Long actions never block**: anything that starts work returns `202` with
  `{"job_id":"…","subject":{…}}`; progress arrives over SSE.
- **Idempotency (D39/D65)**: job-creating POSTs accept an optional `Idempotency-Key` header. A repeat
  within 10 minutes returns the original job with `200` instead of creating a second one; the same
  key with a different body inside the window is `422 idempotency_key_reused`; **after** the window
  the key is free again and creates a new job. The window lives in the `idempotency_keys` table with
  an `expires_at`, not in a permanent unique index — see §2.3. Behind that,
  `idx_jobs_one_live_per_subject` makes two live jobs on one subject impossible regardless — except
  where the subject id is minted per attempt, which is why builds and benches carry a singleton lease
  (D70/D75) and `POST /update/apply` evaluates its guard inside the transaction that inserts (D97).
- **`429`** is used for exactly one thing: `POST /system/restart` while this boot has not yet cleared
  its unit's start-limit counter (D93, §3.3). It carries `retry_after_ms`, and the UI disables the
  button for that long rather than spending a start the revert deadline needs.
- **Setup gate**: until `admin_account` exists, every `session` endpoint returns
  `409 {"code":"setup_required"}`. The SPA routes to the wizard on that code alone, so there is no
  separate "is it configured" flag to keep in sync.
- **Auth column below**: `session` = admin session + CSRF; `setup` = allowed only before the account
  exists, and then only from loopback or with a valid `X-Setup-Token` (D38); `public` = none;
  `token` = the gateway ports, which are not this API.
- **Contract enforcement (D43)**: `api/openapi.json` is generated from the route registry and
  drift-checked in CI, and a response-conformance middleware runs in integration tests — an
  undocumented endpoint, a missing documented field, or an extra field fails the suite.

### 3.1 Health and auth

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/healthz` | public | `200 {"status":"ok","version":"…"}` — liveness only, no state |
| GET | `/api/v1/meta` | public | `{"version","commit","setup_complete","claimed","ui_port"}` — what `install.sh` polls |
| GET | `/api/v1/auth/session` | public | `{"authenticated","setup_complete","expires_at"}` |
| POST | `/api/v1/auth/login` | public | `{"password"}` → `204` + cookies; `401 bad_credentials`; `429 locked_out` + `{"retry_after_sec"}` |
| POST | `/api/v1/auth/logout` | session | `204`, session revoked |
| POST | `/api/v1/auth/password` | session | `{"current","next"}` → `204`; revokes all other sessions |
| GET | `/api/v1/auth/sessions` | session | active sessions (ip, ua, last seen) |
| DELETE | `/api/v1/auth/sessions/{id}` | session | `204` |

### 3.2 Setup wizard

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/setup/state` | public | `{"claimed","token_required":bool,"steps":[{"step":"password","state":"active"},…]}` — `token_required` is false for loopback callers |
| POST | `/api/v1/setup/password` | setup | `{"password"}` (+ `X-Setup-Token` unless loopback) → `204` + session cookie; creates `admin_account`, stamps `setup_claim.claimed_at` |
| GET | `/api/v1/setup/toolchain` | session | latest probe |
| POST | `/api/v1/setup/toolchain/recheck` | session | `202` job |
| POST | `/api/v1/setup/llamacpp` | session | delegates to §3.5 |
| PUT | `/api/v1/setup/hf-token` | session | validates via `/api/whoami-v2`, stores sealed |
| POST | `/api/v1/setup/cache/scan` | session | `202` scan job |
| POST | `/api/v1/setup/skip` | session | `{"step":"models"}` |
| POST | `/api/v1/setup/complete` | session | `204` |

### 3.3 System

| Method | Path | Auth | Response |
|---|---|---|---|
| GET | `/api/v1/system/info` | session | version, uptime, service identity, `systemd_scope`, `systemd_control`, `polkit_ok`, ui port/url, state dir, HF home, kernel, CPU, RAM |
| GET | `/api/v1/system/toolchain` | session | per-tool `{name,found,path,version,min_version,ok,note,docs_url}` incl. gcc/g++, cmake, ninja, make, git, nvcc, driver, glibc, free space |
| POST | `/api/v1/system/toolchain/probe` | session | `202` |
| GET | `/api/v1/system/gpus` | session | `[{id,index,name,uuid,vram_total,vram_used,vram_free,util_pct,temp_c,driver,cuda,compute_cap,state}]` — `state: ok\|unknown` |
| GET | `/api/v1/system/disk` | session | per cache root and state dir: total/free/used, model bytes, version bytes |
| GET | `/api/v1/system/journal` | session | `?unit=llamaman&lines=500&follow=1` → JSON lines, or SSE when `Accept: text/event-stream`. `409 journal_unavailable` carrying the F23 remediation when `runtime_info.journal_read != 'ok'` (D77) — an empty stream and a denied one must not look alike |
| GET | `/api/v1/system/notifications` | session | undismissed notifications with remediation actions |
| POST | `/api/v1/system/notifications/{id}/dismiss` | session | `204` |
| GET | `/api/v1/system/units` | session | per unit: the `# llamaman-units: <N>` stamp `install-units` wrote, the installed content hash vs. the template the running binary would render (for the recorded identity, prefix and port), `drift: none\|stale\|missing`, plus the `*.wants`/`*.requires`/masked-unit diff behind F21, and the exact repair command (§5.4a). **`stale` — an older or absent stamp — is the ordinary state of a host that has self-updated across a release which changed a template, and it blocks nothing** (D95); `missing`, and a hash mismatch at the *current* stamp, are F16. Read-only — the daemon cannot write `/etc` |
| POST | `/api/v1/system/restart` | session | `202 {"job_id":null,"unit":"llamaman.service","listener_continuity":"fdstore"\|"none","drain_sec":20}` — see §9.4 for the exact ordering (commit → flush the 202 → drain → hand listeners to the fd store → **non-blocking** `RestartNoWait`). `409 job_in_flight` while a build or self-update is live (D4); `409 restart_unavailable` when `systemd_control='unavailable'`, in which case the `restart_required` flag is advisory and the response carries `sudo systemctl restart llamaman.service`; **`409 systemd_denied` when the name-scoped `manage-units` grant on `llamaman.service` was refused** (F9, §5.2 branch (b)), carrying both `sudo systemctl restart llamaman.service` and `sudo llamaman install-units --repair-polkit`; and **`429 restart_rate_limited` while this boot has not yet cleared its unit's start-limit counter** (D93), because the daemon has been ready for less than 60 s — the response carries the seconds remaining and the UI disables the button until then. **The 409 wins, and the 429 has exactly one reason.** `RestartUnit` and `ResetFailedUnit` on `llamaman.service` are authorized by the *same* polkit action on the *same* unit name (§5.2 branch (b) grants `manage-units` for that name and lists Start/Stop/Restart/ResetFailed together), so a host that refuses the reset refuses the restart too and never reaches the rate-limit branch at all: the denial is evaluated first and is the honest answer, since there is nothing for the endpoint to spend. An earlier reading gave the 429 a second sub-reason — "because `ResetFailedUnit` was refused" — and it named a state this endpoint cannot be in; it is removed rather than corrected. What that denied-grant host *does* have is the residual §11.1a states and **F26** records. That guard is the reason a restart button can never exhaust `StartLimitBurst=` and leave the host with a `failed` unit, no daemon and released gateway ports. Instances are untouched (§5.5) |
| GET | `/api/v1/system/capabilities` | session | `{"systemd_control","systemd_scope","polkit_ok","polkit_unit_files","listener_continuity","instance_control":bool,"autostart_control":bool,"self_update":bool,"self_update_revert":bool,"journal_read":"ok"\|"denied"\|"unavailable"}` — the single object the UI reads to decide which controls to disable and which explanatory copy to show. `self_update_revert` is §5.4a's drift check answering whether `llamaman.service` still carries `OnFailure=llamaman-update-verify.service` and that unit is installed and unmasked; it is what `POST /update/apply` refuses on (`409 revert_unavailable`, §12.1 step 1), so the UI disables the button with the `install-units` command rather than letting the click fail. §11.1a defines every degraded combination. Both `self_update` and `self_update_revert` are answered by **reading the installed units' own directives** — `OnFailure=` on `llamaman.service`, and the presence and mask state of `llamaman-update-verify.service` and (system scope) `llamaman-selfupdate.service` — and never by "the drift check reports no drift", which would turn every unit-template change into a permanent refusal (D95) |

### 3.4 Settings

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/settings` | session | `{"values":{…},"schema":[{key,type,default,min,max,enum,label,group,restart_required,unit_change_required}]}` |
| PATCH | `/api/v1/settings` | session | partial; per-key `400 setting_invalid`; response flags `restart_required` and, when set, offers `POST /system/restart` |
| POST | `/api/v1/settings/reset` | session | `{"keys":[…]}` → deletes rows, built-in defaults resume |

**Secrets are not settings.** The two credentials in `secrets` — the Hugging Face token and the
optional GitHub token (§2.2) — never appear in `GET /api/v1/settings`, because a settings value is
returned in the clear and these must not be. Each has its own validating triple
(`GET`/`PUT`/`DELETE /api/v1/hf/token` and `/api/v1/github/token`, §3.6) returning presence, hint and
validity only. The Settings UI renders them inside the groups they belong to — the HF token under
**Hugging Face**, the GitHub token under **Builds** with the current api.github.com rate-limit
headroom beside it (§6.2) — so a user finds them where they expect to, while the transport stays the
secret-shaped one.

`restart_required` on a setting (`ui.port_desired`, `ui.bind`, `gateway.bind`, `log.level`) means the
running daemon still holds the old value; the UI shows a "Restart to apply" button wired to
`POST /api/v1/system/restart`, and the flag is cleared when the new daemon comes back. No setting
in the registry carries `unit_change_required` in v1 — that field exists so the UI can render the
"this needs the installer" affordance described in §5.4a if one ever does, and service identity, the
only such change today, is exposed there rather than as a setting.

### 3.5 llama.cpp lifecycle

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/llamacpp/active` | session | active version + binaries + build options + `manifest.json` |
| GET | `/api/v1/llamacpp/versions` | session | list with state, size, dates, `is_active`, `previous_active`, `in_use_by` |
| GET | `/api/v1/llamacpp/versions/{id}` | session | full row incl. `build_options_json`, `failing_step`, `devices_output` |
| POST | `/api/v1/llamacpp/versions` | session | `{"channel","tag","git_url","git_ref","backend","force_source","cmake_extra":[…]}` → `202`; `409 build_in_flight` |
| POST | `/api/v1/llamacpp/versions/{id}/cancel` | session | `202` |
| POST | `/api/v1/llamacpp/versions/{id}/retry` | session | `202` — resumes an `interrupted` build against warm objects (D4) |
| POST | `/api/v1/llamacpp/versions/{id}/activate` | session | `{"restart_instances":"none"\|"rolling"}` → `202` |
| DELETE | `/api/v1/llamacpp/versions/{id}` | session | `409 version_active` / `version_is_rollback_target` / `version_in_use` (D25) |
| POST | `/api/v1/llamacpp/rollback` | session | activates the `previous_active=1` row |
| GET | `/api/v1/llamacpp/versions/{id}/log` | session | `?offset=&limit=` plain text; SSE for a live tail |
| GET | `/api/v1/llamacpp/releases` | session | `?channel=stable\|nightly&refresh=1` → cached releases with rendered changelog HTML and resolved `nightly_tag` |
| GET | `/api/v1/llamacpp/plan` | session | `?channel=&tag=&backend=` → `{"acquisition":"source","backend":"cuda","reason":"no Linux CUDA prebuilt exists","estimated_minutes":9,"missing_tools":[],"cuda_arch":["89"],"free_space_ok":true}` — what would happen, before committing |

### 3.6 Hugging Face (remote)

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/hf/search` | session | `?q=&sort=&limit=&cursor=&author=` → normalized `{id,author,downloads,likes,gated,private,updated_at,tags,gguf:{architecture,context_length,total}}` |
| GET | `/api/v1/hf/model/{repo...}` | session | metadata + `gguf` field + `gated` + local-availability annotations |
| GET | `/api/v1/hf/tree/{repo...}` | session | grouped by quant with **true** `lfs.size` totals, shard groups, mmproj candidates, `local_model_id` |
| GET | `/api/v1/hf/card/{repo...}` | session | **rendered, sanitized HTML** (D35) plus the raw markdown for a "view source" toggle |
| GET | `/api/v1/hf/peek/{repo...}` | session | `?file=` → GGUF header read over HTTP Range **before** downloading: `arch,n_layer,n_head_kv,head_dim_*,n_ctx_train,n_vocab,swa_window,n_expert*,tensor_summary` |
| GET | `/api/v1/hf/token` | session | `{"present","hint","valid","user","scopes"}` — never the token |
| PUT | `/api/v1/hf/token` | session | validated via `/api/whoami-v2`, then sealed |
| DELETE | `/api/v1/hf/token` | session | `204` |
| GET | `/api/v1/github/token` | session | `{"present","hint","valid","user","scopes","rate_limit":{…}}` — never the token (§6.2) |
| PUT | `/api/v1/github/token` | session | validated via `GET https://api.github.com/user`; `422 github_token_invalid` on a 401; then sealed |
| DELETE | `/api/v1/github/token` | session | `204`; the release client reverts to anonymous |

**Why the sub-resource precedes the wildcard.** A repo id contains a `/` (`bartowski/Qwen3-8B-GGUF`),
so it needs Go's multi-segment `{repo...}` wildcard — and `net/http.ServeMux` (§3, "stdlib ServeMux,
no third-party router") requires such a wildcard to be the **final** element of the pattern. Registering
`/api/v1/hf/models/{repo...}/tree` does not 404 at request time; it **panics at registration**, taking
the daemon down at boot. The verb therefore moves in front of the wildcard —
`/api/v1/hf/tree/{repo...}` — which keeps one segment per concept, keeps the repo id unescaped and
readable in the URL, and keeps the router stdlib. The generated `api/openapi.json` and
`ui/src/api/schema.d.ts` carry these paths, so the D43 drift check and the response-conformance
middleware both police the shape. A CI test additionally constructs the real `ServeMux` from the route
registry inside a `recover()` and fails on any panic, so this class of mistake cannot return.

Gated repos return `403 hf_gated` with `{"repo","request_url"}`; the UI links out, because access
grants are browser-only on HF's side.

### 3.7 Local models and cache

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/models` | session | `?state=&kind=&q=&sort=` with `in_use_by:[instance ids]` |
| GET | `/api/v1/models/{id}` | session | row + files + parsed metadata + paired mmproj |
| GET | `/api/v1/models/{id}/delete-preview` | session | `{"files":N,"bytes":N,"blobs_shared_kept":N,"in_use_by":[…]}` (D28) |
| DELETE | `/api/v1/models/{id}` | session | `409 model_in_use` naming instances; otherwise executes the preview |
| POST | `/api/v1/models/{id}/verify` | session | `202` — re-stat and optional sha256 |
| POST | `/api/v1/models/{id}/pair-mmproj` | session | `{"mmproj_model_id"}`; sets `mmproj_auto=0` |
| GET | `/api/v1/models/{id}/metadata` | session | full GGUF KV map |
| GET/POST | `/api/v1/cache/roots` | session | list / add (validates writability, symlink support — F17 — and that the path is not under a `ProtectSystem=full` prefix: `422 root_path_protected`). A new root is **never** primary; it is scan-and-serve only |
| POST | `/api/v1/cache/roots/{id}/promote` | session | makes this root primary — the single write path shared with `PATCH /settings {hf.hub_dir}` (§7.2a); `202` + a scan job |
| DELETE | `/api/v1/cache/roots/{id}` | session | detaches; never deletes files. `409 root_is_primary` on the primary, `409 model_in_use` when any of its models is referenced by **any** `instances` row — soft-deleted ones included, because this is the one path that issues a SQL `DELETE` against `models` (§7.2a) |
| POST | `/api/v1/cache/scan` | session | `{"root_id"}` → `202` |
| GET | `/api/v1/cache/scans/{id}` | session | progress and results |
| GET | `/api/v1/cache/strays` | session | list |
| DELETE | `/api/v1/cache/strays/{id}` | session | `?delete_file=true` |
| POST | `/api/v1/cache/strays/{id}/dismiss` | session | `204` |

### 3.8 Downloads

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/downloads` | session | `?state=active\|all` with per-task progress |
| POST | `/api/v1/downloads` | session | `{"repo_id","revision","files":[…],"include_mmproj":true,"kind":"text","priority":100}` → **`202 {"job_id","subject":{"type":"download","id":…},"model_id":…}`**, the one long-action shape of §3 — the `jobs`, `downloads` and `models` rows are written in one transaction (§2.7), so the job *is* the receipt and `GET /downloads/{id}` carries the detail. An `Idempotency-Key` replay inside the window returns **the same body with `200`** (D65). `409 download_exists`; `409 insufficient_disk` with the numbers — both the standard error shape, so the D43 response-conformance middleware sees exactly one success body here |
| GET | `/api/v1/downloads/{id}` | session | detail |
| POST | `/api/v1/downloads/{id}/{pause\|resume\|retry}` | session | `202` |
| POST | `/api/v1/downloads/{id}/cancel` | session | `?keep_partial=true` (default true, so a retry resumes) |
| PATCH | `/api/v1/downloads/{id}` | session | `{"priority":10}` — queue reorder |

### 3.9 Fit calculator

`POST /api/v1/fit/estimate` — body
`{"source":{"model_id":…} | {"repo_id":…,"file":…}, "flags":{FlagSet subset}, "gpus":["uuid",…], "reserve_bytes_per_gpu":0}`.
The reserve is the caller's own headroom and is **per participating GPU** — the `reserve(g)` term
of §8.7's `placeable(n)`, charged to *every* selected device exactly like `margin` and `OH_gpu`
(§8.4), never divided among them. It is named that way, and echoed in the response beside its
reporting total, so no consumer has to infer the unit:

```jsonc
{
  "inputs": {"n_layer":36,"n_layer_swa":0,"n_head_kv":[8,…],"head_dim_k":128,"head_dim_v":128,
             "n_ctx":8192,"n_ubatch":512,"n_parallel":4,"type_k":"f16","type_v":"f16",
             "flash_attn":true,"n_expert":0,"n_expert_used":0},
  "weights_bytes": 4920000000,
  "weights_offloaded_bytes": 4920000000,
  "kv_bytes": 1207959552,
  "kv_swa_bytes": 0,
  "compute_bytes": 412000000,
  "backend_overhead_bytes": 419430400,      // OH_gpu, PER participating GPU
  "margin_bytes_per_gpu": 1073741824,       // fit.margin_mib — per GPU, like --fit-target
  "margin_bytes": 1073741824,               // × participating GPUs; a reporting total
  "reserve_bytes_per_gpu": 0,               // echoed from the request — per GPU, like margin
  "reserve_bytes": 0,                       // × participating GPUs; a reporting total
  "required_vram_bytes": 8033131776,        // Σ per_gpu[].assigned_bytes — a TOTAL, never the test
  "per_gpu": [{"uuid":"GPU-…","name":"…","free_bytes":23000000000,
               "assigned_bytes":8033131776,"ok":true}],
                                            // verdict "fits" ⟺ every per_gpu[].ok is true (§8.7)
  "spill_to_ram_bytes": 0,
  "system_ram_free_bytes": 51000000000,
  "verdict": "fits",                       // fits | partial | wont_run
  "max_n_gpu_layers": 37,
  "max_ctx_at_full_offload": 32768,
  "per_slot_ctx": 2048,
  "recommendation": {"n_gpu_layers":37,"flash_attn":true,"type_k":"q8_0","type_v":"q8_0"},
  "confidence": "calibrated",              // calibrated | modeled
  "notes": ["V-cache quantization requires flash attention on this build"]
}
```

`POST /api/v1/fit/estimate-batch` takes `{"repo_id","files":[…],"flags":{…}}` and returns one report
per quant plus `"recommended_file"` — this is what drives the quant picker.

### 3.10 Instances

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/instances` | session | config ⋈ status (an **inner** join — the status row is created with the instance, §2.8), with the four derived flags `restart_required`, `stale_version`, `inhibited` (+ `inhibit_reason`) and `draft_unverified` per §2.8. Soft-deleted instances are excluded unless `?include_deleted=true` (D68, §3.10c) |
| POST | `/api/v1/instances` | session | full body; ports auto-allocated when omitted and validated against the §2.8 port rules (`422 port_unavailable`); `422 draft_vocab_mismatch` per D34 |
| GET | `/api/v1/instances/{id}` | session | detail incl. rendered argv, the resolved `-ngl` advisory when `n_gpu_layers.mode=="auto"` (D51), and the last 5 starts with their outcomes |
| PATCH | `/api/v1/instances/{id}` | session | partial + `generation`; response includes `restart_required` |
| DELETE | `/api/v1/instances/{id}` | session | **soft delete (D68)**: stops the unit, disables it *when the daemon may manage unit files* (otherwise it completes anyway and raises `unit_still_enabled` with the manual command — §11.1a), closes the gateway listener, stamps `deleted_at`, keeps every row. It does **not** close the open `instance_starts` row; the supervisor does, from the stop this request just asked for (§3.10c). `?purge=true` is the explicit hard delete that cascades the accounting away; `?keep_tokens=true` leaves `token_instances` rows alone. See §3.10c |
| POST | `/api/v1/instances/{id}/start` | session | `202`; sets `desired_state=running` and stamps `pending_trigger='user'` |
| POST | `/api/v1/instances/{id}/stop` | session | `202`; `?drain_sec=30`; `"hint":"will_start_at_boot"` when `autostart=1` (§2.8) |
| POST | `/api/v1/instances/{id}/restart` | session | `202` |
| POST | `/api/v1/instances/{id}/reset-failed` | session | in one transaction: moves `instance_status.state` `crash-looping → stopped`, clears `reconcile_backoff_until`, stamps `restart_window_reset_at = now` so the crash-loop window starts over (D64) — the three columns the §2.8 writer table names as the API's only reach into `instance_status` — then calls systemd `ResetFailed` |
| POST | `/api/v1/instances/{id}/safe-start` | session | one-shot start with `-ngl 0 -c 2048` to isolate GPU vs. model problems (F3); **never persisted** — see §3.10b for the mechanism |
| PUT | `/api/v1/instances/{id}/autostart` | session | `{"enabled":true}` → `EnableUnitFiles`/`DisableUnitFiles` **only**; never starts or stops. `"hint":"start_now"` when enabling on a stopped instance (§2.8). `409 autostart_unavailable` when the `manage-unit-files` grant was withheld, carrying `sudo systemctl enable llamaman-instance@<name>.service`; `409 systemd_unavailable` in the F10 degraded mode (§11.1a) |
| POST | `/api/v1/instances/{id}/pin-ngl` | session | rewrites `n_gpu_layers` from `auto` to `{"mode":"count","count":N}` using the current fit estimate — an explicit config edit that bumps `generation` and `config_hash` (D51) |
| GET | `/api/v1/instances/{id}/status` | session | `instance_status` + live health |
| GET | `/api/v1/instances/{id}/usage` | session | `?from=&to=` → `instance_usage_daily` rows (both auth modes) plus the `/metrics` counters, each labeled with its source (§2.9) |
| GET | `/api/v1/instances/{id}/starts` | session | the `instance_starts` ledger: trigger, outcome, exit code, `detail_json`, argv — including preflight failures that never reached `execve` (D54) |
| GET | `/api/v1/instances/{id}/command` | session | `{"argv":[…],"env":{…},"unit":"llamaman-instance@qwen.service"}` — copyable and auditable |
| POST | `/api/v1/instances/validate` | session | dry-run a FlagSet: renders argv, checks conflicts, returns a fit estimate |
| GET | `/api/v1/instances/{id}/logs` | session | `?lines=1000&since=`; SSE for journald follow |
| GET | `/api/v1/instances/{id}/{props\|slots\|metrics}` | session | proxied from llama-server |
| GET | `/api/v1/ports/suggest` | session | `?kind=public\|internal` → next free port not in the DB and not bound |

### 3.10a Draft-model validation, when the metadata does not exist yet (D34)

`tokenizer_model` and `n_vocab` are populated only by a GGUF parse (`models.gguf_parsed_at`), and the
design deliberately supports creating an instance against a model that is still `planned` or
`downloading` — that is the whole "queue the download, configure the instance while it runs" flow, and
exit 72 exists for the case where a file later vanishes. A hard reject on NULL metadata would break
it; a silent accept would evaporate D34's guarantee. So the check is three-valued, and the state is
recorded in `instances.draft_validation`:

| both sides `gguf_parsed_at IS NOT NULL` | outcome |
|---|---|
| yes, and `tokenizer_model` + `n_vocab` match | `draft_validation='ok'`; save succeeds |
| yes, and either differs | `422 draft_vocab_mismatch` with both values in `details`; nothing is saved |
| no (either side unparsed) | `draft_validation='deferred'`; save succeeds with `201`/`200` carrying `"warnings":[{"code":"draft_vocab_unverified","message":"…will be checked when <model> finishes downloading"}]` |

A `deferred` pairing is re-checked at exactly two later moments, so it can never stay unresolved:

1. **When the metadata lands.** The models service, in the same transaction that writes
   `gguf_parsed_at`, re-runs the comparison for every non-deleted instance whose `model_id` or
   `draft_model_id` is that model. A match sets `draft_validation='ok'`; a mismatch sets
   `'mismatch'`, raises a `draft_vocab_mismatch` notification naming the instance, and surfaces the
   derived `draft_unverified` badge as an error state on the instance card.
2. **In the launcher's preflight.** §5.6 step 6 already resolves both model paths; it additionally
   refuses to start when `draft_validation='mismatch'`, exiting **65** with
   `error_code='draft_vocab_mismatch'`. A garbled-output failure at runtime is exactly the outcome
   D34 exists to prevent, and this is the last place it can be caught.

`POST /api/v1/instances/validate` returns the same three-valued result as
`{"draft_validation":"ok"|"deferred"|"mismatch"}` plus the warning, and never a 422 — it is a dry run.

### 3.10b Safe start: the transient-override channel (D61)

`POST /api/v1/instances/{id}/safe-start` is F3's primary recovery from a crash loop, and it has to
reach a unit that receives nothing but `%i` (§5.1). There is no argv channel and no environment
channel, writing the override into `flags_json` would persist it and move `config_hash`, and
`StartTransientUnit` is banned outright by D3. The mechanism is therefore a second hand-off column
beside `pending_trigger`, consumed in the same transaction:

1. The handler, in one transaction: sets `desired_state='running'`, stamps
   `pending_trigger='safe_start'` and
   `pending_override_json = {"n_gpu_layers":{"mode":"none"},"ctx_size":2048,"parallel":1}`, clears
   `instance_status.reconcile_backoff_until`, moves `instance_status.state` `crash-looping → stopped`
   if it was latched, stamps `restart_window_reset_at = now`, emits an `events` row, and returns
   `202`. Those three `instance_status` columns are precisely the narrow exception list of §2.8 —
   they have to land synchronously with the request, because a Safe start whose backoff clears on a
   later supervisor pass is a button that appears to do nothing.
2. `instance-exec` consumes **both** columns in its step-3 transaction (§5.6) and clears both. The
   override is a shallow patch over the parsed `FlagSet`: present keys replace, absent keys are
   untouched, `extra_flags` is dropped for the run.
3. Rendering proceeds normally. The launcher records `override_json` and
   `effective_config_hash = sha256(overridden argv …)` on the start row — which differs from
   `instances.config_hash`, so `restart_required` is true for as long as the safe start is the
   running configuration, and the instance page says "running in safe mode — restart to apply the
   saved configuration" rather than pretending the saved config is live.
4. Because the columns are cleared on consumption, the *next* start — from any trigger — is the saved
   configuration again. The override survives neither a crash nor a reboot, which is what "never
   persisted" has to mean for a system whose supervisor may restart the unit on its own.

`GET /instances/{id}/starts` shows safe starts as their own trigger with the patch inline, so "it
only works with `-ngl 0`" is a fact in the history rather than something a user has to remember. That
history — not a derived badge — is where a past safe start stays visible; `restart_required` reads the
**open** row only, so it clears by itself on the next ordinary start (§2.8).

### 3.10c Deleting an instance: soft by default, hard on request (D68)

`instances.deleted_at` exists and at least eight rules elsewhere are written over "non-deleted"
instances — the port walk's exclusion set (§2.8), the D53 autostart coupling, the cache-root detach
guard and the model delete guard (§7.2a), the gateway's listener set (§9.1), `RecomputeConfigHash`'s
target set (§2.8), the supervisor's reconcile set (§5.8) and the draft re-validation sweep (§3.10a).
Those only mean something under soft deletion, so soft deletion is the rule and the wording of all of
them is literal.

**`DELETE /api/v1/instances/{id}`** — one transaction plus the systemd calls, `202`:

1. `desired_state='stopped'`, then `StopUnit`, `DisableUnitFiles` (so it cannot come back at the next
   boot) and `ResetFailed`. **All three are best-effort and individually gated**, because two
   supported installs cannot make them: on a host installed with `--no-autostart-grant`
   (`autostart_control: false`) or one where polkit denied the grant at boot (F9), `DisableUnitFiles`
   is not attempted at all; under `systemd_control='unavailable'` (F10) none of the three is. A
   skipped or denied call never fails the delete — it raises a single `unit_still_enabled`
   notification naming the unit and the exact
   `sudo systemctl disable llamaman-instance@<name>.service`, and the response carries the same
   command in `"hints"`. The safety net for the window in between is already in place and is why this
   can be best-effort at all: an enabled unit for a deleted instance starts `instance-exec`, which
   loads the row, finds `deleted_at` set and exits **64** without launching anything (§5.6 step 2).
2. Close the gateway listener for that instance and remove it from the listener map (§9.1) — a
   soft-deleted instance owns no port, which is what makes the port immediately reusable.
3. Stamp `deleted_at = now`. `?keep_tokens=true` leaves `token_instances` rows in place; the default
   removes them, since a scope entry for an instance nobody can reach is noise.

**The delete handler deliberately does not close the `instance_starts` row.** An earlier reading had
it do so, which would have made an API handler the third writer of a column §5.6's writer table calls
single-shot with exactly two writers — and would have raced the supervisor's own
"an explicit stop was requested → `stopped`" rule for the same row, since step 1 has already issued
the `StopUnit` that triggers it. The stop the handler just requested is what closes the row, written
by the one actor allowed to write it. So that the deletion cannot orphan an open row, the
supervisor's reconcile set is "every instance with `deleted_at IS NULL`, **plus every instance with
an open `instance_starts` row**" — the second term is one lookup on `idx_instance_starts_open`, and
it exists for precisely this case: a soft-deleted instance is reconciled exactly until its last run
is ledgered, and never again. Boot reconciliation (§5.8 step 3) covers a daemon that died in the gap.

`name`, `public_port` and `internal_port` are free the instant `deleted_at` is stamped, because all
three unique indexes are partial (`WHERE deleted_at IS NULL`, §2.8). Creating a new instance with the
old name reuses the systemd unit name `llamaman-instance@<name>.service`, which is safe: the unit is
static, content-free (§5.1) and was stopped and disabled in step 1.

**What a soft-deleted instance still is**: rows in `instance_starts`, `instance_usage_daily`,
`token_usage_daily` and `gateway_denials_daily`, all reachable from `GET /instances/{id}/usage` and
`/starts` with `?include_deleted=true`, and counted in account-wide totals. It is **not** in
`GET /instances` (add `?include_deleted=true`), not reconciled except for the single pass that closes
its last open ledger row (above), not started at boot, not a port holder, and not a `model_in_use`
blocker for deleting the model it referenced — deleting a model never issues a SQL `DELETE` (§7.2),
so the retained `model_id` costs nothing there. It **is** a blocker for detaching the cache root that
holds that model (`409 model_in_use`, §7.2a), because that operation *does* cascade `models` rows
away and would otherwise hit `instances.model_id`'s `ON DELETE RESTRICT` as a raw foreign-key error
inside the transaction. The remedy the 409 names is `?purge=true` on the listed instances.

**`DELETE /api/v1/instances/{id}?purge=true`** is the hard delete: the row is removed and
`instance_status`, `instance_starts`, `instance_usage_daily`, `token_usage_daily`,
`gateway_denials_daily` and `token_instances` cascade with it. The UI puts it behind a second
confirmation that names the row counts and byte totals about to be discarded, because that history is
the one thing in this system that cannot be recomputed. The nightly `maintenance` job never purges on
its own — a soft-deleted instance is retained indefinitely, exactly like the benchmarks (§2.11).

### 3.11 Presets

`GET/POST /api/v1/presets`, `GET/PATCH/DELETE /api/v1/presets/{id}`,
`POST /api/v1/presets/from-instance/{id}`, and
`POST /api/v1/presets/{id}/apply` `{"instance_ids":[…],"overwrite":["ctx_size","flash_attn"]}` →
a per-instance diff plus `restart_required` flags.

### 3.12 Tokens

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/tokens` | session | never returns secrets; `prefix`, counts, last used |
| POST | `/api/v1/tokens` | session | `201 {…,"secret":"lm_…"}` — **the only response that ever contains the secret** |
| GET | `/api/v1/tokens/{id}` | session | detail + per-instance usage summary |
| PATCH | `/api/v1/tokens/{id}` | session | `{"name","state":"disabled","instance_ids":[…],"rate_limit_rpm"}` |
| DELETE | `/api/v1/tokens/{id}` | session | revoke (soft; `state='revoked'`, terminal) |
| GET | `/api/v1/tokens/{id}/usage` | session | `?from=&to=&group=day\|instance` |
| GET | `/api/v1/gateway/denials` | session | denial counters per instance and reason |

### 3.13 Benchmarks

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/bench/runs` | session | list with summary metrics |
| POST | `/api/v1/bench/runs` | session | `{"name","model_id","repetitions":3,"sweep":{"n_gpu_layers":[0,20,"all"],"n_batch":[512,2048],"flash_attn":[true,false],"type_k":["f16","q8_0"],"tests":[{"pp":512},{"tg":128},{"pp":512,"tg":128,"depth":4096}]},"on_conflict":"stop_and_restore"\|"abort"}` → `201` draft or `202` queued; `409 bench_gpu_conflict` naming the instances |
| GET | `/api/v1/bench/preflight` | session | `?model_id=` → GPU conflicts, free VRAM, point count, estimated duration |
| POST | `/api/v1/bench/runs/{id}/start` | session | draft → queued |
| POST | `/api/v1/bench/runs/{id}/cancel` | session | `202`; restores stopped instances |
| GET | `/api/v1/bench/runs/{id}` | session | run + points + progress |
| PATCH/DELETE | `/api/v1/bench/runs/{id}` | session | rename/annotate; delete |
| GET | `/api/v1/bench/runs/{id}/results` | session | flattened rows for the table |
| GET | `/api/v1/bench/runs/{id}/export` | session | `?format=json\|csv\|md` with `Content-Disposition` |
| POST | `/api/v1/bench/compare` | session | `{"run_ids":[…],"x":"n_gpu_layers","y":"avg_ts","series":"test_kind","filters":{…}}` → chart-ready series |
| GET | `/api/v1/bench/series` | session | `?model_id=&test=tg&metric=avg_ts&group=llamacpp_tag` → history across llama.cpp versions |

### 3.14 Self-update, jobs, events

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/update/status` | session | current version, latest known, `update_available`, last check, the in-flight row, and the one **self-update fact** the §12.3 gate last computed: `{"pending":null}` on a settled host, or `{"pending":{"self_update_id":…,"from_version":…,"target_version":…,"staged_at":…,"actor_active":bool}}` — `actor_active` being `systemctl is-active llamaman-selfupdate.service`, which is what the gate itself defers on (D91), so the UI renders "a swap is in flight" and the F24 card from the same fact the daemon acted on |
| POST | `/api/v1/update/check` | session | `202` |
| GET | `/api/v1/update/releases` | session | changelog list, rendered server-side |
| POST | `/api/v1/update/apply` | session | `{"tag":"v1.2.0"}` → `202`. Any tag `/update/releases` lists, **newer or older** — a downgrade is this same pipeline, and §12.1 step 3's probe requires the extracted binary to print exactly the requested tag (D90). Runs the §12.3 resolver first, then the guard of **exactly four clauses**, the same four §12.1 step 1 enumerates, all of them evaluated inside the one `BEGIN IMMEDIATE` transaction that inserts the row and its job (D97): `409 job_in_flight` while a build (D4) **or any `self_update` job** is live — `interrupted` counts (§2.3); `409 selfupdate_unavailable` when `systemd_control='unavailable'` (§11.1a); `409 selfupdate_unsupported` when **the swap actor cannot be summoned** — in system scope, `llamaman-selfupdate.service` absent or masked (§5.4a), which is both the pre-v1.0.0 host that never had one, carrying the `install.sh` one-liner, and the host whose unit was deleted or masked, carrying the `install-units` line; not applicable in user scope, where the daemon performs the swap in process (§5.2a); and `409 revert_unavailable` when the installed `llamaman.service` does not carry `OnFailure=llamaman-update-verify.service`, or `llamaman-update-verify.service` is absent or masked — **no update is ever staged without a working revert** (D88). The last two are read off the installed units' own directives, never off a template hash (D95). For a target older than the running version the response and the dialog additionally carry the schema warning of §12.4 **and its five-command procedure** (D94). The UI then polls `/api/v1/meta` until the new version answers |
| GET | `/api/v1/jobs` | session | `?state=active` |
| GET | `/api/v1/jobs/{id}` | session | state, progress, error |
| POST | `/api/v1/jobs/{id}/cancel` | session | `202`. Two kinds carry a cut-off rather than a blanket accept, and both are the same shape: `llamacpp_activate` is cancelable only before its step-3 transaction commits (§2.3a), and `self_update` only before the `staged` commit — at or after it, `409 selfupdate_not_cancelable`, because from that instant the marker is on disk and the swap is a unit systemd owns, and nothing downstream reads `cancel_requested` (D96, §12.1 step 5). A cancel that *is* accepted moves the `self_updates` row and its job to `canceled` in one transaction and clears `update/` scratch |
| GET | `/api/v1/events/log` | session | `?category=&subject_id=&before=&limit=` |
| GET | `/api/v1/events` | session | **SSE**; `?topics=instances,downloads,llamacpp,gpu,bench,jobs,events,notifications`; `Last-Event-ID` replays from `events` |

SSE frames are `event: <topic>` / `id: <ulid>` / `data: {"type":"instance.status","id":"…","patch":{…}}`
— **patches keyed by entity id**, so the client merges into its query cache without refetching. A
`retry: 3000` directive and a 20 s `:keepalive` comment are sent.

### 3.15 Gateway ports (not under `/api/v1`)

Each instance owns a public listener on `instances.public_port`:

| Path | Auth | Behavior |
|---|---|---|
| `GET /health` | public | Gateway-owned: `200 {"status":"ok"}` when the upstream is ready, `503 {"status":"loading model"}` while loading, `503 {"status":"stopped"}` otherwise — matching llama-server semantics |
| `GET /llamaman/info` | public | `{"instance","model","auth_required"}` — enough for a client to self-configure, nothing sensitive |
| everything else | token, or none when `auth_mode='none'` | transparent, streaming-safe reverse proxy to `127.0.0.1:<internal_port>` |

Credentials are accepted as `Authorization: Bearer lm_…`, `X-API-Key: lm_…`, or `?api_key=`.
Failures are `401 {"error":{"code":"invalid_api_key","message":"…"}}` in the OpenAI-compatible shape
so SDKs surface a sensible message.

---

## 4. Frontend architecture

| Choice | Rationale |
|---|---|
| **Vite 8 + React 19 + TypeScript 7 (strict)** | SPEC §5.4 assumption 4, superseded by the owner's latest-stable directive (D45). Output is plain static assets for `go:embed`. |
| **TanStack Router (code-based routes)** | Type-safe params *and* search params: filters, sort and comparison selections live in the URL, which is what a technical tool needs (shareable links, working back button). |
| **TanStack Query** for all server state | Every screen is a projection of DB rows; Query gives caching, background refresh, and a cache that SSE frames can *patch* instead of polling. |
| **Zustand** for the sliver of client state | Wizard scratch state, theme, unsaved instance-form drafts. Deliberately tiny. |
| **Tailwind CSS v4 + Radix UI primitives** | Dark-first professional theme without a heavy component framework; Radix gives accessible dialogs/menus/tabs with no styling opinion. All assets self-hosted, no CDN (SPEC §5.4). |
| **react-hook-form + zod** | The instance form has ~40 interdependent optional fields; zod resolvers give per-field validation identical to the server's. |
| **uPlot** for charts (D44) | Canvas rendering at ~45 KB handles the thousands of points a 512-point sweep produces. Comparison bars are hand-rolled SVG, so there is exactly one chart dependency. |
| **No client-side markdown renderer** | Model cards and changelogs arrive as sanitized HTML from the server (D35), dropping `react-markdown` + `rehype-sanitize` and moving attacker-controlled content behind one audited sanitizer. |
| **`openapi-typescript` in CI, not at runtime** | Go emits `openapi.json` from the route registry; CI regenerates `ui/src/api/schema.d.ts` and fails on drift, so the types can never lie about the API. |
| **No SSR, no route-level loaders** | A single-user admin app behind a session cookie; a plain SPA with a fallback handler is simpler and embeds cleanly. |

**Build integration.** `ui/` builds to `ui/dist`; `make ui` runs `npm ci && npm run build` and syncs
into `internal/web/dist`, which `internal/web` embeds with `//go:embed all:dist`. Assets are
content-hashed and served `Cache-Control: public, max-age=31536000, immutable`; `index.html` is
`no-store`. Unknown paths that accept HTML fall back to `index.html`; unknown `/api/*` paths return
a JSON 404. A checked-in stub `internal/web/dist/index.html` (telling the developer to run
`make ui`) keeps `go build ./...` working on a clean checkout.

**Data layer.** One `useEvents()` hook opens the SSE stream and maps each frame to
`queryClient.setQueryData`; if the stream drops twice it falls back to interval refetch and shows a
"live updates unavailable" chip. Mutations are optimistic only for cheap toggles (token
enable/disable, autostart); anything job-backed renders the job's progress rather than a spinner.

**Screens.**

1. `/login` — password, lockout countdown.
2. `/setup/*` — the six wizard steps (§11), one route each.
3. `/` **Dashboard** — instance cards (state, port, model, slots busy, tokens/sec when metrics are
   on), GPU VRAM meters, active-jobs strip, disk usage, notifications, recent events, update banner.
4. `/instances` — table: name, model, state, port, autostart, and the four derived badges
   (restart-required, stale-version, inhibited, draft-unverified); the autostart toggle warns when it would leave a
   running instance out of the next boot, or a stopped one in it (§2.8).
5. `/instances/new`, `/instances/:id/edit` — three-pane form (**Model & context** / **Performance** /
   **Advanced**) with a live **fit panel** that re-estimates on every change and a rendered-argv
   preview underneath.
6. `/instances/:id` — status header, live journald log pane, slots table, metrics sparklines, token
   access list, start/stop/restart/reset-failed/safe-start, start history, and the remediation card
   for the last exit code (§17).
7. `/models` — local library grouped by repo: quant chips, size, in-use badges, missing/corrupt
   states, delete with the free-space preview.
8. `/models/browse` — HF search with the GGUF filter, gated badge, sort, infinite list.
9. `/models/browse/:repo` — rendered card, quant table with **true sizes** and a fit verdict per
   quant per GPU, shard groups collapsed, mmproj auto-paired, download button.
10. `/models/:id` — files, blobs, metadata table, paired mmproj, verify, delete.
11. `/downloads` — queue with per-file progress, speed/ETA, pause/resume/cancel/reorder.
12. `/llamacpp` — active version panel, version list with rollback, install dialog
    (channel/tag/custom git) with the plan preview, and a virtualized build-log viewer
    (ANSI-stripped, auto-scroll, failing step highlighted, jump-to-first-error).
13. `/bench`, `/bench/new`, `/bench/:id`, `/bench/compare` — history table and charts; a sweep
    builder with a live "N points ≈ M minutes" estimate and conflict preflight; per-point results
    with export; multi-run comparison with x/y/series pickers.
14. `/tokens` — list, create dialog (secret shown once behind a copy + "I saved it" gate), scope
    editor, per-token usage sparkline.
15. `/settings` — grouped forms mirroring the settings registry: General, Network & Ports, Hugging
    Face, Storage, Builds, Benchmarks, Security, Updates, Danger zone.
16. `/system` — toolchain report cards with fix guidance, GPU details, disk, journal viewer,
    "Download diagnostics bundle".
17. `/events` — filterable audit log.

Baseline quality: keyboard-navigable tables, focus-visible rings, `prefers-reduced-motion`
respected, and every color drawn from a token palette meeting WCAG AA in both themes.

---

## 5. systemd integration

### 5.1 The central decision: a content-free template unit

Instance configuration lives in SQLite only. The template unit encodes no model path, no port and no
flag; it execs the llamaman binary as a launcher (`llamaman instance-exec %i`), which reads the row
and `execve`s `llama-server`. Consequences:

- Editing an instance is a DB write — no unit regeneration, no `daemon-reload`, no drift between a
  unit file and a row, and no second on-disk state store to garbage-collect.
- The daemon needs **no privileged filesystem writes at runtime**; unit files are installed once by
  `llamaman install-units`. §5.4a states which changes can therefore be made from the UI (all of
  them except service identity) and how the unit stopped depending on the mutable cache path at all.
- After `syscall.Exec` the process image *is* `llama-server`, so `MainPID`, `MemoryCurrent`, signals,
  `/proc/<pid>/exe` and journald metadata all refer to the real server. No supervisor sits in the
  middle.

### 5.2 Scope, identity and authorization

**Default (D1): system scope.** `install.sh` writes `llamaman.service`,
`llamaman-instance@.service`, `llamaman-instances.target`, `llamaman-selfupdate.service`,
`llamaman-update-verify.service` into `/etc/systemd/system/`, exactly as SPEC §3.7
states. The daemon runs as the installing user (SPEC §5.1b) so it can read and write that user's HF
cache with correct ownership, and buys unit management with a unit-name-scoped polkit rule:

```javascript
// /etc/polkit-1/rules.d/49-llamaman.rules   (polkit >= 0.106; written by `llamaman install-units`)
// One branch per action id. The catch-all `unit === undefined` escape hatch that earlier drafts
// used is WRONG: manage-unit-files also carries no `unit` detail, so a single undefined check
// silently granted unit-file management for every unit on the host.
polkit.addRule(function(action, subject) {
    if (subject.user !== "@IDENTITY@") { return polkit.Result.NOT_HANDLED; }

    // (a) reload-daemon: no `unit` detail exists for this action, and none is needed. Re-reading
    //     /etc/systemd/system confers nothing on a daemon that cannot write it.
    if (action.id === "org.freedesktop.systemd1.reload-daemon") {
        return polkit.Result.YES;
    }

    // (b) manage-units: Start/Stop/Restart/ResetFailed. This action DOES carry `unit`, so the
    //     grant is name-scoped to the statically installed set. Nothing else, ever.
    if (action.id === "org.freedesktop.systemd1.manage-units") {
        var unit = action.lookup("unit");
        if (unit === undefined) { return polkit.Result.NOT_HANDLED; }   // fail closed
        //     `llamaman-update-verify.service` is DELIBERATELY absent, and so is any timer for it:
        //     the judge is started by `OnFailure=` on llamaman.service, i.e. by the service manager
        //     itself, and never by the daemon or by anything the daemon can reach (D88). Nothing in
        //     this design needs to start it on demand, so nothing here grants that.
        if (unit === "llamaman.service" ||
            unit === "llamaman-instances.target" ||
            unit === "llamaman-selfupdate.service" ||
            /^llamaman-instance@[a-z0-9][a-z0-9-]{0,31}\.service$/.test(unit)) {
            return polkit.Result.YES;
        }
        return polkit.Result.NOT_HANDLED;
    }

    // (c) manage-unit-files: Enable/Disable/Link/Mask/Preset. systemd authorizes this action
    //     BUS-WIDE with no `unit` detail — polkit cannot scope it, and no rule can pretend to.
    //     Granted only when install-units was run WITHOUT --no-autostart-grant; the trade-off is
    //     stated in full below.
    if (action.id === "org.freedesktop.systemd1.manage-unit-files") {
        return @UNIT_FILES_GRANT@;          // polkit.Result.YES  or  polkit.Result.NOT_HANDLED
    }

    return polkit.Result.NOT_HANDLED;
});
```

Four properties of this rule are load-bearing:

- **`reload-daemon` is granted, in its own branch.** `EnableUnitFiles` is useless without it, and it
  is the action every proposal except one forgot.
- **`manage-units` is name-scoped and fails closed.** An `undefined` unit on this action is denied
  rather than allowed. The daemon never calls `StartTransientUnit` and the interface does not expose
  it (D3): polkit sees a unit *name*, not its properties, so any transient-unit grant would let a
  compromised daemon start a unit with `User=root`. Removing the capability is the only real
  mitigation.
- **`manage-unit-files` cannot be scoped, so the design says exactly what it grants.** systemd
  authorizes `org.freedesktop.systemd1.manage-unit-files` for the whole bus with no `unit` detail;
  `action.lookup("unit")` is `undefined` there, which is why the old single-`undefined` escape hatch
  was a hole rather than a convenience. Llama Man needs precisely one verb from it —
  `EnableUnitFiles`/`DisableUnitFiles` on `llamaman-instance@<name>.service`, which is what
  `instances.autostart` *is*. The honest accounting:
  - **What is granted**: the service identity may enable, disable, link, mask, unmask or preset any
    unit file on the host.
  - **What is *not* granted**: starting any of them. Branch (b) remains name-scoped, so a compromised
    daemon cannot `StartUnit` anything outside the llamaman set. The residual escalation is therefore
    *deferred* — enabling or linking a hostile unit gains root at the **next boot**, not immediately —
    which is the same shape as, and no worse than, the `LinkUnitFiles`-into-a-writable-path route.
  - **Compensating controls**: the boot unit-drift check (§5.4a) is extended to enumerate
    `/etc/systemd/system/*.wants/` and `*.requires/` entries and every masked unit, comparing them
    against the set `install-units` wrote, and raises F16 on anything unexpected; `GET /system/units`
    shows the same diff; and `llamaman diagnostics` includes it.
  - **The opt-out**: `install-units --no-autostart-grant` renders branch (c) as `NOT_HANDLED`.
    `runtime_info.polkit_unit_files` becomes 0, `GET /system/capabilities` reports
    `autostart_control: false`, `PUT /instances/{id}/autostart` returns `409 autostart_unavailable`
    carrying `sudo systemctl enable llamaman-instance@<name>.service`, and the instances table renders
    the autostart column read-only with that command in a tooltip. Everything else — create, start,
    stop, restart, self-update — keeps working. Hosts that will not accept the trade also have D2's
    user-scope topology, where there is no polkit and no root to escalate to at all.
- Legacy hosts (polkit < 0.106) get an equivalent
  `/etc/polkit-1/localauthority/50-local.d/49-llamaman.pkla`. `.pkla` cannot express per-action
  branching at all — it keys on action ids only — so it is written as three stanzas, one per action
  id, and the `manage-units` stanza is necessarily broader there (all of `manage-units`, not just our
  unit names); `install-units` prints that difference and records it in `runtime_info.polkit_detail`.
  It writes whichever format the detected polkit version supports, and writes both when detection is
  ambiguous.

At boot, **in system scope**, the daemon calls
`org.freedesktop.PolicyKit1.Authority.CheckAuthorization` for its own process **twice**:
against `manage-units` with `unit=llamaman-instances.target`, and against
`manage-unit-files`. The first negative answer records `runtime_info.polkit_ok=0`, raises a blocking
notification with the exact `sudo llamaman install-units --repair-polkit` remediation, and degrades
the control plane to read-only — rather than failing lazily on the user's first Start click (F9). The
second answer is recorded in `runtime_info.polkit_unit_files` and surfaces as
`autostart_control` in `GET /system/capabilities`, so a host installed with
`--no-autostart-grant` shows autostart as unavailable from the first page load instead of erroring on
the first toggle. In the D2 user-scope topology **neither call is made** — there is no polkit
rule there at all (§5.2a) — so both columns stay `NULL`, meaning "not applicable" rather than
"denied", and `GET /system/capabilities` reports `instance_control` and `autostart_control`
true from the scope alone. §11.1a tabulates every combination, that one included.

### 5.2a Alternate (D2): `install.sh --user-units`

For hosts with no usable polkit authority, the same units are rendered into `/etc/systemd/user/`
(root-writable, and the correct location for admin-installed user units) and run inside the user's
own `systemd --user` manager, with `loginctl enable-linger <user>` so they start at boot without a
login. There is **no polkit rule at all** in this topology: a user manager authorizes its owner
unconditionally.

**Four** things genuinely differ, and each is stated rather than assumed identical:

**(1) Ordering — for every unit, not only the daemon.** A user unit runs *inside*
`user@<uid>.service`, so it can neither order itself against that unit nor `Requires=` it — `%U` in a
user unit expands to the uid, and the dependency would be circular. It also cannot name **system**
targets: `network-online.target` does not exist in a user manager, and `After=`/`Wants=` on a
non-existent unit is either inert (ordering) or a hard start failure (`Wants=` pulls in a unit the
manager cannot find). So the rewrite applies to all three unit templates, and `install-units` performs
it centrally when `--user-units` is given:

| unit | system scope | user scope |
|---|---|---|
| `llamaman.service` | `After=network-online.target dbus.service` / `Wants=network-online.target` / `WantedBy=multi-user.target` | `After=basic.target dbus.socket` / `Wants=dbus.socket` / `WantedBy=default.target` |
| `llamaman-instance@.service` | `After=network-online.target` / `Wants=network-online.target` / `After=llamaman.service` / `PartOf=llamaman-instances.target` | `After=basic.target` / **no `Wants=`** / `After=llamaman.service` / `PartOf=llamaman-instances.target` (the last two are user units and resolve normally) |
| `llamaman-instances.target` | `After=network-online.target` / `WantedBy=multi-user.target` | `After=basic.target` / `WantedBy=default.target` |

Dropping `network-online.target` from the instance template costs nothing that matters:
`llama-server` binds `127.0.0.1` only (§5.7), so it has never needed a routable address to start, and
`enable-linger` is what guarantees `/run/user/<uid>` and the user bus exist at boot. There is nothing
to race. A CI assertion in the `systemd-user` job greps every rendered user unit for
`network-online.target` and `user@` and fails on either.

**(2) Self-update has no root actor.** D12's three-actor design is a *system-scope* design: it exists
because an unprivileged daemon cannot overwrite a root-owned binary on root's `PATH`. In user scope
that premise is gone, and with it D15's rationale — the binary is **not** on root's `PATH`, so it
cannot turn `sudo llamaman` into an escalation. Therefore:

- `--user-units` installs the binary to `~<user>/.local/bin/llamaman`, owned by the service identity,
  0755, and `install-units` renders `ExecStart=` with that prefix (§5.4, D15). The retained previous
  binary sits beside it at `~<user>/.local/bin/llamaman.prev`, same owner and mode, exactly as in
  system scope (D89) — the point of that location is a same-directory atomic rename, which holds in
  both topologies, and its secondary point (a directory the service identity cannot write) is simply
  vacuous here, where one uid owns everything.
- **The forward swap is performed by the daemon itself**, in process, and it is §12.2's sequence with
  the privilege boundary removed: stage and verify per §12.1 steps 1–6 (sha256, ed25519, the version
  probe, the D14 snapshot, `update/pending`), then copy `<prefix>/llamaman` to `llamaman.prev.tmp` and
  rename it to `llamaman.prev`, extract the new binary from the same verified tarball to
  `llamaman.new.tmp`, `rename` it over `<prefix>/llamaman`, and restart through §9.4 — **exiting
  normally here rather than waiting (D79)**, because by the time it exits the binary on disk is
  already the new one and `Restart=always` starting it is the intended outcome.
  `llamaman-selfupdate.service` is **not installed** in user scope, and `selfupdate-apply` refuses to
  run there. The signature re-verification that exists in system scope only to distrust the stager is
  performed anyway, once, by the one process doing both jobs.
- **One ownership predicate, in both scopes, and this is where it is stated.** Before the judge
  installs `<prefix>/llamaman.prev` it requires that file to be **owned by the same uid as
  `<prefix>/llamaman`** and not group- or world-writable. In system scope that uid is 0 and the
  directory is root-only, which is the whole reason D89 moved the retained binary out of the
  service-identity-owned `update/`; in user scope it is the service identity, which owns the prefix,
  the state directory and the daemon alike. Written as "uid 0" the rule would be unsatisfiable here
  and every revert would refuse permanently while `GET /system/capabilities` advertised
  `self_update: true`; written as "the same uid as the binary it replaces" it is the same check
  against the correct authority in both topologies, and it needs no digest, no second file and no
  cross-binary contract to evaluate.
- **The revert is unchanged in shape.** A *user*-scope `llamaman-update-verify.service` runs
  **`~<user>/.local/bin/llamaman.prev update-verify --scope user`**, gated by the same two
  `ConditionPathExists=` lines and started by the same `OnFailure=llamaman-update-verify.service` on
  the user manager's `llamaman.service` (D88). `systemctl --user is-active`, `reset-failed` and
  `start` need no privilege in this topology, which is why `@SYSTEMCTL@` renders with `--user` and
  addresses the user's own manager. The judge's body — two `fstat`s, one `is-active`, one `rename` —
  is byte-for-byte the same code path.
- `runtime_info.systemd_scope='user'` selects the whole path, and `GET /system/capabilities` reports
  `self_update: true` in both topologies.
- **There is no on-demand rollback endpoint in either topology** (D87), so the question of which
  already-installed unit would perform one does not arise. Installing an older release here is the
  same `POST /update/apply` with an older tag, and the same schema consequence and the same
  five-command completion apply verbatim (§12.4, D94) — with `systemctl --user stop|reset-failed|start`
  and `install.sh --user-units --version <older> --no-start` in place of the system-scope commands,
  which is what the F24 card renders when `runtime_info.systemd_scope='user'`. The `reset-failed` step
  is not system-scope-specific: a user manager rate-limits starts by the same rule.
- **D92 and D93 are topology-independent**, because both are the daemon's own code: it unlinks
  `update/pending` before its first migration in either scope, and it clears its unit's start-limit
  counter through the same `Controller` interface — against the user manager here, where the call
  needs no polkit at all.

**(4) The state directory.** `StateDirectory=llamaman` is honored by *both* managers, but they
resolve it differently: `/var/lib/llamaman` for a system service, `$XDG_STATE_HOME/llamaman` —
normally `~/.local/state/llamaman` — for a user service, and `$STATE_DIRECTORY` is set accordingly.
`/var/lib/llamaman` is therefore **not** a constant in user scope, and every unit line that named it
literally (`WorkingDirectory=`, `ReadWritePaths=`, the judge's `ExecStart=`) was wrong there. The
resolution rule is D72 and it is the same one this design already uses for `$NOTIFY_SOCKET`: the
service manager sets the variable, so reading it is not a configuration file and does not breach SPEC
§3.9.

- **Units** use the `%S/llamaman` specifier wherever the state directory appears, so one template
  renders correctly in both scopes.
- **The daemon** (§11.1 step 1) takes `$STATE_DIRECTORY` when present, and otherwise falls back to
  `/var/lib/llamaman` in system scope and `$XDG_STATE_HOME/llamaman` (then `$HOME/.local/state/llamaman`)
  in user scope. The resolved value is recorded in `runtime_info.state_dir`, which is what
  `llamaman status`, `doctor` and `diagnostics` print and what every other path in §6.1 is relative
  to.
- **`install.sh --user-units`** creates that directory and its `{versions,src,build,logs,db-backups,update,tmp}`
  children under the user's own state root rather than under `/var/lib`, and `--uninstall` prints
  that path.

**(3) Root enabling a unit for another user.** `install.sh` runs as root and must enable a *user*
unit belonging to `<user>`, which needs that user's manager — root's own `systemctl --user` would
talk to root's manager and silently do nothing useful. The sequence is explicit:

```sh
loginctl enable-linger "$LM_USER"                       # creates /run/user/<uid>, starts the manager
uid=$(id -u "$LM_USER")
runuser -u "$LM_USER" -- env \
  XDG_RUNTIME_DIR="/run/user/$uid" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  systemctl --user daemon-reload
runuser -u "$LM_USER" -- env XDG_RUNTIME_DIR="/run/user/$uid" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  systemctl --user enable --now llamaman-instances.target llamaman.service
```

`enable-linger` is run **first** and the script polls for `/run/user/<uid>/bus` for up to 10 s before
the `runuser` calls, because the manager start is asynchronous. `--uninstall` performs the mirror
sequence. If `runuser` is absent, `setpriv --reuid`/`su -s /bin/sh` are tried in that order and the
failure message names the missing tool.

**CI (D47) exercises this topology end to end, not only the D-Bus verbs.** The `systemd-user` job
already runs a real user manager under `dbus-run-session`; it additionally runs the user-scope
self-update path — stage → in-process swap → confirm, and stage → a new binary that exits non-zero →
`llamaman.service` reaching `failed` → `OnFailure=` → the judge's one rename → the old binary back and
serving — and asserts that `llamaman-selfupdate.service` is absent and `selfupdate-apply` refuses.
That is the gap that let the three inconsistencies above survive.

`--dedicated-user` (a system `llamaman` account whose HF cache is `/var/lib/llamaman/hf-cache/hub` —
§7.2) is orthogonal to both topologies and remains the locked-down option SPEC §5.1b describes.

### 5.3 Control channel

`internal/systemd.Controller` is implemented twice — `dbusController` (primary) and `execController`
(`systemctl`, degraded) — and chosen at boot by probe, with the winner reported in
`GET /system/info` so the UI can say that status updates are polled rather than pushed.

```go
type Controller interface {
    Start(ctx context.Context, unit string) (JobResult, error)   // blocks on JobRemoved
    Stop(ctx context.Context, unit string) (JobResult, error)
    Restart(ctx context.Context, unit string) (JobResult, error)

    // Fire-and-forget variants: enqueue the job, return its object path, never wait for
    // JobRemoved. Mandatory for the two calls whose completion requires this process to die.
    StartNoWait(ctx context.Context, unit string) (JobPath, error)
    RestartNoWait(ctx context.Context, unit string) (JobPath, error)

    Enable(ctx context.Context, units []string) error            // + Reload()
    Disable(ctx context.Context, units []string) error
    ResetFailed(ctx context.Context, unit string) error
    Props(ctx context.Context, unit string) (UnitProps, error)   // ActiveState, SubState, MainPID,
                                                                 // ExecMainStatus, Result, NRestarts,
                                                                 // MemoryCurrent, ExecMainExitTimestamp
    SubscribeSubState(ctx context.Context, pattern string) (<-chan SubStateEvent, error)
}
```

**Why the no-wait variants exist, and where they are mandatory.** `Start`/`Stop`/`Restart` block on
`JobRemoved`, which is correct for every instance unit and wrong for exactly two calls, both of which
would otherwise deadlock a request goroutine against its own death:

| call | why blocking hangs |
|---|---|
| `StartUnit llamaman-selfupdate.service` (§12.1 step 7) | a `Type=oneshot` start job does not complete until its `ExecStart` exits, and that `ExecStart` ends by restarting `llamaman.service` — i.e. by SIGTERMing the process waiting on the job |
| `RestartUnit llamaman.service` (`POST /system/restart`) | completion requires this process to exit |

Both therefore call the `NoWait` variant, and both run through the ordered shutdown of §9.4 so the
`202` is on the wire and the database is consistent before the job is even issued. A CI test asserts
that `internal/selfupdate` and the `/system/restart` handler contain no call to the blocking forms.
`execController` implements the same split with `systemctl --no-block`.

- Connect with `dbus.NewSystemConnectionContext` (system bus, polkit-mediated), **not**
  `NewSystemdConnectionContext` (private socket, root only). An integration test asserts the
  distinction, because getting it wrong produces a design that only works as root.
- D-Bus wins over parsing `systemctl` for four reasons: push-based state (`SubscribeUnitsCustom`
  delivers changes as they happen, and instance state is the most-watched thing in the UI); typed
  properties in one call, so no output parsing that breaks across systemd versions or locales;
  job semantics, where `StartUnit` returns a job path and completion is a signal rather than a
  timeout guess; and error identity (`org.freedesktop.systemd1.NoSuchUnit` instead of exit code 5
  plus English prose). It is also pure Go.
- The connection is supervised. On drop (bus restart, suspend) it reconnects with exponential
  backoff capped at 30 s, then **resynchronizes every managed unit's properties and reconciles
  against the DB before resuming event processing**; transitions missed during the outage are
  reconstructed from `NRestarts` and `ExecMainExitTimestamp`.
- Journal reading is a `journalctl -o json [--follow]` subprocess in both implementations (D6),
  scoped `--user` in user scope. One subprocess per active log viewer, killed with its context.
- **The identity must actually be allowed to read that journal, and that is arranged, not assumed
  (D77).** `journalctl` shows a caller only what journald's access rules permit: members of
  `systemd-journal` (and of `adm`/`wheel` where the distribution grants it) see the whole journal,
  and everyone else sees only their own `SplitMode=uid` user journal. On the default topology — the
  installing user, uid ≥ 1000 — the instance and daemon units log under that same uid, so it happens
  to work. On the `--dedicated-user` topology SPEC §5.1b describes, the account is created with
  `useradd --system` (§13 step 4) and therefore has uid < 1000: journald keeps its messages in the
  **system** journal, and the subprocess returns an empty stream with exit 0. Every journal consumer
  in this design would then silently show nothing — `GET /system/journal`,
  `GET /instances/{id}/logs`, the §5.8 fit observation, F19's captured tail and
  `llamaman diagnostics` — which is a required SPEC §3.3 feature failing quietly on a supported
  install. So:
  - **`install-units` grants it**, idempotently and as root: it adds the service identity to the
    `systemd-journal` group (creating no group — journald ships it) and prints what it did. This is
    the same privileged, installer-owned write that already produces the units and the polkit rule,
    and it is re-applied by the F16 `install-units` repair path.
  - **The daemon probes it once at boot** (§11.1 step 6) rather than trusting the grant: run
    `journalctl -o json -n 1 --unit=llamaman.service` (plus `--user` in user scope) and record
    `runtime_info.journal_read` — `ok` when it returns at least one entry, `denied` when it exits 0
    with no entries for a unit that has demonstrably logged this boot, `unavailable` when
    `journalctl` is absent or exits non-zero.
  - **A denial is visible, never silent**: `journal_read` is in `GET /system/capabilities`, the log
    panes render the **F23** remediation card carrying
    `sudo usermod -aG systemd-journal <identity> && sudo systemctl restart llamaman.service`, and
    `GET /system/journal` returns `409 journal_unavailable` instead of an empty body. The fit
    observation (§5.8) is skipped and the report stays `confidence: "modeled"` rather than being
    calibrated from nothing.

### 5.4 `llamaman.service`

```ini
# llamaman-units: <N>
[Unit]
Description=Llama Man — llama.cpp management daemon
Documentation=https://github.com/jlbyh2o/llamaman
After=network-online.target dbus.service
Wants=network-online.target
# --user-units mode replaces the two lines above with `After=basic.target dbus.socket` /
# `Wants=dbus.socket` and `WantedBy=default.target`. A user unit runs INSIDE user@<uid>.service and
# therefore cannot order itself against it — see §5.2a.
#
# D88: the self-update revert. When this unit finally enters the `failed` state — which, under
# Restart=always, means the start limit below was exhausted — systemd starts the judge, which is the
# retained previous binary. Its own two ConditionPathExists= lines make it inert unless an update is
# genuinely unconfirmed, so this line is safe to carry unconditionally, on every host, forever. It
# replaces the timer unit, the call that armed it, the call that disarmed it, the polkit entry the
# arming call needed, and the two deadline constants the previous design froze across binaries.
OnFailure=llamaman-update-verify.service
# The three properties below ARE the revert deadline, and CI asserts both the values and the
# arithmetic: 5 x (45 + 2) = 235 s to reach `failed`, and 235 < 600 so the burst counter cannot reset
# between attempts and leave a hanging daemon looping in `activating` forever.
#
# D93: systemd's start rate limit counts EVERY start attempt, not only the failed ones — a
# `systemctl restart` typed by a human, one from the restart button behind the four `restart_required`
# settings (§3.4), one per installer re-run (§13 step 11) and one per self-update all land in this
# counter. So the daemon clears it: after 60 s of continuous readiness it calls
# ResetFailedUnit(llamaman.service) (§11.1 step 12), which resets this counter as well as the failed
# state, and §12.1 step 7 calls it once more before summoning the swap actor. The limit therefore
# counts CONSECUTIVE STARTS THAT NEVER BECAME HEALTHY, which is what "the revert deadline" always
# meant. The burst of 5 rather than 3 is the margin for the one host that cannot make that call at
# all — a withheld `manage-units` grant — where 5 in 600 s is the entire budget. That same grant is
# what RestartUnit needs, so `POST /system/restart` answers `409 systemd_denied` there and spends
# nothing (§3.3); the budget is spent only out of band, and §11.1a and F26 state the residual.
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
Type=notify
NotifyAccess=main
WatchdogSec=30
# A daemon that has not sent READY=1 in 45 s is killed and retried. Legitimately slow starts extend
# this rather than trip it: the boot sequence sends EXTEND_TIMEOUT_USEC= every 10 s while
# PRAGMA integrity_check or a migration is running (§11.1 step 4), so the case that used to worry
# this design — a healthy but merely slow daemon being reverted — cannot arise.
TimeoutStartSec=45
User=@IDENTITY@
Group=@IDENTITY_GROUP@
ExecStart=@PREFIX@/llamaman serve @SCOPE_FLAG@ @PORT_FLAG@
Restart=always
RestartSec=2
# D58: keep the listening sockets alive across a restart. Default FileDescriptorStorePreserve=
# is `restart`, which is exactly the scope wanted: preserved across `systemctl restart`, dropped on
# a full stop. 256 covers the management listener plus 255 instances.
FileDescriptorStoreMax=256
TimeoutStopSec=45
StateDirectory=llamaman
StateDirectoryMode=0750
# %S is the state-directory ROOT the manager chose: /var/lib for a system unit,
# $XDG_STATE_HOME (normally ~/.local/state) for a user unit. Writing it as a specifier rather than a
# literal is what makes one template correct in both scopes (D72, §5.2a item 4).
WorkingDirectory=%S/llamaman
SyslogIdentifier=llamaman
LimitNOFILE=65536
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=no
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryAccounting=yes
ReadWritePaths=%S/llamaman

[Install]
WantedBy=multi-user.target
```

**The `# llamaman-units: <N>` line is part of the rendered content, and its position is fixed: the
very first line of the file, above `[Unit]`, in every unit `install-units` writes** — the three
templates printed in this document, `llamaman-instances.target`, and the two self-update units of
§12.2. `<N>` is the integer template version compiled into the binary that rendered them (D95, §13
step 7). It is stated here because §5.4a hashes the file as a whole: a unit transcribed without the
line hashes differently *and* reports an absent stamp, which is a permanent `drift: stale` that makes
F16 unreachable for the hand-edit it exists to catch.

Four substitutions, one specifier and one deliberate absence:

- `@PREFIX@` is the installation directory, default `/usr/local/bin`, threaded from
  `install.sh --prefix` through `llamaman install-units --prefix` into **both** unit templates
  (D15). Nothing hardcodes the path: `install-units` writes it, the daemon records the resolved
  `filepath.EvalSymlinks(os.Executable())` in `runtime_info.binary_path` at boot for display and
  diagnostics, and the two privileged self-update actors derive their target from their own
  `os.Executable()` rather than from a constant (§12). A `--prefix /opt/bin` install therefore
  produces units, a self-update and a `GET /system/units` drift check that all name `/opt/bin`.
- `@PORT_FLAG@` is empty by default and becomes `--port N` when `install.sh --port N` was used
  (§11.1 defines the precedence: it *seeds* `ui.port_desired` on a fresh DB and the DB wins
  thereafter). It is a flag on our own binary written by our own installer — not a config file and
  not an environment variable, so SPEC §3.9 is intact.
- **`@SCOPE_FLAG@` is how the daemon knows which topology it is in**, and it is the same argument
  from SPEC §3.9: a flag our installer writes into our own unit. `install-units` renders it as
  `--scope user` under `--user-units` and as nothing otherwise, because `install-units` is the one
  component that *decides* the topology — it writes the units, chooses `/etc/systemd/user` over
  `/etc/systemd/system`, and installs or omits the polkit rule. Leaving the daemon to infer the
  answer was the gap: `runtime_info.systemd_scope` selects the whole self-update path (§5.2a item 2),
  the state-directory fallback chain (§11.1 step 1), whether the two boot `CheckAuthorization` calls
  are made at all (§5.2), whether `journalctl` is scoped `--user` (§5.3) and whether
  `selfupdate-apply` refuses to run (§12) — and the obvious candidates (`$XDG_RUNTIME_DIR`,
  `$INVOCATION_ID`, the euid, which bus connected) disagree with one another in exactly the edge
  cases that matter. §11.1 step 1 states the one rule and its single fallback.

  **The same argument is rendered into the two self-update units, and there it has no fallback at
  all.** `install-units` writes `selfupdate-apply --scope system` on the oneshot (a unit it installs
  only in system scope) and `update-verify --scope system|user` on the judge (a unit it installs in
  both). Those actors may run with no daemon at all — the judge's trigger condition is precisely that
  — so neither the database nor a bus probe is available to them; §12.2 states the rule and the
  refusal that replaces the fallback. On the judge the argument selects one thing only, whether
  `systemctl` is addressed with `--user`, and because that `ExecStart` runs the *previous* binary it
  is frozen across versions in both directions.
- **`%S/llamaman` is a specifier, not a path constant (D72).** `StateDirectory=llamaman` resolves to
  `/var/lib/llamaman` under the system manager and to `$XDG_STATE_HOME/llamaman` under
  `systemd --user`, and it exports `$STATE_DIRECTORY` either way. Spelling `WorkingDirectory=` and
  `ReadWritePaths=` with `%S` keeps the three in agreement in both topologies; hardcoding
  `/var/lib/llamaman` made the D2 install start in a directory it did not own and declare a
  `ReadWritePaths=` for a tree it never touches. §11.1 step 1 states the matching runtime rule.
- **`@HF_HOME@` is gone from `ReadWritePaths` (D57).** `ProtectSystem=full` makes only `/usr`,
  `/boot`, `/efi` and `/etc` read-only, and `ProtectHome=no` leaves home directories alone, so every
  plausible cache root — `~/.cache/huggingface`, `/mnt/models`, `/srv/…` — is already writable
  without naming it. Naming it was the bug: a UI-initiated cache relocation would leave a stale path
  baked into a unit the daemon cannot rewrite. Cache roots under a protected prefix are rejected at
  registration with `422 root_path_protected` (§3.7) rather than silently failing at write time.

### 5.4a Unit-affecting changes, and what the UI can and cannot do (D57)

SPEC §3.9 promises that anything requiring a unit change happens "through the UI or installer flags,
which regenerate the units". The design keeps that promise by shrinking the set of unit-affecting
changes to one, and by being explicit about it:

| change | mechanism | UI affordance |
|---|---|---|
| Cache relocation / additional cache roots | **No unit change at all** — see above | full: `POST /cache/roots`, `…/promote`, `PATCH /settings {hf.hub_dir}` (§7.2a) |
| Management port / bind | DB settings + `POST /system/restart` | full |
| Repairing deleted or drifted units and polkit files | `sudo llamaman install-units …` | `GET /system/units` shows the drift and the exact command; F9/F16 remediation cards carry it |
| **Service identity** (`User=`/`Group=`) | installer only: `install.sh --user <name>` / `--dedicated-user`, which re-runs `install-units` | `/settings → Danger zone` shows the current identity **read-only**, explains why (changing it means re-chowning `/var/lib/llamaman` and a different home cache, both of which need root), and prints the copyable one-liner. There is no endpoint that pretends to do it |

The daemon is unprivileged at runtime by design (§5.1); the only privileged writes in the system are
`install-units` (root, invoked by a human or the installer) and the self-update oneshot (§12). Adding
a third privileged helper so the UI could rewrite `User=` would widen the privilege surface D3 and
D15 exist to keep narrow, for a change made once per host. `GET /system/units` closes the loop by
making drift *visible* rather than requiring the user to notice. At boot and on demand the daemon:

1. reads each installed unit's **`# llamaman-units: <N>` stamp** and then hashes the file against what
   the running binary's embedded templates would render for the recorded identity, **prefix**, port and
   **`runtime_info.systemd_scope`**. The scope belongs in that list because it is a substitution in
   three units — `serve @SCOPE_FLAG@`, `selfupdate-apply --scope system` and `update-verify
   @SCOPE_ARG@` (§5.4, §12.2) — so rendering without it either reports a permanent false F16 or
   silently ignores the one argument §12.2 gives no fallback for. The two self-update actor units carry
   one substitution the daemon unit does not, `@SYSTEMCTL@` in their two `ExecStopPost=` lines (§12.2),
   and the drift render resolves it through the same single function `install-units` uses —
   `systemd.SystemctlPath()`, the deterministic two-candidate probe of §12.2, never a `PATH` search —
   so the two agree on any host.

   **The stamp is what makes a mismatch decidable (D95).** Units are written once at install time and
   are *not* rewritten by a self-update (§12.2), while this check renders from the *running* binary's
   templates — so on a host that self-updated across a release which touched any of the five
   templates, the installed file and the rendered one legitimately differ and nobody edited anything:
   - stamp **equal** to the running binary's template version, hash different → a hand-edited unit,
     which is precisely the edit that would remove the line keeping a killed actor from leaving the
     host with no daemon → **F16**;
   - stamp **older or absent** → `drift: stale`, reported at `info` in `GET /system/units` with the
     `sudo llamaman install-units --identity <user>` line, **blocking nothing**: no F16, no refused
     update, no card demanding an editor. Release notes flag a release that moves the stamp, and the
     Updates page shows the one-line repair after the update rather than before it;
   - file **absent** → **F16**, as before.

   **Two properties of these units gate every update, and they are read as directives, never as a
   hash** (D95): `POST /update/apply` refuses `409 revert_unavailable` when the installed
   `llamaman.service` does not carry `OnFailure=llamaman-update-verify.service`, or when
   `llamaman-update-verify.service` is absent or masked, and `409 selfupdate_unsupported` when — in
   system scope — `llamaman-selfupdate.service` is absent or masked (§12.1 step 1, D88). The swap unit
   is enumerated here beside the judge unit for symmetry: a host missing it passed every earlier clause,
   staged the whole update, wrote `pending`, handed its listeners to the fd store and only then failed
   to summon a unit that does not exist — a wholly avoidable outage this check now prevents before
   anything moves. Both predicates are grep-shaped facts about a file's content, so they answer the
   same on a `stale` host as on a fresh one;
2. enumerates `/etc/systemd/system/*.wants/` and `*.requires/` entries and every masked unit,
   diffing them against the set `install-units` wrote, and raising **F21** on anything unexpected.
   This second check exists because the `manage-unit-files` polkit action cannot be scoped to unit
   names (§5.2): the grant is real, so the detection has to be real too. It is read-only — the daemon
   reports drift and prints the `install-units` command, and never repairs `/etc` itself.

A prefix change is drift like any other: a unit whose `ExecStart` names a path the running binary is
not executing is reported with both paths and the exact
`sudo llamaman install-units --identity <user> --prefix <actual>` line.

`Type=notify` (D9): `READY=1` is sent only after the DB is open, `PRAGMA integrity_check` has
passed, migrations are applied, the HTTP listener is bound **and the self-update confirmation gate has
run** (§11.1 steps 10–12) — that last one so a daemon which signals readiness has provably already
resolved `update/pending` and cannot leave the judge armed against a version that booted (D92). A
goroutine sends `WATCHDOG=1` every
10 s **gated on a live `SELECT 1`**, so a daemon wedged on its database is killed and restarted
instead of accepting requests it cannot serve. `STATUS=` carries the resolved URL, so
`systemctl status llamaman` shows the port the walk actually landed on — which SPEC §3.9 requires to
be discoverable from the host. sd_notify is ~25 lines writing datagrams to `$NOTIFY_SOCKET`; no cgo,
no dependency. (`$NOTIFY_SOCKET` is set by systemd, not by a user, so it does not breach the
zero-config rule.)

`ProtectHome=no` is mandatory: the models live under a home directory (SPEC §5.1b).

### 5.5 `llamaman-instance@.service` and the target

```ini
# llamaman-units: <N>
[Unit]
Description=Llama Man instance %i (llama-server)
Documentation=https://github.com/jlbyh2o/llamaman
After=network-online.target
Wants=network-online.target
PartOf=llamaman-instances.target
After=llamaman.service
# --user-units mode replaces the first two lines with `After=basic.target` and NO `Wants=`:
# network-online.target does not exist in a user manager, and a `Wants=` on a unit the manager cannot
# find fails the start. Nothing is lost — llama-server binds 127.0.0.1 only (§5.7) and never needed a
# routable address. `PartOf=`/`After=llamaman.service` name user units and resolve normally. §5.2a
# item (1) tabulates the rewrite for all three templates and CI asserts it.
# `After=` with NO Wants=/Requires=/PartOf= on llamaman.service. Ordering only: at a host boot the
# instance start job is sequenced after the daemon's start job COMPLETES, which — because the daemon
# is Type=notify and sends READY=1 only after migrations are applied (§5.4a) — removes the
# schema-migration race described in §5.6a. Because there is no requirement dependency, a daemon that
# FAILS to start still lets every instance proceed: the job completes either way, and §5.6's promise
# that "instances start correctly at boot even when llamaman.service itself is failing" is intact.
# Deliberately NOT PartOf=llamaman.service: instances must survive daemon restarts and
# self-update (SPEC §3.8).

[Service]
Type=exec
User=@IDENTITY@
Group=@IDENTITY_GROUP@
ExecStart=@PREFIX@/llamaman instance-exec %i
Restart=no
TimeoutStartSec=900
TimeoutStopSec=60
KillSignal=SIGINT
KillMode=mixed
FinalKillSignal=SIGKILL
OOMPolicy=stop
# StateDirectory= is declared here too, and not only on llamaman.service, for one reason: it is what
# exports $STATE_DIRECTORY to `instance-exec`, whose first act is to open <state_dir>/llamaman.db
# (§5.6 step 1). Without it the launcher would fall through to §11.1 step 1's guesswork in a process
# that has no bus to interrogate — and would guess `/var/lib/llamaman` under `systemd --user`. The
# directory already exists and is already owned by the service identity; this line only names it.
StateDirectory=llamaman
StateDirectoryMode=0750
WorkingDirectory=%S/llamaman
Slice=llamaman.slice
SyslogIdentifier=llamaman-instance@%i
LogRateLimitIntervalSec=0
LogRateLimitBurst=0
StandardOutput=journal
StandardError=journal
LimitNOFILE=65536
LimitMEMLOCK=infinity
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=full
ProtectHome=no
ProtectKernelTunables=yes
RestrictSUIDSGID=yes
MemoryAccounting=yes
CPUAccounting=yes
# PrivateDevices is deliberately ABSENT: it hides /dev/nvidia* and /dev/nvidiactl and breaks
# every CUDA instance with an opaque error.

[Install]
WantedBy=llamaman-instances.target
```

`llamaman-instances.target` is trivial (`After=network-online.target`, `AllowIsolate=no`,
`WantedBy=multi-user.target`; `After=basic.target` / `WantedBy=default.target` in user scope).
Autostart is `EnableUnitFiles` linking the instance into that target,
which also gives one lever for "stop everything for maintenance". Because that target starts enabled
instances at boot **without the daemon being involved at all**, `instances.autostart` and
`instances.desired_state` must agree at that moment — which is exactly what the D53 coupling in §2.8
guarantees, and why `instance-exec` records an unstamped start as `trigger='external'` (§5.6) rather
than inventing one.

Load-bearing notes:

- **`Restart=no` (D7).** The supervisor is the only restarter. This makes all three per-instance
  policies exact and data-driven, keeps the template content-free, and avoids the base proposal's
  "exit 0 means the launcher refused" trick, which makes journald report a clean exit for a unit
  that declined to start. The cost is bounded: a crashed instance is not restarted while the daemon
  is down, and the daemon is `Restart=always` with a 30 s watchdog, so that window is seconds; the
  supervisor's boot reconciliation starts anything whose `desired_state='running'` while its unit is
  inactive, so a crash during daemon downtime is repaired on the daemon's return. At a *host* boot
  that same reconciliation first derives `desired_state` from `autostart` (D53, §5.8), so it agrees
  with what systemd has already done through `llamaman-instances.target` instead of undoing it.
- Because `Restart=` is `no`, `StartLimitIntervalSec`/`StartLimitBurst` would be inert and are
  omitted. If they are ever reintroduced they belong in **`[Unit]`**, never `[Service]` — systemd
  moved them in v229 and silently ignores them in the wrong section.
- `KillSignal=SIGINT`: llama-server treats SIGINT as a clean shutdown that finishes in-flight slots.
- `LimitMEMLOCK=infinity` so `--mlock` works when the user enables it.
- `LogRateLimitIntervalSec=0`: journald's default per-unit rate limit eats exactly the bursty
  llama.cpp load output a user needs after a failed load.
- `TimeoutStartSec=900` is not a model-load timeout. Under `Type=exec` the unit is considered
  started once `execve` succeeds; the value covers slow shutdown and restart bookkeeping only. The
  actual load timeout is `instances.start_timeout_sec`, enforced by the supervisor's health probe.

### 5.6 `llamaman instance-exec <name>` — the render step

Runs in the unit's context, as the service identity, with **no D-Bus, no HTTP, no GPU probe and no
network of any kind** — its entire world is the binary, the DB file and `%i`. Because of that,
**instances start correctly at boot even when `llamaman.service` itself is failing** — the control
plane can be broken without taking inference down. It follows that nothing the launcher does may
depend on live hardware state, which is precisely why `-ngl auto` is resolved by llama.cpp rather
than here (D51, §5.7).

**The ledger row is opened before preflight, not after it (D54).** Every exit code below is a real
failure a user must be able to see and a restart policy must be able to count; a row written only on
the happy path would make a config that fails preflight on every attempt look like an instance that
never tried to start — no crash-loop cutoff, no start history, infinite backoff retries. So the
launcher takes its short-lived **read-write** connection first and closes the row on every path out.

1. Resolve the state directory exactly as §11.1 step 1 does — `$STATE_DIRECTORY` first, which the
   instance template's own `StateDirectory=llamaman` guarantees is set (§5.5) — and open a short-lived
   read-write connection to `<state_dir>/llamaman.db` (`busy_timeout=5000`; the design accounts for
   this separate-process writer in §2). Failure → exit **70** with no row — the supervisor detects the
   unit failing with no open ledger row and synthesizes one with `outcome='failed'`,
   `error_code='launcher_db_unavailable'`.
1b. **Schema gate (§5.6a).** Read `MAX(version)` from `schema_migrations` and compare it with the
   migration set compiled into this binary. Equal → proceed. Older → wait for the daemon to migrate,
   as described below. Newer → exit **75**. Nothing else in the launcher touches a table until this
   passes — **including `instance_starts`**. Exit 75 therefore records **no ledger row**, exactly like
   exit 70 above, and for the same reason: writing a row is precisely the operation one must not
   perform against a schema this binary does not understand. The supervisor synthesizes the row
   instead (§5.6a).
2. Load the instance row; missing or `deleted_at` set → exit **64** (no row: the FK has no parent,
   and an instance the user deleted needs no history).
3. **In one transaction** (`BEGIN IMMEDIATE`, which also re-asserts the step-1b schema check so the
   whole run is pinned to one schema version): first **close any `instance_starts` row for this
   instance that is still open** — `outcome='failed'`, `error_code='launcher_superseded'`,
   `ended_at=now`, `exit_code` left NULL — because `idx_instance_starts_open` is UNIQUE (§2.8) and a
   surviving open row would otherwise make this insert, rather than the earlier lost write, the thing
   that fails. A row survives only when the previous run's closing `UPDATE` could not land (a locked
   or unwritable DB, below) or when an external `systemctl start` raced the supervisor; either way the
   outcome of that run was never observed, which is why it is recorded as such and excluded from the
   crash-loop count (§5.8). Then insert `instance_starts` with `outcome=NULL`,
   `ready_at=NULL`, `argv_json=NULL`, `at=now`, `config_hash` from the row,
   `trigger = COALESCE(instances.pending_trigger, 'external')` and
   `override_json = instances.pending_override_json`; then clear **both** `pending_trigger` and
   `pending_override_json`. This is the hand-off half of the trigger contract (§5.8): the daemon
   stamps its intent before `StartUnit`, the launcher consumes it, and a start nobody stamped — a
   boot start of an enabled unit, or a hand-run `systemctl start` — is honestly recorded as
   `external` instead of guessed. Consuming the override in the *same* transaction is what makes
   safe start one-shot (D61, §3.10b): a crash, a reboot or a supervisor restart all find the columns
   already clear and launch the saved configuration. The row id is held in memory for the rest of
   the run.
4. Parse `flags_json` / `extra_flags`, then apply `override_json` as a shallow patch over the parsed
   `FlagSet` (present keys replace; `extra_flags` is dropped for an overridden run); unparsable
   either side → exit **65**. Refuse to start when `instances.draft_validation='mismatch'` — exit
   **65** with `error_code='draft_vocab_mismatch'` (§3.10a).
5. Resolve `versions/active` to a concrete directory; `stat bin/llama-server` → exit **69** if absent.
   Additionally refuse — exit **69** with `error_code='runtime_rebuilding'` — when the `is_active=1`
   `llamacpp_versions` row is not in state `ready`. A forced rebuild of the active version (D71/D78,
   §6.2) moves that row out of `ready` for the duration, and the directory `versions/active` names is
   the one being reinstalled; the row is the only thing the launcher can consult to know that, and
   consulting it costs one indexed read of a table it has already opened. Together with D78's staging
   protocol this closes both halves of the hazard: nothing is written into a live directory, and
   nothing starts against one mid-swap.
6. Validate model paths: primary GGUF (shard 1 for sharded sets), optional mmproj, optional draft.
   Missing → exit **72**, with the resolved path in the message.
7. Render argv (§5.7). `-ngl auto` renders no `-ngl` flag (D51); nothing here consults the GPU, the
   fit calculator or the network, and the process remains free of D-Bus and HTTP.
8. Test-bind `127.0.0.1:<internal_port>`; occupied → exit **78**, with
   `detail_json={"internal_port":N,"public_port":M}` so F5's "shown in the start history with both
   ports" is literally true.
9. Update the ledger row with `argv_json`, `llamacpp_version_id` and `effective_config_hash` (the
   hash of what was actually rendered — equal to `config_hash` normally, and the override hash for a
   safe start), and commit. The row is now *open*: written **before** the exec, so it survives a
   crash-on-load. The launcher does **not** write `instance_status`: `applied_config_hash` is
   stamped by the supervisor when `/health` first returns 200 (§2.8), so a process that dies during
   model load never records a configuration that ran.
10. Print one canonical line to stderr:
    `llamaman: exec <version_id> <argv…>` — the first thing anyone sees in the unit's journal.
11. `syscall.Exec(server, argv, env)`. No `LD_LIBRARY_PATH` is set; the RPATH from D22 makes each
    version directory self-contained.

**Every failing path from step 4 onward closes the row before exiting**: one `UPDATE` setting
`outcome='failed'`, `exit_code=<the code below>`, `error_code`, `error_message`, `detail_json` and
`ended_at=now`. If that write itself fails (a locked or unwritable DB), the launcher still exits with
the correct code and the supervisor closes the row from the unit's `ExecMainStatus` — the ledger is
never left open by a process that has exited.

**Steps 1, 1b and 2 are before the row exists, and that is deliberate.** Exits **70** (no usable DB
connection), **75** (schema gate) and **64** (no such instance) write nothing to `instance_starts`,
because in all three cases writing is either impossible or unsafe:

| exit | why no row | who records the attempt instead |
|---|---|---|
| 70 | there is no working read-write connection | supervisor: sees the unit fail with no open row and synthesizes one, `outcome='failed'`, `error_code='launcher_db_unavailable'` |
| 75 | the schema is not the one this binary understands, and `instance_starts` is a table under that schema — the `schema_ahead` branch says so explicitly, and it applies just as much to an INSERT as to a SELECT | supervisor: same synthesis, `error_code='schema_mismatch'` or `'schema_ahead'` from the unit's `ExecMainStatus` of 75 plus the journal line |
| 64 | the instance row is gone, so the FK has no parent — the insert cannot succeed | nothing; an instance the user deleted needs no history |

The synthesized rows are ordinary closed rows and appear in `GET /instances/{id}/starts` like any
other. They differ from launcher-written rows in one respect only: `argv_json`,
`effective_config_hash` and `llamacpp_version_id` are NULL, because nothing was rendered.

**Exit-code contract** — stable, tested, and mapped to a UI remediation card (§17):

| Code | Meaning | `error_code` | UI remediation |
|---|---|---|---|
| 64 | instance not found or deleted | `instance_missing` | offer re-create |
| 65 | invalid stored flags | `bad_flags` | open the config editor at the offending field |
| 69 | active llama.cpp version missing or unusable | `runtime_missing` | offer rebuild or one-click rollback |
| 69 | the active version is being rebuilt in place (D78) | `runtime_rebuilding` | "waiting for the rebuild of b10621 to finish"; the supervisor starts it on its own once the row is `ready` again, so no user action is needed |
| 70 | internal error (DB unreadable, and so on) | `launcher_db_unavailable` | link the diagnostics bundle |
| 72 | model file missing | `model_missing` | offer re-download from the recorded repo/revision, or re-point |
| 75 | schema/binary mismatch (§5.6a): DB behind and not resolved within the bounded wait, or DB ahead. **No ledger row is written** — the supervisor synthesizes one, and it is excluded from the crash-loop count | `schema_mismatch` / `schema_ahead` | "the daemon has not finished upgrading the database"; the card offers Start once the daemon is up, and the supervisor does so on its own |
| 78 | port conflict | `port_conflict` | port picker with a live bind probe; the supervisor also reassigns (F5, §5.8) |

**Who closes an `instance_starts` row** — the complete writer table. Because `outcome` is written
**exactly once** (D63) and `ready` is not one of its values, no two rules can ever close the same row
and there is no precedence question to resolve:

| situation | writer | column written | value |
|---|---|---|---|
| a row from a previous run is still **open** at the start of a new one | `instance-exec`, inside its step-3 transaction, before the insert | `outcome`, `error_code`, `ended_at` | `failed` / `launcher_superseded` / now. `exit_code` stays NULL — nothing observed how that run ended, which is exactly why the row was left open, and D64 excludes it from the count for the same reason |
| preflight failure (65, 69, 72, 78 — steps 4–8) | `instance-exec` | `outcome`, `exit_code`, `ended_at` | `failed` / the launcher code / now |
| launcher exited **before** the row existed (70, 75, 64) | supervisor | a **new** row, opened and closed in one statement | `outcome='failed'`, `exit_code` from `ExecMainStatus`, `error_code` per the §5.6 table, `argv_json`/`effective_config_hash`/`llamacpp_version_id` NULL. Nothing is written for 64 |
| `/health` first returns 200 | supervisor | **`ready_at` only** | now. `outcome` and `ended_at` stay NULL — the run is still in flight, and this row remains *the* open row for as long as the process lives |
| `/health` fails 3× while the unit is active | supervisor | — | nothing on the ledger; `instance_status.state` goes `degraded`. A run that recovers has one row, not three |
| unit goes inactive or failed after a start | supervisor | `outcome`, `exit_code`, `ended_at` | `failed` if `ExecMainStatus != 0` or `Result != 'success'`, else `stopped` / `ExecMainStatus` / `ExecMainExitTimestamp` |
| an explicit stop was requested | supervisor | as above | `stopped`, regardless of exit status — a stop the user asked for is not a failure even when llama-server exits non-zero on SIGINT |
| the supervisor declines a due restart, and no `inhibited` row for the same reason already exists after `LAST_CLOSED` (§2.8) | supervisor | a **new** row, closed immediately | `outcome='inhibited'`, `error_code` = the `inhibit_reason`, `exit_code` NULL, `ended_at=now`, no `execve`. Never counted by D64, never `LAST_CLOSED`, and never written twice for one refusal episode |
| the daemon restarts while a row is open | supervisor, at boot reconciliation | as available | resolved from the unit's current properties; a unit still active leaves the row **open** (the process it describes is still running); a unit that is gone closes it `failed` with `error_code='daemon_restarted'` |

So the normal life of one row is: opened by the launcher → `ready_at` stamped → hours of serving →
closed exactly once as `failed` or `stopped`. "The most recent closed row" therefore means "the last
completed run", `on-failure` reads `outcome` rather than a NULL-prone `exit_code`, and an instance
that is `ready` has exactly one open row rather than a closed one plus a phantom.

An instance has at most one open row at a time, and that is **enforced** by the unique partial index
`idx_instance_starts_open` rather than left to the writers agreeing to behave (§2.8, D40). Two rules
keep the enforcement from ever being the thing a user sees: the supervisor closes any stale open row
at boot reconciliation (§5.8 step 3), and `instance-exec` closes any that survives into its own
step-3 transaction, in that transaction, before inserting.

### 5.6a The schema gate: `instance-exec` versus migrations at host boot

`llamaman-instances.target` starts enabled instance units at boot, and `instance-exec` opens its own
read-write connection — both by design (§5.5, §5.6). Migrations, however, are applied only by the
daemon's composition root (§11.1 step 4). After a self-update that bumps the schema, the next host
boot is therefore a race: N launchers built against schema v15 may open a database still at v14, or
open it while the daemon is applying v15. Migrations are one transaction each, so a launcher cannot
see a *half-applied* migration — but it can absolutely see v14 in one transaction and v15 in the
next. Left unstated, the likely behavior is a fleet-wide exit 70 at boot, which is precisely the
scenario §5.6 claims is impossible.

Two independent mechanisms close it, and both are cheap:

1. **Ordering.** The instance template carries `After=llamaman.service` with no requirement
   dependency (§5.5). At a host boot the daemon's start job — `Type=notify`, `READY=1` sent only
   after migrations complete — is sequenced before the instance start jobs. In the ordinary case the
   launcher never observes an old schema at all. Because there is no `Requires=`, a daemon that fails
   to start does not block a single instance.
2. **The gate itself**, for every case ordering cannot cover (a hand-run `systemctl start`, a
   `systemd_control='exec'` host, a daemon that crash-loops during migration):
   - `MAX(schema_migrations.version) == binary's highest embedded migration` → proceed.
   - **DB older than the binary** → the daemon has not migrated yet. Sleep-poll every 500 ms for up
     to **60 s**, re-reading the version. On agreement, proceed. On timeout, exit **75** with
     `error_code='schema_mismatch'` — a clean, visible failure rather than a mysterious query error.
     `Restart=no` means the unit simply stays failed; the supervisor starts it as soon as the daemon
     is up and `desired_state='running'`, so the recovery is automatic and needs no user action.
   - **DB newer than the binary** — the state after a **downgrade** across a schema bump, until
     §12.4's procedure has been run (D90, D94) — → exit **75** immediately with
     `error_code='schema_ahead'`. Waiting cannot help; running a v14 query set against a v15 schema
     can corrupt data. In the ordinary case this state is short-lived and self-correcting: the daemon
     refuses the same comparison (§11.1 step 4), `llamaman.service` reaches `failed`, and D88's revert
     puts the newer binary back within ~235 s. It persists only while a *deliberate* downgrade is
     half-done — between step 2 and step 5 of §12.4's five commands, with the service stopped — and
     stops firing once step 3 has restored the matching database.
   - **Neither branch writes a row.** The gate runs before step 3, so there is no `instance_starts`
     row to close, and inserting one would mean writing to a table under a schema this binary does
     not understand — the exact hazard the `schema_ahead` branch exists to avoid. The supervisor
     synthesizes the closed row from the unit's `ExecMainStatus=75` (§5.6), which it can do safely
     because the daemon is by definition the process that owns the current schema.
   - **Exit-75 rows are excluded from the crash-loop cutoff.** They carry `outcome='failed'`, so
     D64's query would otherwise count them, and it must not: a schema mismatch is a property of the
     daemon's upgrade state, not of the instance, and the daemon whose arrival resolves it is the same
     one doing the counting. Five instances failing the gate during a slow migration would all reach
     `crash-looping` and require a manual Reset failed for a condition that fixed itself. §5.8's
     counting query therefore excludes `error_code IN ('schema_mismatch','schema_ahead')`.
   - The check is re-asserted inside the step-3 `BEGIN IMMEDIATE` transaction, so the entire run is
     pinned to the version it validated. A migration that lands between the poll and the transaction
     aborts the transaction and restarts the gate rather than proceeding on a stale assumption.

`instance-exec` never *applies* a migration. Doing so from N concurrent unprivileged launchers with
no leader election is the one thing worse than the race itself.

### 5.7 argv rendering

One function, `instances.RenderArgv(inst, flags model.FlagSet, model, mmproj, draft, version)
[]string` (D49), in a deterministic order so `config_hash` is stable and diffs are readable: model
paths → context → offload → batching → attention and cache → devices → server/network → mode flags →
draft → extra. The `FlagSet` is passed in rather than read from `inst` precisely so a caller can hand
it the override-patched set of a safe start (§3.10b) without the override ever touching
`instances.flags_json`. Its sibling `RenderBenchArgv` (D62, §10.1) renders the same `FlagSet` for
`llama-bench`.

**`RenderArgv` is pure.** It takes rows and returns strings; it does not import `internal/fit` or
`internal/hw`, does not read live VRAM, does not touch the clock, **does not open a file**, and
produces identical output for identical rows on any host. That is what lets `instance-exec` — a
DB-read-only process with no D-Bus, no HTTP and no GPU probe — call it, and what lets the golden argv
test in §15 mean anything.

**Purity is a constraint on the inputs, so the two build-capability rules are columns, not files.**
Both rules below need to know what the active build's `llama-server --help` says, and `manifest.json`
is an on-disk file the renderer may not read. So the capture is parsed **once, at install time**, and
its two consequences are persisted on `llamacpp_versions` (§2.5): `supports_fit` (0/1) and
`help_flags_json` (the set of flag names). The `version` row is already a `RenderArgv` parameter, so
both rules stay inside a pure function over rows:

- **`-ngl auto` on a build that predates `--fit`** reads `version.supports_fit`. When it is 0, `auto`
  renders `-ngl 999` and the instance form shows "this build predates `--fit`; auto behaves as all".
- **The flag-churn guard moves out of the renderer entirely.** `RenderArgv` returns argv and nothing
  else. A sibling pure function `instances.UnknownFlags(argv []string, help model.HelpFlags) []string`
  diffs the produced flags against `help_flags_json`, and only its **callers** — the instance form's
  `POST /instances/validate`, `GET /instances/{id}/command`, and the version-activation preflight —
  invoke it. The launcher never does: it has no user to warn and no reason to spend the work, and a
  warning it could not display would be a file read bought for nothing. This also settles what the
  guard does on a build whose capture is missing (`help_flags_json IS NULL`, e.g. a row migrated in
  from an older schema): `UnknownFlags` returns empty and the UI says "flag check unavailable for this
  build", rather than flagging every flag as unknown.

**`-ngl` (D51).** The four FlagSet modes render as:

| mode | argv |
|---|---|
| `all` | `-ngl 999` |
| `none` | `-ngl 0` |
| `count` | `-ngl <count>` |
| `auto` | **nothing — the flag is omitted entirely** |

`auto` is therefore never "resolved" anywhere in the launch path: omitting `-ngl` is precisely what
leaves llama.cpp's own `--fit` enabled (upstream disables `--fit` when `-ngl` or `--tensor-split` is
pinned — D33), so the runtime that actually knows the free VRAM at load time makes the decision, and
the projection it prints becomes the ground truth shown beside our estimate. The consequences are the
point: `internal/instances` stays pure, `config_hash` cannot change because free VRAM changed, and
`restart_required` cannot oscillate.

The calculator's `max_n_gpu_layers` (§8.2, §8.7) is the **advisory** counterpart: the instance form
shows "auto — llama.cpp will choose; we estimate 37 of 37 layers fit", and one click on
`POST /instances/{id}/pin-ngl` rewrites the FlagSet to `{"mode":"count","count":37}`. Pinning is an
explicit config edit with a new `config_hash`, a bumped `generation` and a `restart_required` flag —
never something the launcher does behind the user's back.

Two guards: `auto` is rejected together with an explicit `tensor_split` at save time
(`422 ngl_auto_conflict`, because `--tensor-split` also disables `--fit`, leaving `auto` meaning
nothing); and when the active version's `supports_fit` column is 0 — set at install time from the
`llama-server --help` capture (§2.5, §6.4 step 4, §6.5 `publish`) — `auto` renders as `-ngl 999` and
the instance form shows "this build predates `--fit`; auto behaves as all". The column, not the file,
is what the renderer reads.

**`config_hash` (D52)** is `sha256` over the rendered argv **with `--host` and the value of `--port`
elided**, plus the resolved model paths and the active version id. The internal port is an
allocation detail the supervisor may reassign after an exit 78 (F5) without any user action, and a
hash that moved with it would raise a spurious `restart_required` on an instance whose configuration
nobody touched. Everything a user can actually change is in the hash.

Always appended and never user-editable: `--host 127.0.0.1`, `--port <internal_port>`, `--no-webui`
(the gateway is the front door), plus `--props`/`--slots`/`--metrics` per the FlagSet.
`extra_flags` is split with POSIX shell word rules — no shell invocation, no globbing — and appended
last, after a validation pass that rejects `--host`, `--port`, `-m`, `--model` and `--api-key`
overrides with `422 extra_flag_forbidden`.

```
/var/lib/llamaman/versions/b10621-cuda-src/bin/llama-server \
  -m /home/<user>/.cache/huggingface/hub/models--bartowski--Qwen3-8B-GGUF/snapshots/<sha>/Qwen3-8B-Q4_K_M.gguf \
  -c 8192 -ngl 999 -b 2048 -ub 512 -np 4 -fa on -ctk q8_0 -ctv q8_0 \
  --alias qwen3-8b --jinja --host 127.0.0.1 --port 21001 --no-webui --props --slots --metrics
```

**Device selection: `--device` only, never `CUDA_VISIBLE_DEVICES` (D66).** Setting both is a silent
wrong-GPU bug rather than a crash: `CUDA_VISIBLE_DEVICES` *renumbers* the devices llama.cpp sees, so
a `--device CUDA1` rendered from the user's second GPU addresses a different physical card after
masking, and `--main-gpu`/`--tensor-split` indices shift with it. The launcher therefore sets no
device environment at all, which makes one mapping true everywhere:

```
nvidia-smi index  ==  gpus.gpu_index  ==  llama.cpp's CUDA<n>
```

`gpus.uuid` remains the stable identity every other subsystem keys on (§8.6, the §10 bench guard,
`fit`), and the two are joined at exactly one point: the instance form resolves the UUIDs the user
picked into `CUDA<n>` labels at **save time**, storing the labels in `device_filter` (rendered
verbatim) and the UUIDs in `device_uuids` (never rendered). `main_gpu` and `tensor_split` are indices
into the `--device` list, matching llama.cpp, and the form labels them that way. On every `ready`
transition the supervisor records the live UUID→index map in `instance_status.device_map_json`; if it
disagrees with `device_uuids` — a GPU was added, removed or re-enumerated — it raises **F22**
("GPU order changed: `CUDA1` is now <name>, not the card this instance was configured for") with a
one-click re-resolve, instead of quietly benchmarking and serving from the wrong card.

Environment: **no `CUDA_VISIBLE_DEVICES`**; `HF_HOME`/`HF_HUB_CACHE` set from the primary cache root
for completeness (`HF_HUB_CACHE` always, `HF_HOME` only when the hub directory ends in `/hub`);
`LLAMA_CACHE` explicitly unset (llama.cpp's own `-hf` cache is never used — SPEC §3.2), and `GGML_*`
passthroughs from settings. `GET /instances/{id}/command` returns this argv and env verbatim, so what
the UI shows is what runs.

**Flag-churn guard.** At install time each version records `llama-server --help` output verbatim in
`manifest.json` and its parsed flag-name set in `llamacpp_versions.help_flags_json`, with
`supports_fit` derived from it. `instances.UnknownFlags` — a pure function over the rendered argv and
that column, called by the API layer and never by the launcher (see the purity note above) — surfaces
unknown flags as **warnings** on the instance form and in `GET /instances/{id}/command`, never as
errors: llama.cpp ships ~10 nightlies a day and a hard failure would make the tool brittle by design.
Activating a new version re-runs the check for every non-deleted instance and raises one notification
listing any instance whose argv now contains a flag the incoming build does not advertise — which is
the moment a user can still choose not to roll.

### 5.8 Supervisor loop

A single reconciler, woken by sub-state signals and by an `instances.health_poll_sec` tick (1 s while
an instance is `starting`/`loading`). Its subject set is **every instance with `deleted_at IS NULL`,
plus every instance with an open `instance_starts` row** — the second term is one lookup on
`idx_instance_starts_open` and exists so that a soft delete's `StopUnit` (§3.10c) is ledgered by the
supervisor, the only writer allowed to close that row, before the row is forgotten. Per instance it
computes `(desired, actual)` and takes **at most one** corrective action per pass:

- desired `running`, actual `stopped`/`failed` → stamp `instances.pending_trigger`, then `StartUnit`,
  subject to `restart_policy` (below), `reconcile_backoff_until` (exponential 5 s → 5 m), and the
  active version being `ready` — while a forced rebuild has moved the `is_active=1` row out of `ready`
  (D78, §6.2) no start is attempted at all, so the launcher's exit 69 `runtime_rebuilding` is the
  backstop rather than the normal path, and the UI shows "waiting for the rebuild".
- desired `stopped`, actual active → `StopUnit`.
- `autostart` ≠ unit-enabled → `EnableUnitFiles`/`DisableUnitFiles` + `Reload`, **only when the
  daemon may manage unit files**. This action is gated on the same capability
  `PUT /instances/{id}/autostart` is (§11.1a): it is skipped entirely when
  `polkit_unit_files = 0` (`install-units --no-autostart-grant`), when polkit denied the grant at boot
  (F9), and when `systemd_control='unavailable'` (F10). Ungated, it was an unconditional corrective
  action issuing a polkit-denied D-Bus call on every pass for every instance whose enable state the
  daemon cannot change — an error loop with no terminal state. Skipped, the divergence is reported
  once instead: a single `autostart_unmanaged` notification listing the affected instances and the
  `sudo systemctl enable llamaman-instance@<name>.service` lines that would reconcile them by hand,
  refreshed rather than repeated, and the instances table renders the autostart column read-only with
  the same command in a tooltip.

**Trigger provenance.** Every start the daemon initiates writes `instances.pending_trigger` in the
same transaction that decides to start (`user` from the API, `supervisor_restart` from the policy
branch, `rolling` from the canary roll in §6.6, `bench_restore` from the bench finalizer in §10) and
then calls `StartUnit`. `instance-exec` consumes and clears it (§5.6 step 3). A start with no stamp
is recorded as `external`, which is exactly the honest answer for a boot start of an enabled unit or
a hand-run `systemctl start llamaman-instance@x`. Without this hand-off the field would be a
constant, because the launcher sees only `%i` and a row.

**The one relabel, and the clock it uses (D74).** The remaining case is a start systemd performed at
boot, through `llamaman-instances.target`, *before* the daemon was up: honest as `external`, but
misleading, because the user did ask for it by enabling autostart. Boot reconciliation relabels it
`autostart` under **three** conditions, all of which must hold:

1. this is the **first daemon start of a new host boot** (the same branch that applies the D53
   coupling — step 1 below), and
2. the row's `at` falls in `[runtime_info.host_boot_at, runtime_info.boot_at)` — after the host came
   up, before this daemon did, and
3. the instance has `autostart=1`.

`host_boot_at` is the **host** boot instant, read from `/proc/stat`'s `btime` field (seconds since
the epoch, ×1000) and stored beside `host_boot_id`. Using `runtime_info.boot_at` for condition 2 —
the *daemon* start time — was the bug: every ordinary daemon restart, including the one every
self-update performs, makes **all** prior `external` starts of an autostart instance older than
`boot_at`, so a deliberate `systemctl start llamaman-instance@x` typed at a shell three days ago gets
rewritten in the ledger as `autostart`. That defeats the honesty the rest of this section argues for.
Condition 1 is what bounds it further: within one host boot, no relabel ever happens, so a hand-run
start is a hand-run start forever. A hand-run start that genuinely happened in the boot window before
the daemon came up is still indistinguishable from an autostart, and the ledger says `autostart`;
that ambiguity is inherent (nothing observed it) and is a few seconds wide rather than days.

**Restart policy (D7/D8),** evaluated from the `instance_starts` ledger, so behavior is observable
data rather than systemd guesswork. "Previous start" always means `LAST_CLOSED` as §2.8 defines it —
the most recent row with `outcome IS NOT NULL` **and `outcome != 'inhibited'`** — which, since
`outcome` is written once at the end of a run (D63), is the last completed run. The `inhibited`
exclusion is what stops the policy from erasing its own input: the refusal row this section writes
would otherwise become the "previous start" on the very next pass, `outcome='stopped'` would no longer
be what the policy sees, and an instance would oscillate between declining and restarting on a
five-second tick:

- `always` — restart on any exit, clean or not.
- `on-failure` — restart only when the previous start closed with `outcome='failed'` (a non-zero
  exit, a failed unit, **or** a preflight failure from the §5.6 table). A previous
  `outcome='stopped'` is not restarted, whatever its exit code — and because the decision reads
  `outcome` rather than `exit_code`, it is well-defined on a preflight row and on a row the
  supervisor closed from unit properties it could not fully observe.
- `never` — do not restart; write an `instance_starts` row with `outcome='inhibited'` and
  `error_code='policy_never'` **if this refusal episode has not already been recorded** (§2.8: at most
  one such row per `(LAST_CLOSED, inhibit_reason)` pair), emit an event naming the policy, and let the
  derived `inhibited` flag (§2.8) surface it. The instance's `state` stays `failed`, because that is
  what it is. The same conditional applies to the other two reasons, `crash_loop` and `clean_exit`:
  declining is this pass's one corrective action, the pass repeats every `health_poll_sec`, and an
  unconditional write would bury a 500-row-per-instance history under refusals within the hour.

**Crash-loop cutoff (D8/D64) — failures only.** The count is

```sql
SELECT count(*) FROM instance_starts
 WHERE instance_id = ?
   AND outcome = 'failed'                                   -- NOT every row
   AND at > :now - instances.restart_window_sec * 1000
   AND at > instance_status.restart_window_reset_at
   AND (error_code IS NULL                                  -- §5.6a: a schema gate failure is a
        OR error_code NOT IN ('schema_mismatch',            -- property of the daemon's upgrade
                              'schema_ahead',               -- state, not of this instance; and
                              'launcher_superseded'));      -- a superseded row is a run whose
                                                            -- outcome was never observed (§5.6),
                                                            -- so counting it as a failure is a guess
```

`> restart_max` → `crash-looping`, no further automatic starts, and the UI offers **Reset failed**
and **Safe start** (F3). Four consequences are deliberate, and each one is a bug the naive
"count every row" rule would have shipped:

- **Successful and user-initiated starts are invisible to the cutoff.** Starting and stopping an
  instance six times in ten minutes while tuning flags is normal operator behavior, not a crash
  loop; `outcome='stopped'` rows are never counted.
- **`inhibited` rows are never counted, and never become `LAST_CLOSED`.** They are written *by* the
  refusal, so counting them would make the refusal self-reinforcing: each declined restart pushes the
  total further above the threshold and the state could never clear on its own. The `LAST_CLOSED`
  exclusion (§2.8) is the second half of the same argument and closes the second half of the same bug:
  a refusal row that *was* the previous start would make `inhibit_reason='clean_exit'` false one pass
  after it became true, so the badge would flicker off while the instance stayed inhibited.
- **Maintenance cannot trip it.** A rolling restart (§6.6), a bench stop-and-restore (§10) and a user
  restart all close their rows as `stopped`, so a maintenance window with `restart_max=5` is not a
  crash loop.
- **A start that worked resets the window.** When a run has `ready_at` set and has been alive for
  ≥ 60 s, the supervisor stamps `instance_status.restart_window_reset_at = ready_at`, so an instance
  that served for an hour and then crashed twice is at 2, not at 2 + whatever it accumulated last
  week. `POST /instances/{id}/reset-failed` and `POST /instances/{id}/safe-start` stamp the same
  column with `now` **inside the request's own transaction** (§2.8's narrow `instance_status`
  exception list), which is what makes those buttons actually clear the state rather than merely
  change the label.
- **A schema-gate failure is not the instance's fault.** Exit 75 rows are excluded by `error_code`
  (§5.6a): they are synthesized by the supervisor for a launcher that ran against a database the
  daemon had not finished migrating, and the daemon's own arrival is the fix. Counting them would put
  every autostart instance into `crash-looping` after one slow post-update boot, each needing a manual
  Reset failed for a condition that had already resolved itself.

Preflight failures **are** counted, which is the whole reason D54 opens the row before preflight: a
model file that has been deleted fails at exit 72 forever, and without counted rows the supervisor
would retry it on backoff until the heat death of the universe instead of stopping after
`restart_max` and showing F4's re-download card.

**Internal-port reassignment (F5).** When a start closes with `exit_code=78`, the supervisor — not
the launcher — allocates the next free port from `[instances.internal_port_min,
instances.internal_port_max]` (skipping ports held by other instances and failing a live bind probe)
and writes `instances.internal_port` through the single store method `ReassignInternalPort`. That
write bumps `updated_at` and emits an event; it does **not** bump `generation` and does **not**
change `config_hash` (D52), so a concurrent `PATCH` is not spuriously rejected and no
`restart_required` badge appears for a change the user did not make. It is the first of the seven
named exceptions to "the API writes `instances`" in §2.8. If the whole pool is exhausted, the supervisor
stops retrying, sets `state='failed'` and raises the F5 notification instead of cycling.

**Health probe.** `GET http://127.0.0.1:<internal>/health` with a 2 s timeout drives
`starting → loading → ready`; `/props` on the first ready fills `ctx_size` and `slots_total`;
`/metrics` (when enabled) fills `requests_served` and is the authoritative source for instance-level
token totals.

On the **first** 200 for a run, in one transaction, the supervisor: stamps
`instance_starts.ready_at` on the open row (it does **not** write `outcome` — D63); copies that row's
`effective_config_hash` into `instance_status.applied_config_hash`, which is the only write of that
column anywhere (§2.8); sets `instance_status.state='ready'` and `ready_at`; and records the live
UUID→index map in `device_map_json`. Sixty seconds later, if the run is still alive, it stamps
`restart_window_reset_at` (D64).

**Version truth (D25).** On every pass, `readlink /proc/<MainPID>/exe` is resolved and compared with
`versions/active`; a mismatch is recorded in `instance_status.exe_version_id`, from which the derived
`stale_version` flag (§2.8) produces the badge "restart to apply llama.cpp b10621". The instance's
`state` is unchanged — it is still `ready` and still serving, which is the entire point of the flag
being derived rather than a state. The same readlink set is the GC guard: a version directory is
never deleted while any live process executes from it.

**Fit observation.** On the first `ready` transition — and only when `runtime_info.journal_read='ok'`
(§5.3, D77); otherwise the observation is skipped, no `fit_observations` row is written, and reports
stay `confidence: "modeled"` rather than being "calibrated" from an empty scan — the last 200 journal
lines are scanned for
llama.cpp's own buffer report (`load_tensors: … buffer size`, `llama_kv_cache: … size`,
`compute buffer size`) and its `--fit` projection; parsed values go to
`instance_status.fit_report_json` and a `fit_observations` row beside the prediction that was made
(§8.6). A load that ended in `cudaMalloc failed`/`out of memory` writes the same row with `oom=1`.

**Boot reconciliation**, in this order, before the loop starts:

1. **The single host-boot decision point.** Compare the `/proc/sys/kernel/random/boot_id` value and
   the `/proc/stat` `btime` instant that §11.1 step 9 read into memory against
   `runtime_info.host_boot_id`. If they differ, this is the first daemon start of a new host boot:
   apply the D53 coupling — `desired_state := autostart ? 'running' : 'stopped'` for every
   non-deleted instance, one event per change — set `host_boot_changed := true` for steps 3 and 4,
   and **only then** write the new `host_boot_id` and `host_boot_at`. If they match, `desired_state`
   is left exactly as it was, so a crash during daemon downtime is still repaired.

   **This is the only writer of `host_boot_id` and `host_boot_at` in the whole design**, and that
   exclusivity is the point rather than a style preference. §11.1 step 9 performs the same read
   during the boot sequence — it needs the answer for logging and for `runtime_info` display — but it
   writes **nothing**. An earlier reading had step 9 "record the new value" before step 10 started
   the supervisor, which meant this comparison always saw equality, the D53 coupling never fired, and
   autostart was broken in both directions: exactly the failure D53 exists to prevent, produced by
   the mechanism meant to detect it. Two readers, one writer, and the writer is the one that acts on
   the answer.
2. Read every managed unit's properties once and write `instance_status`, so a daemon restart never
   shows a stale "ready".
3. Close any `instance_starts` row left open by the previous daemon (writer table, §5.6) — including
   rows belonging to **soft-deleted** instances, which is why the reconcile set carries the
   open-row term (§3.10c) — and synthesize closed rows for any unit that failed with no row at all
   (exits 70 and 75 — §5.6).
   **Only when `host_boot_changed`**, relabel `external` starts as `autostart` under the three
   conditions above: `at ∈ [host_boot_at, boot_at)` and `autostart=1`.
4. Only then start the reconcile loop. Because step 1 has already agreed with what systemd did at
   boot, the first pass takes **no** corrective action on autostart instances — no start-then-stop
   flap, and no start of an instance whose unit is deliberately disabled.

**Per-instance VRAM and GPU attribution (D17).** The GPU sampler runs
`nvidia-smi --query-compute-apps=pid,gpu_uuid,used_gpu_memory --format=csv,noheader,nounits` and
joins on `MainPID`: the `pid` column gives `instance_status.vram_bytes` (summed across rows) and the
`gpu_uuid` column gives `instance_status.gpu_uuids_json` with
`gpu_attribution='measured'`. The `gpu_uuid` field is what makes the §10 bench exclusivity guard
implementable at all — a query of `pid,used_gpu_memory` alone returns no GPU identity and can never
answer "which GPU is this instance on". §8.6 defines the fallbacks when the field or the tool is
unavailable.

---

## 6. llama.cpp lifecycle manager

### 6.1 On-disk layout

```
<prefix>/llamaman                                root:root 0755 (D15); default /usr/local/bin,
                                                 ~<user>/.local/bin under --user-units
<prefix>/llamaman.prev                           the binary the last update replaced, same owner and
                                                 mode (D89). Written by the swap actor as
                                                 llamaman.prev.tmp and renamed into place; consumed
                                                 by the revert's rename; replaced wholesale by the
                                                 next update. Lives HERE, not under update/, because
                                                 (a) a root unit execs and installs it and this
                                                 directory is the one only root writes, and (b) the
                                                 install and the revert are then each ONE rename
                                                 inside ONE filesystem — update/ is under /var/lib
                                                 and <prefix> is usually under /usr/local, which is
                                                 EXDEV on a very common layout. Rollback depth is
                                                 exactly one (SPEC §5 assumption 6).
<prefix>/llamaman.new.tmp                        TRANSIENT: the swap actor extracts the new binary
                                                 from the tarball IT re-verified, here, and renames
                                                 it over <prefix>/llamaman. Opened O_TRUNC, so one
                                                 left by a killed actor is reclaimed by the next run
                                                 and never executed.
/etc/systemd/system/llamaman*.service|.target    installer-owned (or /etc/systemd/user in D2 mode)
/etc/polkit-1/rules.d/49-llamaman.rules          installer-owned (absent in D2 mode)

/var/lib/llamaman/                               0750, owned by the service identity. This is the
                                                 DEFAULT state directory, not a constant: under
                                                 --user-units it is $XDG_STATE_HOME/llamaman
                                                 (D72, §11.1 step 1). Everything below is relative
                                                 to runtime_info.state_dir.
├── llamaman.db                                  0600, WAL; all state and sealed secrets
├── secret.key                                   0600, 32 bytes, AES-GCM key for `secrets`
├── setup-token                                  0600, plaintext one-time token; unlinked on claim
│                                                (D59, §2.2a). Never in the DB, never in a backup.
├── llamaman.lock                                flock single-instance guard (F11)
├── versions/
│   ├── b10621-cuda-src/{bin,lib,manifest.json,build.log}
│   ├── v0.3.0-cpu-bin/{bin,lib,manifest.json}
│   ├── <id>.staging/                              TRANSIENT: every install — prebuilt (§6.4) and
│   │                                              source (§6.5) — lands here and is renamed into
│   │                                              place at publish, so no directory `versions/active`
│   │                                              can resolve into is ever written in place (D78)
│   ├── active   -> b10621-cuda-src                atomic symlink; the ONLY activation mechanism
│   └── previous -> v0.3.0-cpu-bin                 one-click rollback target
├── src/llama.cpp/                               one shared partial clone
├── build/<version_id>/                          git worktree + cmake build dir (kept on failure — D4)
├── logs/build/<version_id>.log                  durable copy of build output (F15)
├── db-backups/llamaman-<version>-<ts>.db        VACUUM INTO before every update (D14). Newest 7
│                                              kept, oldest first, and the NEWEST is never pruned:
│                                              a snapshot is written only just before an update and
│                                              is labeled with the version it replaces, so the
│                                              newest one is always the database as the version now
│                                              at <prefix>/llamaman.prev left it — the file step 3
│                                              of §12.4's downgrade procedure restores.
├── llamaman.db.superseded-<ts>                  written by `llamaman restore-db` before it replaces
│                                              the live database, so the discarded one is recoverable
│                                              by re-running the command against it; pruned after
│                                              30 days (§12.4).
├── update/{llamaman_<ver>_linux_<arch>.tar.gz, checksums.txt, checksums.txt.sig,
│           llamaman.new, pending}
│                                              §12.1. Everything here is the DAEMON's: it downloads
│                                              the three signed artifacts, extracts `llamaman.new`
│                                              only to run its version probe and unlinks it again,
│                                              and writes `pending`. **No privileged actor ever
│                                              installs anything from this directory** — the swap
│                                              actor extracts its own copy from the tarball it
│                                              re-verified, into <prefix> (D89) — and the retained
│                                              previous binary lives at <prefix>/llamaman.prev, not
│                                              here. `pending` is written by the daemon and deleted
│                                              by the confirmation gate; the scratch beside it is
│                                              cleared by that gate and again by the next
│                                              `POST /update/apply` before it stages, so nothing here
│                                              outlives the update that created it (§12.3).
├── tmp/                                         download staging
└── hf-cache/hub/                                ONLY in the --dedicated-user topology (§7.2 rule 4);
                                                 a standard HF hub layout, 0750, not our own format

<hub_dir>/                                       model bytes — the standard HF cache, never ours.
                                                 Usually $HF_HOME/hub; $HF_HUB_CACHE verbatim when
                                                 set; /var/lib/llamaman/hf-cache/hub for a
                                                 dedicated user (§7.2).
```

**No GGUF ever lives in Llama Man's private layout** (SPEC §3.2) — not under `versions/`, `src/`,
`build/`, `logs/`, `db-backups/`, `update/` or `tmp/`. A second copy would double disk usage and
desynchronize from every other HF-aware tool. The `--dedicated-user` topology is not an exception to
that rule: `hf-cache/hub` is the *standard* hub layout, sitting beside the state directory rather
than inside our own structures, and `HF_HOME=/var/lib/llamaman/hf-cache hf download …` sees exactly
what Llama Man sees. Putting it there is what makes the locked-down option work at all — a nologin
system account whose `$HOME` is the state directory has nowhere else that both exists and is writable
(§7.2 rule 4).

A version's binaries are always invoked through the path **resolved at start time**, so flipping the
symlink never affects a running process — its executable and its `lib/` are already mapped.

### 6.2 Version identity and channel resolution

A version's id is `<tag>-<backend>-<acq>` where `acq` is `bin` for a prebuilt tarball and `src` for a
source build (`b10621-cuda-src`, `b10621-cpu-bin`, `v0.3.0-cpu-bin`, `fork-a1b2c3d-cuda-src`), and it
is also the directory name. **All three axes are load-bearing (D60):**

- *backend*, because the same upstream tag can exist as a CPU download and a CUDA build on one host,
  and rollback must be able to name either;
- *acquisition*, because D18's fallback — the single mechanism behind SPEC §3.7's distro-agnostic
  promise — enqueues a **source** build of the same tag and the same backend whose prebuilt just
  failed `failed_verification`. With a two-part id that insert collides with the row it is supposed
  to replace. With three, `b10621-cpu-bin` stays as the failed record, `b10621-cpu-src` is built,
  and `superseded_by` links them so the UI can say "prebuilt rejected (requires GLIBC_2.38, host has
  2.36) — built from source instead" in one line.

`UNIQUE(tag, backend, acquisition)` states the same thing in the schema, so an implementer cannot
reintroduce a two-part key by accident. The UI never shows the raw id when it can help it: it renders
"b10621 · CUDA · source" from the three columns.

**A three-part identity is a constraint, so what happens when it collides is part of the contract
(D71).** `POST /api/v1/llamacpp/versions` resolves its request to an id *before* it inserts anything,
and then follows exactly one of five branches. Nothing here is left to the database's default
behavior, because "`UNIQUE` constraint failed" is not a user-facing answer:

| existing row with that id | outcome |
|---|---|
| none | insert `pending`, enqueue `llamacpp_install`, `202` |
| live (`pending`…`verifying`) | `409 build_in_flight` with the existing `job_id` and version id. An `Idempotency-Key` replay inside its window returns that job with `200` instead (D39) |
| `ready`, and the requested build options match the stored `build_options_json` / `cuda_arch_list` | `200` with the existing row and `"reused": true`. Nothing is rebuilt; the UI says "already installed" and offers Activate |
| `ready`, and the requested options **differ** | `409 version_options_differ`, `details` naming each differing option with both values, and the hint that `force_rebuild: true` replaces the installed build in place |
| `failed`, `failed_verification`, `canceled` or `deleted` | **reuse-and-reset**: the same row returns to `pending` per the §2.5 transition table and a fresh job is enqueued. This is what makes "the prebuilt failed verification last week, try again on a newer host" and "retry after installing `nvcc`" ordinary operations rather than dead ends |

`force_rebuild: true` on a `ready` row takes the reuse-and-reset branch as well, after refusing when
the row is `is_active=1` **and** any instance is currently executing out of its directory (D25) —
rebuilding a directory a live process is running from is the one case that must be a `409
version_in_use` rather than a rebuild.

**Rebuilding the *active* id needs one more rule, because `versions/active` still points into it
(D78).** Activation is symlink-only and nothing re-points it, so a forced rebuild of the active
version with no instances currently running would otherwise reinstall into the exact directory
`versions/active` resolves to. An instance started during that window runs §5.6 step 5 against a tree
in the middle of `cmake --install` — exit 69 at best, an `execve` of a half-written `llama-server` at
worst. The `versions/<id>.staging` + `rename` protocol §6.4 already uses for prebuilts is therefore
applied to the source `install` phase too, and extended to cover a target that already exists:

- Every install, prebuilt or source, writes to `versions/<id>.staging` and is renamed into place at
  `publish`. For a fresh id that is one atomic `rename` into a non-existent target, exactly as today.
- For an id whose directory exists — a forced rebuild — `publish` re-checks the D25 live-process
  guard and then swaps: `rename(versions/<id> → versions/<id>.old)`,
  `rename(versions/<id>.staging → versions/<id>)`, remove `.old`. `versions/active` names `<id>` and
  is never touched, so it is correct before and after; the only window in which it dangles is between
  the two renames.
- That window is closed from the other side by data rather than by timing: the row is `pending` …
  `building` … `verifying` for the whole rebuild, and both the supervisor (§5.8) and `instance-exec`
  (§5.6 step 5) refuse to start an instance while the `is_active=1` row is not `ready`. The launcher's
  refusal is exit 69 `runtime_rebuilding`, and the supervisor starts the instance by itself once the
  row returns to `ready`, so nothing needs a user.

`force_rebuild` on the active id therefore stays the ordinary operation D71 made it, and the fleet is
paused rather than exposed for the minutes it takes.

**Two forks at the same short SHA are two versions.** A custom build's tag is
`fork-<urlhash6>-<short>`, where `<urlhash6>` is the first six hex characters of
`sha256(normalized git_url)` (scheme and any `.git` suffix stripped, host lowercased) and `<short>`
is the first seven of the resolved commit. Without the URL discriminator two unrelated forks that
happen to share a seven-hex prefix — or the same commit fetched from a mirror — collide on
`UNIQUE(tag, backend, acquisition)` and the second one silently reuses the first one's build. The UI
shows the git URL beside the tag for every `channel='custom'` row, so the hash never has to be read
by a human.

| Channel | Resolution |
|---|---|
| stable | `GET /repos/ggml-org/llama.cpp/releases/latest` → semver tag (e.g. `v0.3.0`); then fetch that release's `nightly-tag.txt` asset → the pinned `b#####`, which is what is actually fetched or built. The UI shows both ("v0.3.0 — build b10621"). |
| nightly | `GET /releases?per_page=50`, keep `prerelease && tag ~ ^b\d+$`, sort by numeric build id descending, show the last ~30 with dates. |
| custom | User-supplied git URL + ref (tag, branch, or 40-hex commit); `git ls-remote` validates it before the row leaves `resolving`, and the ref is resolved to a concrete SHA so "rebuild the same thing" is reproducible. No GitHub API involved. |

GitHub calls go through one client with `If-None-Match` conditional requests persisted in
`release_cache.etag`, served stale-while-revalidating. Unauthenticated api.github.com allows 60
requests/hour/IP, which a UI that polled the release list would exhaust in an afternoon; an optional
user-supplied GitHub token raises that to 5000/hour.

**That token is reachable, not merely storable.** `secrets` enumerates `'github_token'`, so it needs
the same three endpoints the HF token has — `GET`/`PUT`/`DELETE /api/v1/github/token` (§3.6) — a
place in the UI (`/settings → Builds`, beside the channel and build-jobs fields), and a validation
rule. `PUT` validates by calling `GET https://api.github.com/user` with the presented token and
storing it sealed only on `200`, recording `hint` as `ghp_…AbC` and `scope_json` from the
`x-oauth-scopes` response header; a `401` is `422 github_token_invalid`. The token is optional
everywhere: with none present the client stays anonymous and the release list is served
stale-while-revalidating from `release_cache`, and `GET /api/v1/llamacpp/releases` reports
`"rate_limit":{"remaining":N,"reset_at":…,"authenticated":false}` so "why is the nightly list stale"
has an answer on screen. It is never used for anything but api.github.com, and never sent to
raw.githubusercontent.com or to a release-asset CDN redirect (the same cross-host header strip §7.1
applies to the HF token).

### 6.3 Acquisition decision (`GET /api/v1/llamacpp/plan`)

```
backend == "cuda"                                  → source build (no Linux CUDA prebuilt exists)
backend == "cpu" && channel != "custom"
   && asset llama-<tag>-bin-ubuntu-<asset_arch>.tar.gz exists
   && settings.llamacpp.prefer_prebuilt_cpu        → prebuilt, subject to the D18 acceptance test
otherwise                                          → source build
```

**`<asset_arch>` is upstream's vocabulary, not Go's, and the mapping is pinned here** because getting
it wrong is silent: `llama-b10621-bin-ubuntu-amd64.tar.gz` simply does not exist, the asset lookup
misses, and every CPU install falls through to a source build that nobody asked for.

| `runtime.GOARCH` / `uname -m` | release-artifact arch (`llamaman_<ver>_linux_<x>.tar.gz`, §16.2) | llama.cpp **asset** arch |
|---|---|---|
| `amd64` / `x86_64` | `amd64` | **`x64`** |
| `arm64` / `aarch64` | `arm64` | `arm64` |
| `s390x` | — (not built) | `s390x` |

One function, `llamacpp/github.AssetArch(goarch string) (string, bool)`, owns the mapping; it is
table-tested, and `nightly.yml` re-resolves the live nightly tag and asserts that the name it
constructs is actually present in the release's asset list — so upstream renaming an asset is caught
by CI rather than by a user's afternoon.

The plan endpoint returns the decision **with its reason, the detected CUDA architectures, the
missing toolchain items and a free-space check** before the user commits. That is the difference
between "build failed after four minutes" and "install cmake first".

### 6.4 Prebuilt pipeline

1. **download** — the asset into `tmp/` with the same resumable downloader as models (§7.4);
   SHA-256 computed inline and compared with GitHub's digest when present.
2. **extract** — into `versions/<id>.staging/` with a hardened tar reader that rejects absolute
   paths, `..` traversal, symlinks escaping the root, and device nodes, and strips the archive's
   top-level directory. `chmod +x bin/*`.
3. **verify (D18)** — run `bin/llama-server --version` **on this host**; it must exit 0. If it does
   not, `debug/elf` parses `.gnu.version_r` from the ELF and compares required `GLIBC_*` versions
   against the host's, producing "requires GLIBC_2.38, host has 2.36" instead of a raw loader
   error. The row (`<tag>-cpu-bin`) goes to `failed_verification` and is **kept**; a *new* row
   `<tag>-cpu-src` is inserted with `acquisition='source'`, the glibc diagnosis carried into its
   `params_json`, and the two linked by `superseded_by`. The three-part identity of D60 is what makes
   this insert possible at all — with a `<tag>-<backend>` primary key the fallback would collide with
   the very row it replaces. This is what makes SPEC §3.7's distro-agnostic promise true, and it
   costs one stdlib package.
   `llama-bench --version` is *not* required to exit 0 — llama-bench has its own argument parser and
   an unrecognized flag exits non-zero; its presence is asserted by `stat` plus a `-h` run whose
   output must mention `llama-bench`.
4. **install** — `rename(versions/<id>.staging → versions/<id>)` for a fresh id, or the guarded
   two-rename swap of §6.2 (D78) when that directory already exists, write `manifest.json` including the
   verbatim `bin/llama-server --help` capture, parse that capture into
   `llamacpp_versions.help_flags_json` and derive `supports_fit` from whether it mentions `--fit`
   (§2.5), → `ready`. The two columns are what keep `RenderArgv` pure (§5.7): the renderer reads rows,
   never `manifest.json`.

### 6.5 Source-build pipeline

Each step is a named phase written to `jobs.progress_json`, prefixed in `build.log`, and recorded in
`failing_step` on error.

| phase | action |
|---|---|
| `preflight` | `internal/toolchain` probes git, cmake (≥ 3.14), ninja (optional), cc/c++, ccache (optional), nvcc, driver, compute capabilities. Missing pieces abort here with per-distro guidance and **never** a package-manager call. |
| `space` | require ≥ 12 GiB free for CUDA, ≥ 3 GiB for CPU. |
| `fetch` | first run `git clone --filter=blob:none --no-checkout <url> src/llama.cpp`; later `git remote set-url` + `git fetch --tags --prune`. Then `git worktree add build/<version_id> <sha>` and `git submodule update --init --recursive`. Partial clone plus worktrees means the second build of the day downloads only new objects. |
| `configure` | see below |
| `compile` | `cmake --build build/<version_id>/build -j N` (D20) |
| `install` | `cmake --install …` into **`versions/<version_id>.staging`**, never into `versions/<version_id>` (D78); assert `bin/llama-server`, `bin/llama-bench`, `bin/llama-cli`, `lib/` |
| `verify` | D18 + D19, run against the staging tree |
| `publish` | write `manifest.json` (with the `llama-server --help` capture), fill `help_flags_json` and `supports_fit` from it (§2.5, §5.7), then **rename the staging tree into place** — one `rename` for a fresh id, and for a rebuild of an existing id the guarded two-rename swap of §6.2 (D78) — remove the worktree and cmake dir, release the build lease, → `ready` |

```sh
cmake -S build/<version_id> -B build/<version_id>/build -G Ninja \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_INSTALL_PREFIX=<state_dir>/versions/<version_id>.staging \
  -DLLAMA_BUILD_SERVER=ON -DLLAMA_BUILD_TOOLS=ON \
  -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
  -DLLAMA_CURL=OFF -DBUILD_SHARED_LIBS=ON -DGGML_NATIVE=ON \
  -DCMAKE_INSTALL_RPATH='$ORIGIN/../lib' -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON \
  [cuda:] -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES="86;89" \
  <settings.llamacpp.extra_cmake_flags> <request.cmake_extra…>
```

- **`-DLLAMA_BUILD_TOOLS=ON` (D23)** — `llama-bench` lives under `tools/` upstream; without this
  flag the headline SPEC §3.5 feature simply is not built.
- **`-DBUILD_SHARED_LIBS=ON` with the install RPATH (D22)** — without
  `BUILD_WITH_INSTALL_RPATH=ON`, CMake strips the build RPATH at install time and the installed
  `llama-server` cannot find `libggml*.so` in the versioned tree. With it, each `versions/<id>/` is
  fully self-contained and relocatable, which is exactly what makes symlink activation and rollback
  safe and lets the launcher set no `LD_LIBRARY_PATH`.
- **`-DLLAMA_CURL=OFF`** — SPEC §3.2 forbids `-hf` model fetching, so libcurl would be a build-time
  system dependency bought for a feature we never use, and it is a common source of source-build
  failures on minimal hosts.
- **`GGML_NATIVE=ON` on both backends**, because the build host is the run host. The host CPU flag
  set is recorded in `llamacpp_versions.host_cpu_flags`; if it later changes (state directory moved
  to another machine) the UI raises "rebuild recommended" rather than letting an illegal-instruction
  crash explain it.
- **`CMAKE_CUDA_ARCHITECTURES` from detection (D21)** — the compute capabilities of the GPUs present,
  recorded with their UUIDs in `manifest.json`. `all` multiplies compile time; `native` silently
  produces a binary that will not run if the GPU set changes. When the live GPU set no longer
  matches the manifest, a "rebuild recommended" notification is raised.
- Ninja when present, else Unix Makefiles. `ccache` is used when detected
  (`-DCMAKE_C_COMPILER_LAUNCHER=ccache -DCMAKE_CXX_COMPILER_LAUNCHER=ccache`), which makes rebuilds
  of nearby commits fast.

**Parallelism (D20).** `N = min(NumCPU, max(2, MemAvailableGiB/2))` unless
`settings.llamacpp.build_jobs` overrides it. If a compiler process is killed by the OOM killer
(detected by signal 9 on a child plus a matching `oom-kill` line in the kernel log), the build is
retried **once automatically at `-j1`** and the UI states why. That retry converts the most common
workstation build failure from a hard error into a slow success.

**Durability (D4).** Builds are daemon child processes in their own process group; there are no
transient units and therefore no transient-unit polkit grant. If the daemon restarts mid-build the
job becomes `interrupted`, the build directory is **kept**, and `POST …/retry` re-runs
`cmake --build` against the warm object cache — minutes, not a full CUDA rebuild. While a build is
live, both `POST /api/v1/update/apply` and `POST /api/v1/system/restart` (the UI-initiated daemon
restart defined in §3.3) return `409 job_in_flight`, and `idx_jobs_one_live_per_subject` keeps
`interrupted` rows exclusive.

**Log streaming.** `procx` merges stdout and stderr line by line into (a) `logs/build/<id>.log`,
(b) `versions/<id>/build.log`, and (c) an in-memory ring of the last 5000 lines with a broadcast
channel. `GET …/log` serves the file for backfill and the channel for follow. Progress is parsed
from Ninja's `[812/1930]` counters into `jobs.progress_json.pct`.

**Failure attribution.** Each phase records its exit code; the log viewer scrolls to the first line
matching `error:` / `CMake Error` / `fatal:`, whose line number is stored. Known failures get
actionable hints: `nvcc fatal: Unsupported gpu architecture` → "set CUDA architectures in Settings →
Builds"; OOM kill → "reduced to -j1 automatically; see the retry below"; missing `nvcc` →
per-distro package names.

**Cancellation.** `jobs.cancel_requested=1` → context cancel → SIGTERM the process group → SIGKILL
after 10 s → remove the worktree and the partial `versions/<id>.staging` directory.

**Concurrency (D70).** One build at a time, enforced by the **`build_lease` singleton** of §2.3 — not
by `idx_jobs_one_live_per_subject`, which is scoped to `(subject_type, subject_id)` and therefore says
nothing at all about two `llamacpp_install` jobs on two *different* version ids. That distinction is
not academic: a daemon restart during two queued builds leaves two `interrupted` rows (D4), and two
Retry clicks would have started two CUDA compiles side by side on a host whose §6.5 `space` phase
sized free disk for one and whose D20 parallelism sized memory for one. The in-process mutex is kept
as a cheap first gate but is explicitly **not** the guarantee: it is per process, and the second
builder can be the *next* boot. The worker acquires the lease in the same transaction that moves its
job to `running`, refreshes `expires_at` on every progress write, and releases it on completion,
cancellation, and at boot for any row whose `owner` is not the current `boot_id`. A build that cannot
take the lease stays `queued` with `run_after = now + 15 s`, and the UI shows it as "waiting for the
running build" — a queue, which is what a user expects, rather than a 409.

### 6.6 Activation, canary rolling restart, rollback

`POST /llamacpp/versions/{id}/activate` — one transaction, then one filesystem operation, then an
optional roll:

1. Guard: target is `ready`; refuse while another activation is live, and refuse while a bench is
   live — where "a bench is live" is now a single well-defined fact rather than an inference: the
   `bench_lease` singleton is held (`job_id IS NOT NULL AND expires_at > now`), **or** any `bench_runs`
   row has `restore_done=0 AND stopped_instances_json IS NOT NULL` (D75, §10). Both terms are needed:
   the lease covers a bench that is executing, the second covers one that has stopped production
   instances and not yet put them back.
2. Previous active → `is_active=0, previous_active=1`; the row that *was* `previous_active` has its
   flag cleared and is **recorded in the activation job's `params_json` as the deletion candidate** —
   the `llamacpp_delete` job is enqueued only when the activation job reaches `succeeded`, i.e. after
   the canary has passed (or after a `restart_instances:"none"` activation commits). Enqueuing it here
   was a bug of ordering: step 5 may revert the whole activation, and it cannot revert a version
   directory that a delete worker has already removed. `llamacpp.keep_previous` is a **bool** (SPEC §5.6 asks
   for "exactly the previous build"), and it selects between exactly two behaviors:
   - `true` (default) — one rollback target is retained, as above.
   - `false` — the outgoing active version is queued for deletion on the same "after the job
     succeeds" schedule, no row is marked `previous_active`, `versions/previous` is removed, and
     `POST /llamacpp/rollback` returns `409 no_rollback_target`. The version list and the update
     dialog both say "rollback disabled" while it is off — and the dialog additionally warns that a
     failed canary can then only be reverted onto whatever other `ready` version exists, because the
     build being replaced is scheduled for deletion.

   There is deliberately no depth > 1: the schema cannot express it (`previous_active` is a 0/1
   column under a unique index, and `versions/previous` is one symlink), so a tunable integer would
   be a setting whose values 0 and 2 mean nothing. If multi-depth retention is ever wanted, it is a
   migration that drops `idx_llamacpp_one_prev` for an ordered `retired_at` list — not a settings
   change.
3. Target → `is_active=1, activated_at=now`, **and in the same transaction
   `instances.RecomputeConfigHash` for every non-deleted instance (D69, §2.8)**. `config_hash` folds
   in the active version id (D52), so a flip changes that input for every instance at once; without
   this step the stored hash silently disagrees with its own definition, `restart_required` never
   lights, the "restart to apply llama.cpp b10621" prompt never appears, and comparing it with
   `applied_config_hash` stops carrying information. The write does not bump `generation` (nobody
   edited a configuration) and does not touch `applied_config_hash` (nothing has re-loaded yet) —
   which is precisely how every running instance acquires the `restart_required` badge the moment
   activation commits, and how the canary roll's progress stays legible as "4 of 7 still on b10604".
   The same recomputation runs for `POST /llamacpp/rollback`, which is this routine with
   `previous_active` as the target.
4. `symlink(versions/<new>, versions/.active.tmp)` then `rename(.active.tmp, versions/active)` —
   atomic, so there is no instant at which `active` is missing. `versions/previous` is written the
   same way.
5. **Canary roll (D24)**, when `restart_instances="rolling"`: order running instances (user-chosen
   canary first, else creation order); each restart stamps `instances.pending_trigger='rolling'`
   before `StartUnit`, so the ledger names the roll (§5.8). Restart the first and wait for `/health`
   within `instances.start_timeout_sec`.

   **On canary failure the activation is reverted, and the revert is a database write first and a
   filesystem operation second** — in that order, because §6.6's own boot reconciliation makes the row
   win over the symlink, so flipping `versions/active` back while leaving `is_active=1` on the failed
   build would merely defer the problem to the next daemon start, which would re-point the symlink at
   it and restart the whole fleet onto it. The revert is therefore the mirror image of steps 2–4, in
   one transaction and then one filesystem step:
   - restore `is_active`/`previous_active` to their pre-activation values on both rows;
   - re-run `instances.RecomputeConfigHash` for **every non-deleted instance** (D69), which is what
     clears the `restart_required` badge that step 3 raised across the fleet — without it every
     instance would wear a permanent "restart to apply" prompt against a version that was rolled back,
     since `applied_config_hash` was never touched and the stored hash would still fold in the
     abandoned version id;
   - no `llamacpp_delete` is enqueued, and the candidate recorded in step 2 is dropped: the outgoing
     build is the build we just went back to;
   - then repair `versions/active` and `versions/previous` from the restored rows by the same
     `symlink` + `rename` pair, restart the canary onto the old build, abort the rollout without
     touching any other instance, close the activation job `failed`, and raise a notification carrying
     the captured journal tail (which is available when `runtime_info.journal_read='ok'` — §5.3 — and
     is otherwise replaced by the F23 hint).

   On canary success, proceed one instance at a time, each gated on `/health`; a later failure stops
   the roll, leaves the remainder untouched, and reports "3 of 7 migrated" rather than continuing
   silently. A *later* failure does **not** revert: by then instances are serving on the new build,
   and reverting under them is a second unplanned restart of everything that already worked. The
   canary exists precisely so that the one instance whose failure is cheap to undo is the one that
   decides.
6. Rollback (`POST /llamacpp/rollback`) is the identical routine with `previous_active` as target,
   including the revert path — a rollback whose own canary fails goes back to where it started.

**Garbage collection (D25).** Only `active` and `previous` are retained. Before deleting any version
directory, `readlink /proc/<MainPID>/exe` is resolved for every running instance and any directory
in that set is skipped, with the derived `stale_version` badge shown instead. Database bookkeeping
alone is not trusted for this. When `llamacpp.keep_previous` is false, `previous` does not exist and
only `active` (plus anything a live process is executing) is retained.

**Boot reconciliation, which is also the `llamacpp_activate` finalizer (§2.3).** If `versions/active`
disagrees with the `is_active` row, the **row wins** and the symlink is repaired from it; the same
holds for `versions/previous` and `previous_active`. The DB is the source of truth; the symlink is
the mechanism. Then any `llamacpp_activate` job left `interrupted` by the restart is closed here, and
the row decides how:

- the row's `is_active=1` version **is** the job's target → the step-3 transaction committed, the
  activation is complete, and only a canary roll nobody is waiting on was lost. Close the job
  `succeeded`, enqueue the `llamacpp_delete` for the candidate named in `params_json` if one is due,
  and raise a notification offering the rolling restart that did not finish (`restart_required` is
  already true on every running instance, since step 3 recomputed the hashes).
- it is **not** the target → the transaction never committed and nothing happened. Close the job
  `failed` with `error_code='daemon_restarted'`; no version state changed and no delete is due.

Either way the version rows stay `ready`, which is what §2.3a's activate column asserts, and no
reading of the boot state re-activates a build whose canary failed — because step 5 reverted the row
before the daemon ever went down.

---

## 7. Hugging Face client

### 7.1 Endpoints used

| Purpose | Request |
|---|---|
| Search | `GET {endpoint}/api/models?filter=gguf&search=&sort=downloads&direction=-1&limit=30&cursor=` |
| Model metadata | `GET {endpoint}/api/models/{repo}` → `sha`, `gated`, `private`, `siblings`, `tags`, and HF's server-computed `gguf` object (architecture, context_length, totals) |
| File tree, true sizes | `GET {endpoint}/api/models/{repo}/tree/{rev}?recursive=1&expand=1` → per entry `path`, `size`, `lfs:{oid,size}` |
| Card | `GET {endpoint}/{repo}/raw/{rev}/README.md` |
| File bytes / header peek | `GET {endpoint}/{repo}/resolve/{rev}/{path}` with `Range`; `HEAD` first for `etag`, `x-linked-etag`, `x-linked-size`, `x-repo-commit` |
| Token validation | `GET {endpoint}/api/whoami-v2` |

**True size is always `lfs.size` when the entry has an `lfs` object, never the top-level `size`** —
for LFS files the plain `size` can be the ~130-byte pointer, which would make a 40 GB model look
free and break the fit calculator outright (SPEC §3.2 calls this out).

Auth: `Authorization: Bearer <hf_token>` when a token is stored. **The header is stripped on
cross-host redirects** — HF redirects file downloads to a CDN with its own signed URL — by a custom
`CheckRedirect` that compares hosts. User-Agent:
`llamaman/<version> (+https://github.com/jlbyh2o/llamaman)`. The token is never logged and never
returned by the API (masked to `hf_…AbC`).

Gated repos: metadata succeeds while `resolve` returns 401/403 with `x-error-code: GatedRepo`, which
maps to `hf_gated`; `RepoNotFound` on an existing-looking repo with no token means "private, sign
in". Robustness: one shared `http.Client` with `Timeout: 0` (streams) and per-request contexts,
`MaxIdleConnsPerHost: 8`, retry on 429/5xx honoring `Retry-After` with jittered exponential backoff
(max 5 tries), one client-side limiter so a bulk metadata refresh cannot starve a user-initiated
search, and a 30-minute TTL cache for search/tree/metadata.

### 7.2 Cache layout — read, write, scan

The layout is `huggingface_hub`'s, which is the entire point:

```
<HF_HOME>/hub/
├── .locks/<repo_folder>/<etag>.lock       flock — the SAME lock huggingface_hub takes (D27)
└── models--{org}--{name}/
    ├── refs/<branch>                      file containing the commit sha
    ├── blobs/<etag>                       content; for LFS objects <etag> == sha256 hex
    ├── blobs/<etag>.incomplete            in-progress download, resumable by either tool (D26)
    ├── snapshots/<commit>/<path>          relative symlink -> ../../blobs/<etag>
    └── .no_exist/<commit>/<path>          negative cache; we read it, never write it
```

**Detecting the hub directory.** This runs once at first boot and is then persisted and editable.
The chain resolves a **hub directory**, not an `HF_HOME`, because `huggingface_hub` lets the two come
apart — and the case where they do is exactly the case this product exists for (a multi-terabyte
model disk):

| # | source | hub directory | `detected_from` |
|---|---|---|---|
| 1 | `$HF_HUB_CACHE` | the value **verbatim** — it overrides `<HF_HOME>/hub` outright, with no `/hub` appended | `HF_HUB_CACHE` |
| 2 | `$HUGGINGFACE_HUB_CACHE` (legacy), then `$TRANSFORMERS_CACHE` (legacy) | verbatim, same rule | `legacy_env` |
| 3 | `$HF_HOME` | `$HF_HOME/hub` | `HF_HOME` |
| 4 | the service identity's home **is** the state directory — the `--dedicated-user` topology (§13 step 4) | `<state_dir>/hf-cache/hub` | `dedicated_user` |
| 5 | `$XDG_CACHE_HOME` | `$XDG_CACHE_HOME/huggingface/hub` | `XDG_CACHE_HOME` |
| 6 | — | `$HOME/.cache/huggingface/hub` | `default` |

The first entry that names an existing directory, or (when none exists) the first entry at all, wins;
the chain is evaluated top to bottom exactly once and the winner becomes the primary
`hf_cache_roots` row. Every *other* entry that names an existing directory containing at least one
`models--*` is registered as a **non-primary, scan-and-serve** root, so a user who once used
`HF_HOME` and later moved to `HF_HUB_CACHE` sees both libraries on first boot rather than half of
one. Environment variables are honored for **detection only** and are never required (SPEC §3.9);
the resolved paths live in `hf_cache_roots` and are edited in the UI thereafter.

**Nothing may assume the `/hub` suffix.** `hf_cache_roots.path` is the hub directory itself, and rule
1 produces one that does not end in `/hub`. `settings['hf.hub_dir']` is therefore the authoritative
setting; `settings['hf.home']` and `runtime_info.hf_home` are courtesy projections that are empty
whenever the suffix is absent, and the Storage form renders the hub directory as the editable field
with `HF_HOME` shown beneath it only when one exists (§7.2a).

**Rule 4 keeps §6.1's promise honest.** `--dedicated-user` creates a system account whose home *is*
`/var/lib/llamaman`, so rules 5–6 would resolve to `/var/lib/llamaman/.cache/huggingface/hub` — a
different directory from the `/var/lib/llamaman/hf-cache` §5.2 advertises, and one that buries GGUFs
in a path §6.1 says never holds them. Rule 4 removes the ambiguity by detecting the topology from a
fact the daemon can observe (`$HOME == state_dir`) rather than from an environment variable SPEC §3.9
forbids requiring. `install.sh --dedicated-user` pre-creates `/var/lib/llamaman/hf-cache/hub` mode
0750 owned by the account so the first boot finds it already there. §6.1's assertion is restated
precisely: **no GGUF ever lives in Llama Man's private layout** — not under `versions/`, `tmp/`,
`db-backups/` or `update/`. In the dedicated-user topology the HF cache is rooted *beside* them at
`/var/lib/llamaman/hf-cache`, in the standard hub layout, readable by any HF-aware tool pointed at
it — which is the property SPEC §3.2 actually asks for, and which a `~/.cache` path under a nologin
system account would not have delivered any better.

### 7.2a One cache location, four representations, one writer

The cache location is visible in four places, and it would drift in a week if they were four
independent facts. They are not: `hf_cache_roots` is the **authority**, and the rest are projections
of its `is_primary=1` row.

| representation | role | written by |
|---|---|---|
| `hf_cache_roots` rows | every known hub directory, with `is_primary`, `writable`, `symlinks_ok`, `detected_from`, free space | `cache.SetPrimaryRoot` / `cache.AddRoot` / `cache.DetachRoot` |
| `settings['hf.hub_dir']` | the **primary hub directory**, verbatim — the authoritative setting, and what the UI's Storage form edits | `cache.SetPrimaryRoot`, never directly |
| `settings['hf.home']` | courtesy projection: `hub_dir` minus a trailing `/hub`, else `""` | `cache.SetPrimaryRoot`, never directly |
| `runtime_info.hf_hub_dir` / `hf_home` | display copies for `llamaman status` / `doctor`, which must not make an HTTP call | `cache.SetPrimaryRoot` and boot |

**`cache.SetPrimaryRoot(hubDir)` is the only writer of all four**, and it is what
`PATCH /api/v1/settings {"hf.hub_dir":…}`, `POST /api/v1/cache/roots/{id}/promote` and the wizard's
`hf` step all call. In one transaction it: validates the path (absolute, not under a
`ProtectSystem=full` prefix, creatable, writable by the service identity, symlink probe — F17);
upserts an `hf_cache_roots` row for that **hub directory exactly as given**, with
`detected_from='setting'|'manual'`; clears `is_primary` on the old row and sets it on the new; writes
`settings['hf.hub_dir']` and the derived `settings['hf.home']`; refreshes `runtime_info.hf_hub_dir`
and `hf_home`; and enqueues a `cache_scan` job for the new root. It emits one event and returns
`restart_required: false` — nothing about the change touches a unit file (D57) or the running
listener set.

`PATCH /settings {"hf.home": X}` remains accepted as a convenience for anyone who thinks in
`HF_HOME`: it is translated to `SetPrimaryRoot(X + "/hub")` and answered with the resulting
`hub_dir`. There is no path by which the two settings can disagree, because only one of them is
stored as an input.

The remaining questions the two representations left open, answered:

- **The old root is kept, not dropped.** It becomes a non-primary root: its `models` rows keep their
  `root_id`, keep resolving to real files, and keep serving instances that reference them. Nothing is
  moved, copied or deleted — relocating the cache is a statement about *where new downloads go*, not
  a migration. The UI says exactly that.
- **`models.root_id` is never rewritten** by a relocation. A model's row belongs to the root whose
  filesystem actually holds it, forever, and the only thing that changes it is a rescan discovering
  the same repo+revision under a different root (which produces a second row, since
  `UNIQUE(root_id, repo_id, revision, primary_file)` scopes identity by root).
- **Only the primary root receives downloads.** `POST /api/v1/downloads` always writes to
  `is_primary=1`; there is no `root_id` parameter, because "which disk did that 40 GB go to" is a
  question a user should never have to answer twice. Non-primary roots are scan-and-serve only, and
  `writable=0` roots are additionally excluded from ever being promoted (`422 root_not_writable`).
- **Detaching** (`DELETE /cache/roots/{id}`) refuses the primary (`409 root_is_primary`) and refuses
  a root any of whose models is referenced by **any `instances` row, soft-deleted ones included**
  (`409 model_in_use`, naming them and marking which are deleted, with `?purge=true` on those
  instances as the stated remedy). Otherwise the row is deleted, its
  `models`/`model_files`/`stray_files` rows cascade away, and **no file is touched**.

  **The soft-deleted instances are not a courtesy in that guard; they are the whole of it.** This is
  the one operation in the design that issues a SQL `DELETE` against `models` — `models.root_id` is
  `ON DELETE CASCADE`, so the rows go — and `instances.model_id` is `ON DELETE RESTRICT`. Under D68 a
  soft-deleted instance keeps its row *and* its `model_id`, deliberately, so the history stays
  readable (§3.10c). A guard phrased over non-deleted instances therefore passes, and the transaction
  then fails inside SQLite with a raw foreign-key violation instead of the documented 409. §7.2's
  "deleting a model never issues a SQL `DELETE`" is true of the *model* delete path and only of it;
  this path is the exception, and it is why this guard counts every referencing row.
- **The wizard's "cache-root detection and confirmation" step** calls `SetPrimaryRoot` with whatever
  the user confirms — the same method, so there is no wizard-only path that could write one
  representation and forget another.

**The lock path (D27) is built by exactly one function**, `cache.LockPath(hub, repoID, etag)`
returning `<hub>/.locks/<repo_folder>/<etag>.lock`, with `<repo_folder>` the same
`models--{org}--{name}` form. A test asserts the constructed path against a fixture tree, and the
optional CI job that runs the real `hf` CLI asserts mutual exclusion. Locking the `.incomplete` file
itself, or a `.lock` inside the repo directory, interlocks with nothing — and SPEC §3.2's
one-shared-cache promise is only as true as this path.

**The lock *primitive* is half the contract, and it is `flock(2)`.** A correct path taken with the
wrong syscall interlocks with nothing at all, and would still pass the fixture-tree path test — so it
is pinned here:

- **Syscall**: `flock(2)`, via `golang.org/x/sys/unix.Flock(fd, LOCK_EX|LOCK_NB)`. This is what
  `huggingface_hub` takes: its `WeakFileLock` is built on `filelock`, whose Unix backend calls
  Python's `fcntl.flock` — which is `flock(2)`, **not** `fcntl(F_SETLK)`. POSIX record locks and BSD
  file locks are entirely independent mechanisms in the kernel: a process holding one does not block
  a process taking the other, on the same file, ever. `internal/hf/cache` therefore never calls
  `fcntl` locking, and a lint rule says so.
- **File handling**: the lock file is opened `O_CREAT|O_RDWR` mode 0644 (other tools run as the same
  user must be able to open it), the `.locks/<repo_folder>/` directory is created 0755, and the file
  is *not* removed on release — `huggingface_hub` leaves it too, and unlinking a lock file under a
  concurrent acquirer is a classic race. The nightly `maintenance` pass removes `.lock` files older
  than 7 days with no holder.
- **Acquisition policy**: try `LOCK_EX|LOCK_NB` first. On `EWOULDBLOCK`, the task moves to
  `state='running'` with `last_error='waiting_for_lock'`, the UI shows "another tool is downloading
  this file", and the worker retries every 1 s for up to 30 minutes before failing the task with
  `lock_timeout` (resumable, nothing discarded). Blocking `flock` is never used, because it would
  hold a worker slot invisibly and make the UI message in §7.2 step 3 unimplementable.
- **Scope**: held for the duration of one file's transfer, released before verification's `rename`
  is followed by symlink creation — never held across an entire multi-file download.

**Write path for one file:**

1. `HEAD` (or the streamed `GET`'s headers) on the resolve URL, recording **two different strings
   that must never be conflated** (§7.4):
   - the **blob name** — `x-linked-etag`, falling back to `etag`, with quotes and any `W/` prefix
     stripped. This names `blobs/<etag>` and, for LFS objects, equals the sha256 hex. It is stored in
     `download_tasks.etag` / `model_files.etag` and is **never** sent in a header.
   - the **HTTP validator** — the `ETag` response header of the *final* response after redirects,
     byte for byte, quotes and `W/` included, together with the host that issued it and any
     `Last-Modified`. Stored in `download_tasks.validator` / `validator_host` / `last_modified` and
     used for nothing but `If-Range`.

   Also take `x-linked-size` (falling back to `Content-Length`) and `x-repo-commit`.
2. **If `blobs/<etag>` already exists at the right size, skip straight to linking.** Another tool may
   have put it there; this short-circuit is what actually makes the shared cache shared.
3. `flock` the D27 lock path for the duration of the transfer; a lock held by another process
   surfaces in the UI as "another tool is downloading this file" rather than a failure.
4. Stream into `blobs/<etag>.incomplete`, computing SHA-256 inline; `fsync` on completion.
5. Verify: for LFS objects the digest must equal `<etag>`. A mismatch marks the file `corrupt`,
   deletes it, and retries once.
6. `rename` `.incomplete` → `blobs/<etag>` — same directory, so atomic and never a 40 GB copy.
7. `mkdir -p snapshots/<commit>` and create the **relative** symlink
   `snapshots/<commit>/<path> -> ../../blobs/<etag>`, matching `huggingface_hub` and keeping the
   cache movable; write `refs/<branch>`. On a filesystem that rejects symlinks (probed at root
   registration — F17), fall back to copy mode with a warning about the extra disk cost.
8. Files 0644, directories 0755: other tools running as the same user must be able to read them.

**Scan (`cache_scan` job).** Walk `<hub>/models--*/snapshots/*/`, follow symlinks, and for every
`*.gguf`: parse the GGUF header (§8.4) for architecture and shape and for `split.count`/`split.no`;
group shards by the `-NNNNN-of-NNNNN.gguf` suffix and by `split.*` metadata; classify (`mmproj*`
filename or `clip.has_vision_encoder` → `mmproj`; an embedding architecture or a `pooling_type` →
`embedding`; otherwise `text` — "draft" is a *per-instance role*, not a model property, so it is
never auto-classified); map `models--{org}--{name}` back to `{org}/{name}`; upsert `models` +
`model_files` with `origin='scanned', state='ready'`. Card metadata
is fetched lazily when a model is opened, so a 300-model scan makes no network calls and works
offline. Rows previously `ready` whose files vanished become `missing`, never deleted — a disk may
have been unplugged. GGUFs outside a snapshot directory, orphan blobs, and broken symlinks become
`stray_files` rows. Progress counters update every 250 ms, and the whole scan is one job, so it
survives a restart.

**The revision comes from the snapshot directory name — never from `refs/`.** The walk visits
`<hub>/models--*/snapshots/<commit>/`, so `<commit>` *is* `models.revision`, and it is the only
source that is right for every directory it visits. A repo directory legitimately holds several
snapshots at once — a pinned revision the user downloaded by sha, an older `main` left behind by a
previous fetch, a branch — and files fetched by explicit revision have no `refs/` entry at all.
Taking the revision from `refs/main` would stamp the current `main` sha onto every one of them,
collapsing distinct snapshots onto a single `UNIQUE(root_id, repo_id, revision, primary_file)`
identity and mislabeling the rest. So: N snapshot directories produce N distinct `models` rows, each
correctly labeled, each independently deletable and referenceable by an instance.

`refs/` is read for one purpose only: to fill the **display** field `models.ref_name`. Each file
under `refs/` is read once per repo directory and its contents matched against the snapshot shas; a
match sets `ref_name` to the branch name (`main`, `refs/pr/3`, …) and a snapshot no ref points at
keeps `ref_name = NULL` and is shown by its short sha. `.no_exist/` directories are skipped
entirely — they are a negative cache, not snapshots.

**mmproj auto-pairing.** Within one repo+revision, an mmproj is attached via `models.mmproj_model_id`
when exactly one candidate exists, preferring a precision matching the weights, then `f16`, then
`f32`. Several candidates produce a picker rather than a guess, and any manual choice sets
`mmproj_auto=0`.

**A model's resolved path is a `config_hash` input, so the models service maintains it (D69).**
`instances.config_hash` folds in the *resolved model paths* (D52), and those move without any user
edit: a `planned` model becomes `ready` with a real `snapshot_dir`/`primary_file` when its download
lands, a re-verify turns a `ready` model `missing`, and a rescan can re-point an instance's model at a
different snapshot. In the same transaction that writes any of `models.snapshot_dir`,
`models.primary_file` or `models.state`, the models service calls
`instances.RecomputeConfigHash(tx, …)` for every non-deleted instance whose `model_id`,
`mmproj_model_id` or `draft_model_id` is that model — the same method, the same rules: `updated_at`
bumped, an `events` row emitted, `generation` and `applied_config_hash` untouched. This is what makes
the "queue the download, configure the instance while it runs" flow (§3.10a) end with a
`restart_required` badge instead of an instance whose stored hash describes a path that never
existed. It sits beside `draft_validation` in §2.8's exception table for exactly the same reason.

**Delete (D28).** Refuse when any non-deleted instance references the model (`409 model_in_use`
naming them) — a *soft-deleted* instance is not a blocker (D68), and cannot be one at the SQL level
either, because **deleting a model** never issues a SQL `DELETE`: the row moves `deleting → deleted`
(§2.6) and stays, so `instances.model_id`'s `ON DELETE RESTRICT` is never exercised on this path and a
retained instance keeps a readable record of what it once pointed at. That claim is scoped to this
path on purpose: the **cache-root detach** of §7.2a *does* issue a SQL `DELETE` against
`hf_cache_roots`, which cascades `models` away and does exercise the `RESTRICT`, which is why its own
guard counts soft-deleted instances too. Otherwise: compute the plan — remove this model's snapshot links, then every blob
whose refcount **across all snapshots in that repo directory** reaches zero, then empty
`snapshots/<commit>` directories, then the `models--…` directory if nothing remains — present it as
"will free N GB, N files, keeping M shared blobs", and only then execute.

### 7.3 Sharded GGUF and mmproj in the downloader

A download is created for a **logical model**. The user picks a quant; the API expands it to every
shard plus, when `include_mmproj`, the chosen mmproj — which becomes a *separate* `models` row of
`kind='mmproj'` linked by `mmproj_model_id`, because it is separately reusable. `model_files` rows
are created up front in `state='planned'` with `size_bytes` from `lfs.size`, so total bytes and ETA
are exact from the first byte, and the queue refuses a partial shard set. Only shard 1 is needed to
parse metadata, so the fit panel becomes exact as soon as the first shard lands.

### 7.4 Resumable downloader

- Worker pool over `download_tasks` in `(priority, created_at)` order, `hf.download_concurrency`
  (default 3) **files** at a time, **one connection per file (D26)**. Rationale: HF's CDN generally
  saturates a normal link on one connection, and single-stream makes resume a one-variable problem
  instead of interval bookkeeping — and, decisively, it keeps the partial file byte-for-byte
  compatible with `huggingface_hub`'s own `.incomplete` semantics, which a striped file could never
  be. Sharded models still progress on several shards at once through the file-level pool.
- **Resume**: `stat` the `.incomplete` file → `Range: bytes=<n>-`, plus **at most one** conditional
  header chosen by this rule. The distinction the rule exists for: `download_tasks.etag` is the
  **blob name** — de-quoted, `W/`-stripped, and for LFS objects equal to the sha256 hex — and
  sending *that* string as `If-Range` cannot match any validator the origin will ever compare it
  against. The server answers `200`, the design's own rule discards the partial, and resume silently
  never works on any file, forever, while every test that stubs the origin passes.

  | condition | header sent |
  |---|---|
  | `validator` is a **strong** ETag (no `W/`) recorded from the same `validator_host` we are about to hit | `If-Range: <validator>` — the byte-exact string including quotes |
  | no strong ETag, but `last_modified` was recorded | `If-Range: <last_modified>` |
  | neither, or the redirect resolved to a different host than `validator_host` | **no `If-Range` at all** — send a bare `Range` |

  The last row is the common one on Hugging Face, and it is deliberate: `resolve/` redirects to a CDN
  whose `ETag` for the same object need not equal `x-linked-etag`, and may differ between two
  requests for the same bytes. Omitting `If-Range` there is safe rather than sloppy, because the
  **whole-file SHA-256 is the real integrity gate** (write path step 5): a resumed transfer that
  spliced the wrong bytes fails the digest, is deleted and retried, and can never produce a corrupt
  blob. `If-Range` is an optimization that avoids one wasted re-download, not the correctness
  mechanism.

  A `206` continues; a `200` means the server ignored the range or the file changed upstream, so the
  partial is discarded and the transfer restarts (and the stale `validator` is cleared).
  `Content-Range`'s total must match the recorded size or the task fails as `size_mismatch`. A `416`
  likewise restarts the file. On success the validator recorded from *this* response replaces the
  stored one. The integration suite includes an origin that returns a different `ETag` on the second
  request and one that returns the LFS oid as its `ETag`, asserting resume still completes and the
  digest still verifies.
- **Integrity across resume**: SHA-256 must cover the whole file, so the hasher state is rebuilt by
  re-reading the existing `.incomplete` bytes from disk before continuing — a sequential read at
  gigabytes per second, negligible beside the network.
- **Progress**: a wrapping `io.Reader` counts bytes; a 1 Hz ticker computes an EWMA speed and ETA
  (honest, because `bytes_at_start` is recorded), writes the rows every 2 s, and pushes an SSE patch
  every 1 s.
- Pause is a context cancel that keeps the `.incomplete` file; cancel is the same plus an optional
  delete. Network failures retry up to 5 times with backoff; the download stays `running` while any
  task is retryable.
- Optional token-bucket rate limiting when `hf.rate_limit_bytes_sec > 0`.
- **Disk guard**: before starting, free space on the target filesystem must exceed
  `bytes_total - bytes_done + 1 GiB`, else `409 insufficient_disk` carrying the numbers.
- A startup sweep removes `.incomplete` files under our own repo directories with no matching
  `download_tasks` row — and leaves every other `.incomplete` file alone, because it may belong to a
  concurrent `hf download`.

---

## 8. Fit calculator

`internal/fit` is a pure function — `Estimate(ModelShape, FlagSet, []Device, Calibration) → Report` —
with no I/O and no clock, unit-tested against recorded real-world loads.

### 8.1 Inputs

From GGUF metadata (exact after download, or via an HTTP-Range header peek before it):
`n_layer` (`{arch}.block_count`), `n_embd`, `n_ff` (`feed_forward_length`),
`n_head` (`attention.head_count`), `n_head_kv` (`attention.head_count_kv`, **scalar or per-layer
array**), `head_dim_k` (`attention.key_length`, default `n_embd/n_head`), `head_dim_v`
(`attention.value_length`, default `head_dim_k`), `n_ctx_train`, `n_vocab`,
`n_expert`/`n_expert_used`, `attention.sliding_window` and `attention.sliding_window_pattern`,
and the **per-tensor byte sizes** from the tensor index.

From the FlagSet: `n_ctx` (C), `n_parallel` (P), `n_batch` (B), `n_ubatch` (U), `n_gpu_layers`,
`flash_attn`, `type_k`/`type_v`, `split_mode`, `tensor_split`, `embedding`.

From the host: per-GPU total and free VRAM (§8.5), free system RAM, and `fit.margin_mib`
(default 1024, matching llama.cpp's own `--fit-target`).

### 8.2 Weights (D29)

Exact, post-download: `bytes(t) = (numel(t) / block_size(type)) × type_size(type)`, summed and
bucketed **by name**:

```
W_layer[i] = Σ bytes(t) for t.name matching ^blk\.i\.
W_other    = W_total − Σ W_layer[i]            # token_embd, output_norm, output head

offloaded = ngl.mode=="all"  ? L+1
          : ngl.mode=="none" ? 0
          : ngl.mode=="count"? min(count, L+1)
          : auto → the largest n with placeable(n) (§8.7)         # ESTIMATE ONLY — see below
                                                                  # per-GPU test; there is no
                                                                  # scalar Required (§8.4)

W_gpu(n) = Σ_{i<min(n,L)} W_layer[i] + (n > L ? W_other : 0)
W_ram    = W_total − W_gpu(n)
```

**`auto` is an estimate here and nowhere else (D51).** `internal/fit` resolves `auto` to a layer
count so the UI can say what will probably happen and so `max_n_gpu_layers` has a value; the launch
path does not. `RenderArgv` omits `-ngl` entirely for `auto` (§5.7), llama.cpp's `--fit` makes the
real decision at load with the real free VRAM, and its projection comes back as ground truth (D33).
The two numbers appear side by side on the instance page — "estimated 37, llama.cpp chose 36" — and
a persistent disagreement is a calibration signal (§8.7), not a bug in the launcher. This is why the
fit calculator may import nothing into `internal/instances` and why `config_hash` never moves when
free VRAM does.

`file_size / n_layer` averaging is **forbidden**: for MoE models and large-vocabulary output heads it
is wrong exactly where the answer matters, and `-ngl all` versus `-ngl L` differs by the output head
alone, which can exceed a gigabyte. Pre-download, when only HF's `gguf` summary is available, the
report falls back to `W_gpu ≈ file_bytes × n/(L+1)` and is stamped `confidence: "modeled"`.

### 8.3 KV cache, including sliding window (D30)

```
kv_ctx  = round_up(C, 256)                       # llama.cpp pads the KV cache
W_swa   = {arch}.attention.sliding_window        # the window WIDTH in tokens; NULL when absent
P_swa   = {arch}.attention.sliding_window_pattern  # the PERIOD; see the derivation below

is_swa(i) = false                                    when W_swa is absent or P_swa <= 1
          = ((i + 1) mod P_swa) != 0                 otherwise        # layer indices are 0-based
L_swa   = |{ i in [0, L) : is_swa(i) }|
L_full  = L − L_swa
per_tok(i) = n_head_kv[i] × (head_dim_k × bpe(type_k) + head_dim_v × bpe(type_v))

KV_full = kv_ctx × Σ_{i ∈ full layers}  per_tok(i)
KV_swa  = min(kv_ctx, W_swa + U) × Σ_{i ∈ swa layers} per_tok(i)
KV_total = KV_full + KV_swa
```

**The SWA derivation, spelled out, because D30 exists to get exactly this right.** Upstream's
convention is that `sliding_window_pattern = N` means a repeating group of `N` layers in which the
**last** layer uses full attention and the other `N − 1` use the sliding window — so Gemma 3's
`pattern = 6` is "five local, one global", and `L_swa = L − floor(L / N)` for `L` divisible by `N`.
Two default rules matter as much as the formula:

- **`sliding_window` absent** → the model has no SWA at all: `L_swa = 0`, `KV_swa = 0`, regardless of
  what `sliding_window_pattern` says. A pattern without a window is meaningless metadata.
- **`sliding_window` present but `sliding_window_pattern` absent** → `P_swa` is treated as **1**,
  which under `is_swa` above makes **every layer full-attention** (`L_swa = 0`). This is the
  deliberately conservative reading and it is the opposite of the naive one: `pattern = 1` means
  "every 1st layer is global", i.e. all of them. Reading the schema default of 1 as "all layers are
  SWA" would *under*-count KV by up to an order of magnitude and let the calculator promise a fit
  that OOMs — the single failure mode the golden-test rule in §8.7 forbids outright. When a model
  declares a window with no pattern, the report adds a `notes` entry saying the window was ignored
  and marks `confidence: "modeled"`.

`models.swa_pattern` therefore stores the metadata value verbatim (schema default 1) and the
*interpretation* lives in this one function, `fit.SWALayers(L, W_swa, P_swa) (L_swa int, ok bool)`,
which is table-tested against the gemma3 fixture in `testdata/gguf/` and against a model that
declares a window and no pattern.

KV follows its layers: only offloaded layers hold VRAM KV, the rest are charged to RAM. `n_head_kv`
is read as a per-layer array whenever the metadata provides one; a scalar is broadcast.

`bpe` (bytes per element), noting that block-quantized cache types are fractional:

| type | block | bytes/block | bytes/elem |
|---|---|---|---|
| f32 | 1 | 4 | 4.0 |
| f16 / bf16 | 1 | 2 | 2.0 |
| q8_0 | 32 | 34 | 1.0625 |
| q5_1 | 32 | 24 | 0.75 |
| q5_0 | 32 | 22 | 0.6875 |
| q4_1 | 32 | 20 | 0.625 |
| q4_0 / iq4_nl | 32 | 18 | 0.5625 |

`n_ctx` in llama-server is the **total** context shared across `-np` slots, so KV is sized by `C`
alone and does not scale with `P`. The UI additionally shows the derived per-slot context `C / P`,
because that is the number users actually get wrong.

### 8.4 Compute buffers and overhead (D31)

```
CB_logits = (embedding ? n_embd : n_vocab) × max(U, P) × 4
CB_act    = k_act × U × n_embd × 4                      # k_act default 6, per-arch table
CB_attn   = flash_attn ? 2 × U × n_head × head_dim_k × 4
                       : n_head × U × min(kv_ctx, 4096) × 4     # why FA matters
CB_moe    = n_expert_used × U × n_ff × 4                # 0 when n_expert == 0
CB        = CB_logits + CB_act + CB_attn + CB_moe
OH_gpu    = 400 MiB                                     # PER GPU: CUDA context + cuBLAS workspaces
margin    = fit.margin_mib                              # PER GPU, matching llama.cpp --fit-target
```

**Everything is per-GPU; there is no scalar `Required`.** The unit of the calculation is
`assigned(g, n)` — what one device is charged when `n` layers are offloaded:

```
assigned(g, n) = W_gpu(g, n) + KV_gpu(g, n) + CB + OH_gpu + margin
required_total(n) = Σ_g assigned(g, n)                  # a REPORTING total, never a test
```

`W_gpu(g, n)` and `KV_gpu(g, n)` distribute the offloaded layers across the participating GPUs by
`tensor_split` (evenly when unset) under `split_mode=layer`, the default; `split_mode=row` charges
weights by `tensor_split` and KV entirely to `main_gpu`. `CB`, `OH_gpu` **and** `margin` are charged
to *every* participating GPU — conservative, matching observed behavior, and matching llama.cpp's
`--fit-target`, which is documented as a per-device margin. The DTO reports both
`margin_bytes_per_gpu` and `margin_bytes` (= per-GPU × participating GPUs) so nothing has to be
inferred from one number.

### 8.5 GGUF parsing

A small pure-Go reader in `internal/gguf`, not a dependency. Three needs — the header KV map, the
tensor index, and first-shard-only reads — plus one nothing else offers: reading over **HTTP Range**,
so a quant can be measured before downloading 20 GB. (`gguf-parser-go` is used as a correctness
reference for the formulas, never as a dependency.)

Implementation: magic `GGUF`, version (2/3), `tensor_count`, `kv_count`, then typed KV pairs
including nested arrays, then the tensor descriptors (name, dims, type, offset). It is backed by an
`io.ReaderAt` — `*os.File` locally, and remotely a Range-issuing reader with a 1 MiB read-ahead
(a typical header is well under 2 MiB; a large token array can reach ~8 MiB, still a sub-second
fetch). Huge token arrays are parsed but not retained in `metadata_json`. A `FuzzParseHeader` target
guards against malformed input.

### 8.6 Hardware detection (D16)

`hw.Prober` is an interface with one v1 implementation, `NvidiaSMIProber`:

```
nvidia-smi --query-gpu=index,uuid,name,memory.total,memory.used,memory.free,\
utilization.gpu,temperature.gpu,power.draw,compute_cap,driver_version \
--format=csv,noheader,nounits
nvidia-smi --query-compute-apps=pid,gpu_uuid,used_gpu_memory --format=csv,noheader,nounits
```

**Units are converted at this boundary and nowhere else.** `--format=…,nounits` strips the suffix but
does **not** change the unit, and every memory field `nvidia-smi` emits is **MiB**, not bytes:
`memory.total`, `memory.used`, `memory.free` and `used_gpu_memory`. Every column and formula
downstream is bytes — `gpus.vram_total_bytes`, `instance_status.vram_bytes`, the whole of §8.4's
`assigned(g, n)`, the `/system/gpus` DTO — so wiring the parser straight into them is a factor-of-2²⁰
error that does not crash: it silently reports a 24 GB card as having 24 KB free and turns every fit
verdict into `wont_run`. `NvidiaSMIProber` therefore multiplies each of those four fields by
`1 << 20` as it parses, and that multiplication appears exactly once in the codebase. The other
columns are left alone and their units are named here too, so the same mistake cannot be made twice:
`utilization.gpu` is a percentage, `temperature.gpu` is degrees Celsius, `power.draw` is watts (a
float, and `[N/A]` on cards without a sensor), `compute_cap` is a `major.minor` string. A unit test
feeds a recorded `nvidia-smi` line for a 24 GB card and asserts `vram_total_bytes == 25769803776`.

**The `gpu_uuid` column is not optional (D17).** Per-process attribution has two consumers, and the
second cannot be built without GPU identity: `instance_status.vram_bytes` needs only `pid` and
`used_gpu_memory`, but `instance_status.gpu_uuids_json` — which SPEC §3.5's bench exclusivity guard
reads through §10 — needs to know *which* GPU each row belongs to. A `pid,used_gpu_memory` query
returns no such field, so on a multi-GPU host it can never answer "would this bench collide with a
loaded instance on these GPUs". `gpu_uuid` is queried directly, and `instance_status.gpu_attribution`
records how the answer was obtained so every consumer can see its own confidence:

| `gpu_attribution` | how | when |
|---|---|---|
| `measured` | the `gpu_uuid` column above, joined on `MainPID` | the normal path |
| `measured` | fallback: loop `nvidia-smi -i <index> --query-compute-apps=pid,used_gpu_memory` once per GPU, taking identity from the loop variable | drivers whose `nvidia-smi` rejects `gpu_uuid` as an unknown field (detected once, by the non-zero exit plus its "not a valid field" message, and then remembered for the process lifetime) |
| `declared` | the instance's own `device_filter` / `tensor_split` / `main_gpu`, else **every present GPU** | the process is running but attribution produced no rows yet (early in load), so the conservative superset is used |
| `unknown` | — | `nvidia-smi` failed entirely (F14): `gpu_uuids_json` is `NULL`, `vram_bytes` is `NULL`, never 0 |

Consumers must not treat `declared` or `unknown` as "no GPUs": §10's guard treats a non-`measured`
instance as occupying every GPU it could occupy, so the exclusivity promise fails closed.

Results are cached ~2 s. A non-zero exit or unparsable line marks every GPU `state: unknown` and
returns `null` fields — **never zeros** (F14), because a fabricated 0 MiB free would make the fit
calculator confidently wrong. NVML via `purego`/`dlopen` is deliberately rejected: hand-bound
versioned C structs are untestable ABI risk, a `CGO_ENABLED=0` binary is statically linked with no
dynamic loader to service `dlopen`, and the whole saving is one ~30 ms fork per sample.
**That substitution is recorded as an explicit amendment to SPEC §3.2's and §3.1's literal
"NVML" in D16**, not left as an unstated departure — `nvidia-smi` is itself an NVML client, so
the numbers the spec asks for are the numbers this reports. The interface
is where a v2 ROCm/Vulkan prober drops in. RAM comes from `/proc/meminfo` (`MemTotal`,
`MemAvailable`), CPU from `/proc/cpuinfo`, disk from `unix.Statfs` — all pure Go.

### 8.7 Calibration and verdicts (D32, D33)

**Calibration.** Every time an instance reaches `ready` — and after every bench point — the
supervisor parses llama.cpp's own startup lines (model buffer size, KV self size, compute buffer
size, and the `--fit` projection when present) and writes a `fit_observations` row **beside the
prediction that was made**. `fit.Calibration` then corrects `k_act` and `OH_gpu` per
`(arch, backend, llamacpp_tag)` by the median observed ratio over the last 20 observations, clamped
to `[0.5, 2.0]` and requiring ≥ 3 samples; below that, defaults stand. Reports say
`confidence: "calibrated"` once a correction is in effect and `"modeled"` otherwise. The
compute-buffer term is the only genuinely empirical part of the model, and learning it from this
host's own loads beats any hard-coded constant.

**Verdicts.**

```
placeable(n) ⟺ ∀ g ∈ selected GPUs : assigned(g, n) ≤ free(g) − reserve(g)
reserve(g)   = the request's reserve_bytes_per_gpu (§3.9), default 0 — charged to
               EVERY participating GPU, like margin and OH_gpu; never divided

fits      ⟺ placeable(L+1)
partial   ⟺ ∃ n ∈ [0, L] with placeable(n),
            and W_ram + KV_ram ≤ 0.9 × free system RAM      → report the largest such n
wont_run  otherwise
```

**The test is per-GPU (`∀`), never a sum.** Comparing one scalar against `Σ free VRAM` says "fits" on
a 23 GB + 4 GB pair for a model that cannot be placed on either card, because llama.cpp does not
pool VRAM across devices — layers land on a specific GPU and either fit there or fail. The response
DTO already carries the right shape: `per_gpu[].assigned_bytes` and `per_gpu[].ok`, and the verdict is
exactly `∀ g: per_gpu[g].ok`. `required_vram_bytes` remains in the response as the sum, clearly
labeled a total, so the UI can show "18.4 GB across 2 GPUs" without anything computing a verdict from
it. When the verdict is `wont_run` or `partial` because of one imbalanced device, `notes` names that
device ("GPU 1 is short by 2.1 GB — try `--tensor-split 0.85,0.15`") rather than reporting a total
the user cannot act on.

Also returned, each by a descending scan or binary search over the same pure function (≤ 200
evaluations, sub-millisecond): `max_n_gpu_layers` (**what we predict llama.cpp's `--fit` will choose
for `-ngl auto`, and the value `POST /instances/{id}/pin-ngl` writes — advisory, never rendered into
argv; D51**),
`max_ctx_at_full_offload` (largest C, rounded down to 256, that fits with everything offloaded), and
`recommendation` — prefer full offload; failing that try `type_k`/`type_v = q8_0` with
`flash_attn: on` (required for quantized V on most builds) before reducing layers; never recommend a
context below 4096 without saying so in `notes`.

**Ground truth (D33).** llama-server's own `--fit` projection, captured from the startup log, is
shown next to the estimate on the instance page and labeled "reported by llama.cpp": the estimate is
for planning, the report is for truth, and a disagreement becomes visible and explainable rather
than an invisible bug. Upstream disables `--fit` when `-ngl` or `--tensor-split` is pinned, so the
UI marks it "unavailable" in that case instead of pretending — and `-ngl auto` renders no `-ngl` at
all (D51) precisely so this projection stays available and authoritative. On such a load the parsed
`--fit` layer count is also written to `fit_observations` beside our `max_n_gpu_layers`, which makes
"auto" the best-calibrated configuration in the product rather than the least understood one.

**Golden-test rule.** The fit suite runs against ~20 recorded real loads in `testdata/fit/` and
asserts predictions within ±10% **and, non-negotiably, that a verdict never says "fits" for a
recorded load that actually OOM'd** (`fit_observations.oom = 1`).

---

## 9. Inference gateway and token store

### 9.1 Listener lifecycle

`internal/gateway` keeps `map[instanceID]*publicListener`, reconciled from the DB at boot and on
every instance create/update/delete:

- **Bind address.** Every public listener binds `gateway.bind`:`instances.public_port`, where
  `gateway.bind` is a settings key defaulting to `0.0.0.0` — SPEC §1's trusted-LAN exposure, since a
  gateway reachable only from loopback would defeat the entire point of owning the public port.
  `ui.bind` governs the **management UI only** and the two are deliberately separate: binding the UI
  to `127.0.0.1` while inference stays on the LAN is a reasonable and common posture, and one
  setting could not express it. Changing `gateway.bind` is `restart_required` (§3.4); every listener
  is rebound on the next daemon start.
- **Port collisions are prevented at save time, not discovered at bind time.** The §2.8 port rules
  reject a `public_port` equal to the management port or inside the internal pool with
  `422 port_unavailable`; F6 below remains the runtime fallback for the genuinely unpredictable case
  (an unrelated process grabbed the port between validation and listen).
- A listener is **open whenever the instance row exists and is not deleted** — not only while the
  model is loaded. A client hitting a stopped instance gets a JSON
  `503 {"error":{"code":"instance_not_running"}}` instead of connection-refused, which is far easier
  to debug, and the port cannot be stolen by another process while the instance is stopped.
- Changing `public_port` closes the old listener and opens the new one. **A bind failure is a
  per-instance banner and a notification, never a daemon start failure** (F6); the instance keeps
  serving on its internal port and the UI offers a port picker.
- Each listener is its own `http.Server`: `ReadHeaderTimeout: 15s`, `WriteTimeout: 0` (a token
  stream can run for many minutes), `IdleTimeout` from settings, `MaxHeaderBytes: 1MB`.
- On-demand start and idle-stop are **not** implemented in v1: SPEC §6 defers multi-model routing,
  and putting unit-start latency inside a request path contradicts the rule that the daemon is never
  in the data path of a running model.

### 9.2 Request path

```
accept → route (/health and /llamaman/info handled locally; everything else proxied)
       → extract credential (Authorization: Bearer | X-API-Key | ?api_key=)
       → auth_mode=='none' ? allow : verify(token)
       → httputil.ReverseProxy → 127.0.0.1:<internal_port>
       → account (bytes, duration, reported usage) → batched flusher
```

- `Rewrite` sets only the upstream URL and preserves path and query verbatim: the OpenAI-compatible
  API passes through **unmodified**, per SPEC §3.4.
- `FlushInterval: -1` (immediate flush) so SSE and chunked token streams are not buffered.
- **`DisableCompression = true` on the Transport (D36).** Without it, when a client sends no
  `Accept-Encoding`, Go's Transport adds `gzip` and transparently decompresses — so the bytes the
  client receives are not the bytes llama-server sent, breaking byte-for-byte pass-through in a way
  no test would notice. With it, the client's own negotiation passes through untouched.
- `ResponseHeaderTimeout` 10 minutes to cover prompt processing on a cold cache; no overall response
  timeout; `IdleConnTimeout` 90 s.
- Hop-by-hop headers stripped; `X-Forwarded-For`/`X-Forwarded-Proto` appended. The client's
  `Authorization` header is **replaced, never forwarded**, since llama-server runs without
  `--api-key`.
- Upstream errors become `502`/`503` in the OpenAI error shape with `code: instance_not_running`, and
  when the instance is loading, a `Retry-After` derived from the previous launch's observed load
  time.
- The client's context is propagated, so a disconnect cancels upstream and frees the llama-server
  slot immediately. Request bodies are size-capped (`gateway.max_body_mb`) and streamed, never
  buffered.
- **The response body is never buffered either.** `gateway.max_body_mb` is a *request* cap and there
  is deliberately no response cap: a 40-minute completion stream has no size known in advance. The
  only thing the gateway retains from a response is the bounded tail described in §9.3, and it is
  retained *behind* the writer, so every byte still reaches the client immediately under
  `FlushInterval: -1`.

### 9.3 Token store

- **Format**: `lm_` + base58 of 32 `crypto/rand` bytes. `prefix` is `lm_` plus the first six
  characters, stored in the clear for display and log correlation.
- **Hash (D37)**: `sha256` of the full secret. These are 256-bit uniformly random secrets with no
  dictionary to attack, and an argon2 hash on every inference request would add ~100 ms to every
  call. The admin *password* keeps argon2id.
- **Verification is O(1)**: `token_hash` is unique-indexed, with a `sync.Map` cache of
  `hash → {id,state,scope,instanceIDs,expiresAt,rateLimit}` in front of it, guarded by an **epoch
  counter** bumped by every token or scope write. A request whose cached epoch is stale re-reads the
  row, so revocation takes effect within one request with no reload of anything — which is the whole
  reason Llama Man owns the public port (SPEC §1). Comparison uses
  `crypto/subtle.ConstantTimeCompare`.
- Scope: `global` passes everywhere; `instances` requires a `token_instances` row for the listener's
  instance. Expiry, `state` and the optional `rate_limit_rpm` token bucket are checked per request.
- **Accounting (D56)**: two in-memory counter maps are upserted every 5 s and on shutdown.
  `map[instanceID+day+authMode] → instance_usage_daily` is written for **every** proxied request,
  including `auth_mode='none'`, and is the source for SPEC §3.3's "requests served" and the
  dashboard's bytes and error rates. `map[tokenID+instanceID+day] → token_usage_daily` is written
  additionally whenever a credential was presented, and is the per-token breakdown SPEC §3.4 asks
  for. Writing only the second table would mean a no-auth instance — an explicit SPEC feature —
  accumulated no requests, no bytes and no errors anywhere in the database, with only
  `gateway_denials_daily` (rejections) to show for it. `api_tokens.last_used_at`/`request_count`
  update at most once per 10 s per token.
- **Token counts are recorded only when the upstream reports them, and only through a bounded tail
  tap.** This is the one place the gateway looks *inside* a response, so the mechanism, its memory
  bound and its failure mode are stated rather than implied:
  - **Classification uses the response, never the request.** The gateway promised not to buffer the
    request body, so it cannot know whether the client sent `"stream": true` — and it does not need
    to. A response whose `Content-Type` is `text/event-stream` is a **stream**; anything else is
    **non-streamed**. That is exactly the distinction that matters, it is available in the response
    headers before the first byte of body, and it costs nothing.
  - **Non-streamed**: the proxy wraps the upstream body in a `tailTap` — an `io.Reader` that copies
    every byte straight through to the `ResponseWriter` and additionally keeps the last
    `gateway.usage_parse_kb` (default **64 KiB**) in a fixed ring buffer. Nothing is held back and
    nothing is delayed. At EOF the ring is scanned backwards for `"usage"` and the object after it is
    parsed with a small brace-matching scanner. The tap is installed only when `Content-Type` is
    `application/json` and either there is no `Content-Length` or it is ≤ 8 MiB; otherwise no tap is
    installed at all.
  - **Streamed**: the same tap, sized the same, with the scan run on the final non-empty `data:`
    frame — which is where `usage` appears when, and only when, the client set
    `stream_options.include_usage`.
  - **Memory bound**: `gateway.usage_parse_kb` per in-flight proxied request, allocated from a
    `sync.Pool`, hard-capped and never grown. `gateway.usage_parse_kb = 0` disables the tap
    entirely, which is the setting for anyone who wants byte-for-byte pass-through with provably zero
    inspection.
  - **Failure mode**: a truncated tail, a `usage` object split across the ring boundary, a compressed
    body, or any parse error leaves `prompt_tokens`/`completion_tokens` `NULL`. It is never an error
    the client sees, never a retry, and never a reason to hold a response. Accurate instance-level
    token totals come from `/metrics` regardless (below), which is why guessing here would be worse
    than abstaining.
  The gateway **never rewrites the request** to obtain these numbers, because that would change
  client-visible behavior and break pass-through. Absent numbers stay `NULL` and
  the UI says "not reported" rather than inventing a value; instance-level **token** totals come from
  llama-server's own `/metrics` (`llamacpp:prompt_tokens_total`,
  `llamacpp:tokens_predicted_total`), which is authoritative and free, and the UI labels the two
  sources distinctly. Turning `metrics_endpoint` off therefore costs token totals and slot detail
  and leaves `instance_status.requests_served` `NULL` (the UI says "metrics disabled", never 0) —
  but it never costs the gateway's own request, byte, error and duration counters, which come from
  `instance_usage_daily` and exist for every instance in both auth modes (§2.9).
- Denials are counted per instance and reason; five denials from one IP within a minute emit a
  `warn` event so the dashboard can show "unauthorized attempts on port 8081".

### 9.4 Restarting the daemon without dropping the public ports (D58)

SPEC §1 makes Llama Man the owner of the public inference ports, and SPEC §3.8 promises that a
self-update leaves running instances unaffected. Those two statements are only compatible if a daemon
restart does not close the gateway. It is worth being exact about what would otherwise happen: the
public listeners are in-process `http.Server`s (§9.1), `selfupdate-apply` ends with
`systemctl restart llamaman.service`, and D12 deliberately removed the FD-inheriting re-exec — so a
naive implementation closes every public listener and returns connection-refused on every instance
port for at least `RestartSec=2`. `llama-server` would indeed be untouched, and no API client could
reach it, which is not what SPEC §3.8 promises.

**Socket activation cannot solve it here.** A `.socket` unit per instance would have to be written to
`/etc/systemd/system` whenever a user creates an instance — a privileged runtime write that D3 and
D57 exist to forbid. The mechanism that *does* fit is systemd's **file-descriptor store**, which
needs no privileged write, no unit per instance, and no re-exec:

**Shutdown**, driven by SIGTERM, by `POST /system/restart` and by §12.1 step 7. Steps 1–6 are
identical for all of them; step 7 is where they part, and D79 says why:

1. Commit whatever domain transition prompted the restart (`self_updates` `staged → swapping`, the
   settings write, …) **before anything else**. The DB must be consistent even if the process is
   killed in the next millisecond.
2. Flush the HTTP `202` to the caller and close that one connection (`Connection: close` plus an
   explicit `Flush()`), so the response is on the wire before the process can be signaled. Only then
   is the restart scheduled, from a detached goroutine after a 250 ms grace.
3. Stop accepting **new** connections on each listener, but keep the socket open.
4. Drain: in-flight proxied requests are given `gateway.drain_sec` (default 20 s) to finish. Streams
   that outlive it are closed; the count is logged and recorded in the `events` row so
   "zero dropped requests" is a measured claim rather than a hope.
5. `PRAGMA wal_checkpoint(TRUNCATE)`, close the job queue's leases, flush the accounting counters.
6. For each listener, `sd_notify` `FDSTORE=1` with `FDNAME=ui` or `FDNAME=gw-<instance_id>`, passing
   the socket fd. systemd holds them; `FileDescriptorStorePreserve=` defaults to `restart`, which is
   exactly the wanted scope — preserved across `systemctl restart`, dropped on a full `stop`.
7. **End the process — but *how* depends on who is going to act next, and this is the one step the
   two callers do not share (D79).**

   | caller | step 7 |
   |---|---|
   | SIGTERM, and `POST /system/restart` | **Exit.** systemd is already stopping or restarting this unit; the job that will bring us back is in flight, and exiting is what completes it. `TimeoutStopSec=45` covers the drain. |
   | §12.1 step 7 (**system-scope** self-update) | **Do not exit. Wait to be stopped.** After the fd store hand-off the daemon closes its listeners, releases its job leases, and blocks on SIGTERM with a 120 s failsafe. |

   The distinction is not fastidiousness — `llamaman.service` is `Restart=always` with
   `RestartSec=2`. A daemon that exited voluntarily two milliseconds after
   `StartNoWait llamaman-selfupdate.service` would be restarted by systemd, as the **old** binary,
   while `selfupdate-apply` was still re-verifying the tarball and had not yet renamed the new one
   into place. That intermediate boot runs the §11.1 step 11 confirmation gate against an
   `update/pending` naming a version it is not, and then the real swap happens underneath it. Waiting
   removes the race entirely: the oneshot's own `systemctl restart --no-block llamaman.service` is
   what ends this process, at the moment the binary on disk is the one that should come back.

   The **failsafe** exists because a blocked wait with no bound is a hang. If no signal arrives within
   120 s the daemon logs at `error`, deletes **nothing**, and exits, letting `Restart=always` bring a
   binary back for an ordinary boot — the new one if the swap has already happened, the old one if it
   has not. Either way the boot is inert: §12.3's gate confirms (branch 1) if the swap took, defers
   (branch 2) while `llamaman-selfupdate.service` is still active, and closes the update out (branch 3)
   once it is not. The deferral is bounded by that unit's own `TimeoutStartSec=120` rather than by any
   clock this design keeps (D91), so an actor that is still working is always deferred to and one that
   died is resolved rather than waited on forever.

   The one caller not in the table is the D2 user-scope self-update, because it is not this shape:
   there the daemon performs the swap itself *before* issuing the restart (§5.2a item 2), so by the
   time it exits the binary on disk is already the new one and `Restart=always` starting it is the
   intended outcome. That is the whole table now — there is no rollback path to give it a third row
   (D87), and no actor anywhere in this design stops `llamaman.service` on the daemon's behalf.

**Startup** (§11.1 step 10): read `LISTEN_FDS`/`LISTEN_FDNAMES`, adopt each fd by name with
`net.FileListener`, and match it against the instance set. A name with no surviving instance is
closed; an instance with no stored fd is bound fresh; a stored fd whose `public_port` changed while
the daemon was down is closed and rebound. The kernel accept queue held every connection that
arrived during the gap, so from a client's perspective a self-update is a pause of a second or two,
not a refusal.

**What is and is not preserved — stated plainly, because the difference is the honest version of
SPEC §3.8:**

| | across a daemon restart |
|---|---|
| `llama-server` processes and loaded models | untouched — separate units, and they `execve`'d away from the llamaman binary long ago |
| Listening sockets on every public port | **preserved** (fd store); no connection-refused window |
| Connections that arrive during the gap | queued by the kernel, served when the new daemon accepts |
| Requests in flight at the moment of restart | drained for up to `gateway.drain_sec`; anything still streaming after that is closed. A generation longer than the drain window **is** interrupted, and the UI's restart confirmation says so |
| The management UI session | preserved (sessions are in the DB) |

**When the fd store is unavailable** — systemd older than v229, `systemd_control='exec'`, or the D2
user-scope manager on a host where the store is disabled — the daemon detects it at boot (no
`NOTIFY_SOCKET`, or `FDSTORE=1` rejected), records
`runtime_info.listener_continuity='none'`, and reports it in `GET /system/capabilities`. The restart
still happens; it simply has a short connection-refused window, and both the self-update dialog and
the `POST /system/restart` confirmation say "clients will see ~2 s of connection refused" instead of
"no interruption". Nothing silently degrades.

**Risk 6's test is written against this design** (§18): the integration suite self-updates while a
stub instance serves a token-authenticated stream, and asserts **zero connection-refused errors and
zero requests dropped inside the drain window**, with the post-drain closures counted and asserted
to be zero for a request that completes in under `gateway.drain_sec`. The `listener_continuity='none'`
path is asserted separately to produce exactly one refused window and the matching capability flag.

---

## 10. Bench runner

- **Expansion first.** `POST /bench/runs` expands the sweep cross-product into `bench_points` rows
  *before* execution, which buys exact progress, exact resume after a crash, and a duration estimate
  from prior runs' median seconds-per-point for that model class.
- **One `llama-bench` invocation per point**, `-o json -r <repetitions>`, rather than letting
  llama-bench expand the cross-product internally. llama-bench only prints results at the end, so
  per-point invocation is what gives incremental persistence (a 3-hour sweep interrupted at point 40
  keeps 40 results), precise cancellation, live progress, and per-point failure isolation. The
  model-reload cost is accepted and shown in the duration estimate.
  Example: `versions/active/bin/llama-bench -m <path> -ngl 999 -b 2048 -ub 512 -fa 1 -ctk q8_0
  -ctv q8_0 -p 512 -n 128 -d 4096 -r 3 -o json`.
- Points are rendered from the **same `FlagSet`** as instances, by the sibling renderer
  `instances.RenderBenchArgv` (D62) — see §10.1, which pins the mapping field by field. A benchmark
  therefore measures the configuration the user would actually run, without pretending that
  `llama-server` and `llama-bench` accept the same command line.
- **One bench at a time is a lease, not an index (D75).** The bench worker acquires the `bench_lease`
  singleton (§2.10) in the same transaction that moves its job to `running`, with the same conditional
  UPDATE the build worker uses: `WHERE id=1 AND (job_id IS NULL OR owner=? OR expires_at < ?)`. Zero
  rows changed means another sweep holds it, and the job stays `queued` with `run_after = now + 15 s`
  and "waiting for the running benchmark" in the UI — a queue, not an error. Without it nothing at all
  prevented two concurrent sweeps: `jobs.subject_id` for a bench is `bench_runs.id`, so
  `idx_jobs_one_live_per_subject` never binds across two different runs (exactly the reasoning D70
  gives for builds), and the exclusivity guard below inspects only `instances`, never other
  `bench_runs`. Two sweeps on one GPU would each stop the other's restored instances and each write
  `stopped_instances_json` and `restore_done` over an overlapping set — the one outcome this section
  calls the worst possible one, arrived at by two well-behaved workers.

  The lease is released when the job reaches a terminal state, when a run is canceled, and at boot for
  any row whose `owner` is not the current `boot_id` — **but never before the stop-and-restore
  finalizer has set `restore_done=1`**, because a run that still owes production instances their
  restart is still occupying the host even though nothing is executing. That is also what makes §6.6
  step 1's guard total: "a bench is live" is the lease being held **or** any row with
  `restore_done=0 AND stopped_instances_json IS NOT NULL`.
- **Exclusivity guard** (`bench.exclusive_gpu`, default on — SPEC §3.5): preflight lists instances
  whose `instance_status.gpu_uuids_json` intersects the target GPUs. That column is populated from
  the `pid,gpu_uuid,used_gpu_memory` attribution query of §8.6/D17 — the `gpu_uuid` field is
  load-bearing here, because without per-GPU identity the guard has no way to distinguish "loaded on
  the GPU you are about to benchmark" from "loaded on the other one", and SPEC §3.5's promise cannot
  be kept on a multi-GPU host. **The guard fails closed**: an instance whose
  `gpu_attribution` is `declared` or `unknown` is treated as occupying every GPU it could occupy
  (its `device_filter`/`tensor_split`/`main_gpu` set, else all present GPUs), so a bench is never
  launched into a collision merely because attribution was unavailable, and the preflight response
  says which instances were included on that basis. `on_conflict:"abort"` → `409 bench_gpu_conflict`
  naming them. `on_conflict:"stop_and_restore"` → stop them, record the ids in
  `stopped_instances_json`, and restore them (stamping `pending_trigger='bench_restore'`) from a
  finalizer that runs on success, failure **and** cancellation, and again at boot. A benchmark that
  leaves production instances down is the worst possible outcome, so restoration is idempotent and
  re-checked at boot.
- **The boot restore condition is `restore_done = 0`, in any run state.** It is deliberately *not*
  "left `running` with `restore_done=0`": a state predicate alone is a trap, because whatever moves
  the run out of `running` — a crash mid-run, a cancel that did not finish, an operator's delete —
  would silently disqualify the very rows that still owe a restore. The boot sweep is therefore
  `SELECT … FROM bench_runs WHERE restore_done = 0 AND stopped_instances_json IS NOT NULL`, over
  every state including terminal ones, and `restore_done=1` is written only after every named
  instance has been restarted (or found already running, or found deleted). The two facts are kept in
  separate columns for this reason: `state` says what the *benchmark* did, `restore_done` says what
  the *host* is owed.
- **Its `jobs` row survives a daemon restart as `interrupted`, not `failed` (§2.3).** That is the
  second half of the same guarantee. Generic orphan recovery marking a `bench_run` job `failed` would
  — under §2.3a — also mark `bench_runs.state='failed'`, and any restore rule phrased over `running`
  would then match nothing at all: a bench that stopped two serving instances would leave them down
  forever. `interrupted` keeps the subject locked so no second bench can start on it, and hands the
  row to this finalizer. On resolution the finalizer restores the instances, sets `restore_done=1`,
  moves the run to `partial` if any point succeeded and `failed` otherwise, and closes the job
  `failed` with `error_code='daemon_restarted'` — both rows, one transaction, per §2.3a.
- **Parsing**: each object in llama-bench's JSON array becomes a `bench_results` row —
  `n_prompt`, `n_gen`, `n_depth`, `avg_ts`, `stddev_ts`, `avg_ns`, `stddev_ns`, `samples_ns` — with
  `raw_json` preserved verbatim, so a future llama-bench schema change never loses data.
- **Environment capture** lives on the run row, not the results: llama.cpp tag and commit, backend,
  GPU names/UUIDs/VRAM/driver/CUDA version, CPU model and cores, RAM, kernel. Cross-version
  comparisons are meaningless without it.
- **Progress and cancellation**: stdout is line-parsed between repetitions into
  `jobs.progress_json` (`{points_done, points_total, current:"ngl=20 b=2048"}`); cancel SIGINTs the
  process group, marks the point `skipped`, and runs the restore finalizer.
- **Comparisons** are plain SQL over `bench_points ⋈ bench_results` with the sweep axes as columns,
  so "tg128 across ngl for three llama.cpp builds" is one grouped query rather than post-processing
  in Go.
- **Export**: `json` (runs + points + results, self-describing), `csv` (one row per result with all
  axes flattened), `md` (a table plus a provenance header naming model, quant, llama.cpp tag, driver
  and GPU) — ready to paste into an issue.

### 10.1 `RenderBenchArgv`: the `FlagSet` → `llama-bench` mapping (D62)

`llama-bench` is a different program with a different argument parser, and the two example command
lines in this document prove it: `-fa 1` versus `-fa on`, `-p 512 -n 128 -d 4096 -r 3 -o json` versus
`--host --port --no-webui --props --slots --metrics`. No single `RenderArgv` can emit both. So there
are two renderers, both in `internal/instances` so the D49 invariant and its import-graph test still
hold, both consuming one `model.FlagSet`, and the translation is pinned here so they cannot drift:

| `FlagSet` field | `llama-bench` | note |
|---|---|---|
| model path | `-m <path>` | shard 1 for a sharded set, exactly as `RenderArgv` resolves it |
| `n_gpu_layers` | `-ngl <n>` | `all`→`999`, `none`→`0`, `count`→`N` — the **same constants `RenderArgv` emits** (§5.7), so a bench measures the number the server would run. **`auto` → `-ngl 999`**: llama-bench has no `--fit`, so "let llama.cpp decide" has no meaning here; the point is labeled `ngl=999 (auto)` in the results table so the substitution is visible |
| `batch_size` / `ubatch_size` | `-b` / `-ub` | direct |
| `threads` / `threads_batch` | `-t` / `-tb` | omitted when NULL, as everywhere |
| `flash_attn` | `-fa 0\|1` | **`on`→1, `off`→0, `auto`→1**, and `auto` adds a `notes` line to the run: llama-bench takes no tri-state |
| `cache_type_k` / `cache_type_v` | `-ctk` / `-ctv` | direct, same vocabulary |
| `split_mode` / `tensor_split` / `main_gpu` | `-sm` / `-ts` / `-mg` | direct; `-ts` is comma-separated, not repeated |
| `device_filter` | `--device` | verbatim, D66 applies identically — no `CUDA_VISIBLE_DEVICES` |
| `mlock` / `no_mmap` | `--mlock` / `--no-mmap` | direct |
| `numa` / `cpu_mask` / `prio` | `--numa` / `-C` / `--prio` | direct |
| — | `-p`, `-n`, `-d`, `-r`, `-o json` | **from the sweep point, not the FlagSet**: prompt length, generation length, depth, repetitions, output format |
| `ctx_size` (`-c`) | **dropped** | llama-bench sizes context from `-p`/`-n`/`-d`; passing `-c` is an unrecognized-argument exit |
| `alias`, `parallel`, `cont_batching`, `embedding`, `pooling`, `rerank`, `jinja`, `chat_template*`, `n_keep`, `n_predict`, `defrag_thold`, `cache_reuse`, `slot_save_path`, `*_endpoint`, `log_verbosity`, `rope_*`, `yarn_*` | **dropped** | server-only concepts; llama-bench has no server |
| `--host`, `--port`, `--no-webui`, `--props`, `--slots`, `--metrics` | **never emitted** | appended only by `RenderArgv` |
| `draft.*` | **dropped** | speculative decoding is a server feature; a draft-model bench is out of v1 scope and the sweep builder says so |
| `extra_flags` | **dropped** | they are `llama-server` flags. A separate `bench.extra_flags` field on the sweep is passed through verbatim for llama-bench's own escape hatch, validated against the same forbidden list (`-m`, `-o`, `-r`) |

Every dropped field is dropped **loudly**: `GET /bench/preflight` returns
`"ignored_flags":[{"field":"ctx_size","reason":"llama-bench has no -c; context is derived from -p/-n/-d"},…]`
and the sweep builder renders them as a dismissible note above the estimate, so "why is my benchmark
not measuring my 32k context" is answered before the run rather than after it.

Two golden tests, side by side in `internal/instances`, render the same `FlagSet` through both
functions and assert both byte-exact outputs — which is what makes the mapping above a contract
rather than a comment. One further test asserts that `RenderBenchArgv` never emits `-c`, `--host`,
`--port` or any string from the dropped list.

---

## 11. Setup wizard, zero-config bootstrap, and the CLI

### 11.1 Boot sequence (`llamaman serve`) — no configuration file exists, ever

1. **Determine the systemd scope, then resolve the state directory.** Neither can be assumed, and the
   order matters: the state-directory fallback chain branches on the scope.

   **Scope**, by one rule with one fallback. Both this and the state directory are decided **in
   memory** here — the database is not open until step 3 — and persisted into
   `runtime_info.systemd_scope` and `runtime_info.state_dir` at step 6 with everything else the probe
   learned. Nothing re-derives either value later; every consumer reads the column:
   1. `serve --scope user|system` when present. `install-units` renders it (`@SCOPE_FLAG@`, §5.4)
      because `install-units` is the component that *decides* the topology — it chooses
      `/etc/systemd/user` over `/etc/systemd/system`, installs or omits the polkit rule, and picks the
      prefix. Like `--port`, it is a flag on our own binary written by our own installer, not a config
      file and not an environment variable, so SPEC §3.9 is intact.
   2. Otherwise — a hand-run `llamaman serve`, or a unit installed before the flag existed — the scope
      is `user` when a connection to the **user** bus (`$DBUS_SESSION_BUS_ADDRESS`, else
      `/run/user/<euid>/bus`) succeeds *and* that manager reports `llamaman.service` as a known unit,
      and `system` in every other case, including no bus at all. This is a cheap probe made here, and
      the connection it opens is the one step 6 reuses for the full controller selection (§5.3) —
      there is one connection, not two. The manager that owns our unit is the one fact that cannot
      disagree with what the daemon will actually do next, which is why it is the fallback and
      `$XDG_RUNTIME_DIR`, `$INVOCATION_ID` and the euid are not: each of those is set in cases where
      the others are not, and a `--user-units` install is routinely a uid ≥ 1000 with a session bus
      present — indistinguishable, by those signals alone, from a system-scope install run by the same
      person while logged in.

   A resolved scope of `user` with euid 0, or `system` with a state directory under `$HOME`, is not
   fatal but is reported: `runtime_info.polkit_detail` records it and `doctor` raises it, because it
   almost always means the units and the binary came from different installs.

   **State directory (D72), in this order**, recorded in `runtime_info.state_dir` — every other path
   in §6.1 is relative to it:
   1. `$STATE_DIRECTORY`, which the service manager exports from `StateDirectory=llamaman` and which
      is therefore the manager's own answer to the question, correct in both scopes. Like
      `$NOTIFY_SOCKET`, it is set by systemd rather than by a user, so reading it is not a
      configuration file and does not breach SPEC §3.9. (When it names several paths, the first
      wins.)
   2. `$XDG_STATE_HOME/llamaman`, then `$HOME/.local/state/llamaman`, when the scope resolved above
      is `user` or the process is not running under a service manager.
   3. `/var/lib/llamaman` otherwise.

   `/var/lib/llamaman` is the **default**, not a constant: `systemd --user` resolves
   `StateDirectory=llamaman` to `~/.local/state/llamaman` and sets `$STATE_DIRECTORY` accordingly, so
   a hardcoded literal disagreed with the manager, with the unit's own `WorkingDirectory=` and
   `ReadWritePaths=` (now `%S/llamaman`, §5.4) and with the D2 topology's own judge path — the
   `--user-units` install simply could not start. The units guarantee the directory exists; this step
   only decides which one it is. A `doctor` check asserts the resolved directory is writable by the
   service identity and reports all three candidates when it is not.
2. `flock` **`<state_dir>/llamaman.lock`** — the directory step 1 just resolved, never a literal
   `/var/lib/llamaman/llamaman.lock`. A second daemon exits **70** with a message naming the holding
   PID (F11), so a hand-run `llamaman serve` can never race the unit. The ordering is not cosmetic:
   under `systemd --user` that literal path is neither created by `install.sh --user-units` (§13
   step 6 creates the tree under `~/.local/state/llamaman`) nor writable by the service identity, so
   locking it first made the very first boot step fail with the F11 message on a topology that had not
   even started — reproducing the "the D2 install could not start" failure D72 exists to remove, one
   step before D72 ran.
3. Open or create `llamaman.db`, enforce mode 0600, run `PRAGMA integrity_check`. On failure (F12):
   move the file aside, restore the newest `db-backups/` entry, else start a fresh DB — and raise a
   notification listing what was lost. **Instances keep running throughout**, because they are
   separate units that do not depend on the daemon.
4. **The schema gate, then the migrations.** If `MAX(schema_migrations.version)` is **greater** than
   the highest migration embedded in this binary, the database was written by a newer release — the
   state a downgrade leaves behind (§12.4, D90). The daemon does not open it for writing, does not
   migrate and does not serve: it logs one journald line naming both versions and the newest
   `db-backups/` snapshot whose schema this binary can open, and exits non-zero. Running a v14 query
   set against a v15 schema can corrupt data, and there is no forward-only migration that undoes one.
   That refusal is a start failure, which is exactly how the automatic revert of §12.2 sees it: after
   `StartLimitBurst` attempts the unit reaches `failed`, `OnFailure=` starts the judge, and the
   version that *can* open the database is put back — so an accidental downgrade self-corrects, and a
   deliberate one is completed by the five-command procedure of §12.4, of which `llamaman restore-db`
   is the third step and never the whole thing (D94).

   **Then, before the first migration runs: disarm the revert (D92).** If `update/pending` exists and
   this boot is about to apply at least one migration, read the marker into memory and **unlink it
   now**. Applying a migration is the exact instant `<prefix>/llamaman.prev` stops being a binary that
   could open this database, so it is the exact instant the judge's second `ConditionPathExists=` must
   stop holding — otherwise a daemon that migrates and *then* starts failing (a panic anywhere between
   here and step 11, the gate included) sends the unit to `failed` with both of the judge's conditions
   still true, and the judge renames a binary back that can no longer start. That second failure finds
   `<prefix>/llamaman.prev` consumed, so the judge is skipped, its `ExecStopPost=` does not run either,
   and the host is left with no daemon and no public gateway ports. Step 11's gate resolves the
   in-memory copy exactly as if it had read the file, so nothing else in §12.3 changes; the price is
   that a crash between here and step 11's commit leaves the row to the closing pass, which closes it
   `failed`/`daemon_restarted` for an update that in fact took — one mislabeled history row, in a
   narrow window, against a dark host (§12.3 stop-point rows 11a and 11b). **The rule fires on "about
   to migrate", not on "a migration committed"**, so a boot whose *first* migration fails also loses
   the revert even though the schema never moved; D92 states why that direction of trade is the right
   one, and §12.3 row 11b gives that case its own, shorter exit — re-install the previous binary, with
   no database restore. An `update/pending` this binary
   cannot parse is unlinked on the same rule and reaches the gate as the same in-memory fact, which is
   the sweep branch it would have taken from disk.

   **Then, the migrations themselves** — the embedded set, one transaction each; a checksum mismatch
   is fatal and says so. While `PRAGMA integrity_check` or a migration is running the daemon sends
   `EXTEND_TIMEOUT_USEC=` every 10 s over `$NOTIFY_SOCKET`, so a legitimately slow start extends
   `TimeoutStartSec=` instead of being killed and judged (D88, §5.4).
5. Generate `secret.key` (0600) if absent; load settings (built-in defaults plus `settings` rows).
6. Probe the environment **once**, persisting the results and never requiring them: the HF hub
   directory by the six-rule chain of §7.2 (`$HF_HUB_CACHE` first, legacy variables next, then
   `$HF_HOME/hub`, the dedicated-user path, `$XDG_CACHE_HOME` and the default), registering every
   other existing hub directory as a non-primary scan-and-serve root; systemd reachability (the
   *scope* was settled in step 1 and is not re-derived here); polkit authorization including whether
   `manage-unit-files` was granted (§5.2); **journal readability into `runtime_info.journal_read`,
   by the one-line `journalctl` probe of §5.3 (D77)**; GPUs; toolchain;
   `filepath.EvalSymlinks(os.Executable())` into `runtime_info.binary_path`. Environment variables
   are first-boot *hints* only; the resolved values live in `settings` and are edited thereafter
   exclusively in the UI (SPEC §3.9).
6a. **If systemd is unreachable**, record `systemd_control='unavailable'` and continue into the
   degraded mode of §11.1a. The daemon does **not** refuse to start, and it does **not** spawn
   `llama-server` itself.
6b. **Port preference from the unit, resolved once.** The unit may carry `serve --port N`, written by
   `llamaman install-units --port N` on behalf of `install.sh --port N` (§5.4). The flag is a *seed*,
   never an override, so the DB stays the single source of truth SPEC §3.9 requires:
   - `settings['ui.port_desired']` absent (a fresh install) → write it from the flag with
     `updated_by='system'`, and use it.
   - present and equal → nothing to do.
   - present and different → **the stored setting wins** (it was set in the UI, deliberately, by a
     human), the flag is recorded in `runtime_info.ui_port_flag`, and a `warn` notification reports
     the divergence with the one command that realigns the unit:
     `sudo llamaman install-units --identity <user> --port <stored value>`. Nothing is broken by the
     divergence — it is cosmetic — and the notification says so.
   The flag is also what `install.sh` polls against in §13 step 9.

7. **Port walk**, over the candidate set defined in §2.8: `[ui.port_desired, +20]` minus every
   `instances.public_port` and `internal_port` of a non-deleted instance and minus the internal pool,
   binding `ui.bind`:candidate until one succeeds. The exclusion is not cosmetic — the gateway
   listeners do not open until step 10, so a bare "next free port" walk could take a port an instance
   owns and only discover the theft when that instance's listener failed to bind (F6). If every
   candidate is excluded or occupied, bind an ephemeral port rather than refusing to start, and raise
   `ui_port_exhausted`. Persist the winner in `runtime_info.ui_port`, log
   `llamaman listening on http://<primary-ipv4>:<port>` to journald at `info`, and publish the same
   URL through `sd_notify STATUS=` so `systemctl status llamaman` shows the truth (D9/D24).
8. **Setup claim.** If `admin_account` is empty, run the mint step of §2.2a: 32 random bytes,
   base58, `setup_claim` row with the hash and `token_path`, the plaintext written 0600 to
   `<state_dir>/setup-token`, and one journald line
   `SETUP: open http://<ip>:<port> — setup token <token> (not needed from this machine)`. If
   `admin_account` exists but a stale `setup-token` file is present, remove it. If `claimed_at IS
   NULL` and the file is missing, mint a fresh token and replace the row — a one-time credential
   nobody can read is worse than a new one.
9. Read `/proc/sys/kernel/random/boot_id` and `/proc/stat`'s `btime`, compare the boot id with
   `runtime_info.host_boot_id`, and hold the result **in memory** as
   `{host_boot_id, host_boot_at, host_boot_changed}`. A difference means this is the first daemon
   start of a new host boot, which is the one moment `autostart` writes `desired_state` (D53).
   **This step writes nothing.** Persisting the new value here — as an earlier reading did — destroys
   the input of the comparison the supervisor makes a moment later at boot reconciliation step 1
   (§5.8): it would always find equality, the D53 coupling would never fire, and autostart would be
   broken in both directions. The read happens twice because two subsystems need the answer; the
   **write happens once**, in the one place that acts on it, and §5.8 step 1 is that place.
10. Adopt any listeners systemd held in the fd store across the restart (`LISTEN_FDS` /
    `LISTEN_FDNAMES`, §9.4) and bind the rest. Write `runtime_info.schema_version` and
    `listener_continuity` while doing so. **`READY=1` is not sent here** — step 11 runs first.
11. **Self-update confirmation gate** (§12.3), which is also the finalizer that resolves the
    `self_update` job §2.3 left `interrupted`. The gate is one routine — `ResolveUpdateMarkers`,
    specified in §12.3 — and this step is only its **first** caller: the same routine runs on a 30 s
    ticker while `update/pending` exists and ahead of `POST /update/apply`'s guard, which is
    what gives every stop point in §12.3's table an exit that does not wait for the next boot.

    **It runs before `READY=1`, deliberately.** Everything it needs is already resolved — the database
    (step 4), the systemd controller and `journal_read` (step 6) — and putting it here buys one
    property worth stating on its own: **a daemon that ever signals readiness has already resolved the
    marker**, so the judge cannot be armed against a version that demonstrably booted. Together with
    step 4's disarm that leaves exactly one shape of unconfirmed update — a binary that never finished
    a boot — which is what §12.2 claims the judge is for. The daemon sends `EXTEND_TIMEOUT_USEC=` while
    the gate runs, for the same reason it does during a migration: branch 3 reads a journal tail, and a
    slow journal must extend `TimeoutStartSec=` rather than trip it.

    **There is one marker, and the question asked of it is not a clock.** `update/pending` names a
    `target_version`; this binary knows its own; and `llamaman-selfupdate.service` either is or is not
    active, which systemd answers authoritatively and bounds with that unit's own `TimeoutStartSec=`
    (D91). Those three facts decide the branch. On this boot's first call the marker may already have
    been consumed by step 4's disarm, in which case the gate reads the in-memory copy that step made
    and is otherwise identical (D92). Whichever one matched, the routine ends with the closing pass, in
    the same transaction:

    - `pending.target_version` **is this version** → the update took: mark the `self_updates` row
      `succeeded` and its job `succeeded`, commit, then unlink the marker and clear `update/` scratch.
      `<prefix>/llamaman.prev` is kept — the next update replaces it.
    - `pending.target_version` **is not this version and the oneshot is active** → a swap is genuinely
      in flight, and this boot is the intermediate one §9.4 step 7's failsafe can produce. **Do
      nothing**: leave the marker, leave the row `staged`/`swapping` and its job `interrupted`, log at
      `info`. The deferral cannot outlive that unit, which is the entire reason it is expressed as a
      question about a unit rather than as a deadline this design would have had to freeze.
    - `pending.target_version` **is not this version and no actor is active** → the update did not
      take, whether because the swap never happened or because the judge reverted it. Close the row
      and its job `failed`/`error_code='update_not_applied'`, commit, unlink the marker, clear
      scratch, raise **F24** with the actor units' journal tail. A marker this binary cannot parse at all takes this
      branch too: sweeping a file no process is waiting for is safe, and leaving it would reproduce
      the one property §12 exists to prevent — a file under `update/` outliving every process that
      knows what it means.

    **A `pending.self_update_id` that names no row is a no-op, not an error** (§12.3): the marker is
    still unlinked, the scratch still cleared, and branch 3 still raises F24 from the versions in the
    marker. That state is ordinary after the most disruptive recovery in the design — F12's fresh-DB
    arm, or a `restore-db` to a snapshot older than the update — and the branch that runs right after
    it must not abort a boot over a row a restore took away.

    Every branch that writes at all writes the job row and the `self_updates` row in **one
    transaction**, per §2.3a — the closing pass included, which shares that transaction rather than
    opening its own.

    - **The closing pass.** Every non-terminal `self_updates` row whose paired `self_update` job is
      **`interrupted`** and which a surviving `update/pending` does not name is closed `failed` /
      `error_code='daemon_restarted'`, job and row together. Three properties make that guard exactly
      right. `interrupted` means the lease belongs to a boot that is gone (§2.3), so the pass can never
      close work the calling process is itself performing — which is what lets the same pass run in all
      three callers rather than at boot alone, including ahead of the endpoint's guard while a forward
      update is `downloading` on *this* boot's lease. It cannot touch the deferral branch, whose marker
      is on disk precisely so it keeps naming its row. And it cannot touch the row a matched branch
      just resolved, because that row is now terminal. What is left is the orphan and nothing else: a
      plain daemon restart mid-download, and the row a restored database resurrects — F12's recovery
      from `db-backups/` and `llamaman restore-db` (§12.4) both take a snapshot's word for a
      `self_updates` row that was `verifying` when it was written. This is still the only path by
      which a self-update job reaches `failed` for a plain daemon restart, and it still runs *after*
      the marker has been consulted, which is why §2.3 exempts the kind from generic orphan recovery:
      marking it `failed` before this gate ran would contradict the `succeeded` the gate is about to
      write on every update that worked, breaking the §2.3a one-state-per-activity invariant an
      integration test asserts.

12. **`sd_notify READY=1`**, then start the watchdog, the systemd subscriber and the background
    workers (job queue, download queue, health poller, GPU sampler, HF metadata refresher, nightly
    maintenance), then reconcile everything against reality.

    **Sixty seconds later, clear the unit's start-limit counter (D93).** A goroutine that has watched
    this boot stay ready for 60 s — the same threshold D64 uses for "this start served" — calls
    `ResetFailedUnit("llamaman.service")`, which resets `StartLimitBurst=`'s counter as well as the
    failed state. Without it that counter is a budget every ordinary restart spends: the four
    `restart_required` settings behind `POST /system/restart` (§3.4), each installer re-run (§13
    step 11) and each self-update. With it the counter holds only *consecutive starts that never became
    healthy*, which is what §5.4 always claimed the revert deadline measured. The call is inside the
    name-scoped `manage-units` grant (§5.2 branch (b)), so it needs no new privilege; where it is
    refused or systemd is unreachable the fact is recorded in `runtime_info`, and — because that same
    grant is what `RestartUnit` needs — `POST /system/restart` answers `409 systemd_denied` rather
    than the 429 (§3.3), while `doctor` raises a **warning** carrying
    `sudo systemctl reset-failed llamaman.service`, the one command that keeps such a host's budget
    from running out. §11.1a states the residual and F26 records it. The 60 s delay is what stops a binary
    that reaches `READY=1` and then panics from resetting its own counter on every attempt and never
    letting the unit reach `failed` at all.

**First-run protection (D38/D59).** A request from loopback may claim the daemon with no token at all
— which is the overwhelmingly common case and preserves SPEC §3.9's acceptance test literally
("download → start → open browser → done"). Any other origin must present the one-time token in
`X-Setup-Token`. The token reaches a human through the D59 file, and every printer reads that one
file: `llamaman status` (`setup.token` under `--json`), `install.sh` step 10 (which shells out
to plain `llamaman status` and echoes its `Setup` block, so the script needs no JSON parser),
and journald for anyone watching the boot. All three require host access —
the correct authorization for claiming a host service — and none of them can recover the token from
the database, which stores only its hash. There is no time-based window and therefore no
clock-dependent second code path to test.

### 11.1a Degraded modes, enumerated (D67, F9, F10)

Four independent facts determine what the control plane can do — plus a fifth, `journal_read`, which
determines what it can *show* and is orthogonal to all four (below). `GET /system/capabilities`
returns exactly them so the UI never has to guess:

| condition | `systemd_control` | `instance_control` | `autostart_control` | `self_update` | what still works |
|---|---|---|---|---|---|
| normal, polkit fully granted | `dbus` | yes | yes | yes | everything |
| polkit `manage-unit-files` withheld (`--no-autostart-grant`, §5.2) | `dbus` | yes | **no** — `409 autostart_unavailable` with the `systemctl enable` command | yes | everything else. `autostart_control: false` is read by **three** call sites, not one: `PUT /instances/{id}/autostart` returns the 409; the supervisor's `autostart` ≠ unit-enabled action is **skipped** rather than retried into a polkit denial every pass, and reports the divergence once as `autostart_unmanaged` (§5.8); and `DELETE /instances/{id}` skips `DisableUnitFiles`, completes, and raises `unit_still_enabled` with the manual command (§3.10c). A unit left enabled for a deleted instance is harmless because `instance-exec` exits 64 on a row with `deleted_at` set |
| polkit denied at boot (F9) | `dbus` | **no** — `409 systemd_denied` with the `--repair-polkit` command | no | no | models, downloads, cache, fit, bench, tokens, gateway, settings, diagnostics. The three `autostart_control` consequences of the row above apply verbatim here, and for the same reason: a capability the daemon does not have must be *checked* at each call site, not discovered by a denial |
| D-Bus unusable, `systemctl` present | `exec` | yes (polled, not pushed) | yes | yes | everything; the UI says status is polled |
| **user scope** (D2, `install.sh --user-units`) — no polkit rule exists at all | `dbus` | yes | yes | yes | everything. A user manager authorizes its owner unconditionally, so the two boot `CheckAuthorization` calls of §5.2 are **not made**: `polkit_ok` and `polkit_unit_files` are `NULL` — "not applicable", never 0 — and the UI reads `instance_control`/`autostart_control` here rather than the raw columns |
| **systemd absent entirely** (F10) | `unavailable` | **no** — `409 systemd_unavailable`, each carrying the equivalent manual command | no | **no** — `409 selfupdate_unavailable` with the `install.sh` one-liner | models, downloads, cache scan, GGUF parsing, fit calculator, **bench** (child processes, no units needed), tokens, the gateway proxying to whatever `llama-server` is already listening, settings, `doctor`, `diagnostics`. Instance deletion also still works: all three systemd calls are skipped, the row is soft-deleted, and the notification names what to do by hand (§3.10c) |

`journal_read` is the fifth fact and is **orthogonal to all four** — a fully granted polkit host can
still have an identity that cannot read the journal, and a systemd-less host has no journal to read.
It gates only the journal-derived features and says so explicitly rather than returning empty
streams: `GET /system/journal` and `GET /instances/{id}/logs` return `409 journal_unavailable` with
the F23 card, the §5.8 fit observation is skipped, and F19's notification carries the hint instead of
a tail (§5.3, D77).

`self_update_revert` is a sixth fact, orthogonal to the table and reported for the same reason: a
host with every capability above can still have had `OnFailure=llamaman-update-verify.service` edited
out of its unit or the judge unit masked, and an update staged on such a host would have no automatic
revert. `POST /update/apply` refuses `409 revert_unavailable` rather than staging one (D88, §12.1
step 1), and the Updates page shows the `sudo llamaman install-units --identity <user>` line instead
of a button that fails. **The `self_update` column carries the symmetric fact about the swap actor**:
in system scope it is `false` when `llamaman-selfupdate.service` is absent or masked, which is what
`409 selfupdate_unsupported` refuses on — otherwise a host missing that unit passes every check,
stages the whole update, hands its listeners to the fd store and only then discovers there is nothing
to summon. Both facts are read off the installed units' own directives and never off a template hash,
so a `drift: stale` host — the ordinary state after a self-update across a release that changed a
template — reports them correctly (D95).

**The denied-`manage-units` host keeps one residual, and it is stated rather than mitigated (F26).**
On a host where the name-scoped `manage-units` grant is refused — the F9 row above — the daemon cannot
call `ResetFailedUnit`, so D93's "a healthy boot clears the counter" never happens and
`StartLimitBurst=5` in `StartLimitIntervalSec=600` is the host's entire start budget, exactly as D93
says. What that row also means is that **no in-product guard can protect the budget**: the same polkit
action authorizes `RestartUnit`, so `POST /system/restart` answers `409 systemd_denied` and spends
nothing, and the only remaining spenders are things this product does not mediate — a human typing
`systemctl restart llamaman.service`, and each `install.sh` re-run (§13 step 11). Five such starts
inside ten minutes leave `llamaman.service` in `failed` with "start request repeated too quickly",
the judge inert (its `ConditionPathExists=` on `update/pending` is false, no update being in flight),
its `ExecStopPost=` therefore unrun, no daemon — and, once the unit has fully failed,
`FileDescriptorStorePreserve=restart` releases the public gateway ports with it. The design does not
close this: closing it would need a privilege the host has deliberately withheld. It **names** it
instead — `doctor` raises a warning on every run for this host, carrying
`sudo systemctl reset-failed llamaman.service`, and F26 is the row and the remediation card. Hosts
unwilling to grant `manage-units` at all have D2's user-scope topology, where a user manager
authorizes its owner unconditionally and the reset needs no polkit.

**F10 is a degraded mode, not a refusal.** An earlier reading of "refuse to serve" contradicted three
other parts of this design at once — `runtime_info.systemd_control` has an explicit `'unavailable'`
value, `POST /system/restart` defines a `409 restart_unavailable` response for exactly that state,
and F9 already defines a read-only control plane — and it would have made the entire model, download,
fit and benchmark half of the product unusable on a host where all of it works perfectly well. So the
daemon starts, serves the UI, and disables precisely the operations that require a service manager.

The clause that actually carries security and correctness weight is kept verbatim and is **not**
softened: **there is no silent child-process fallback.** The daemon never forks `llama-server`
itself, in any mode. Doing so would put model processes inside the daemon's lifetime and break SPEC
§3.8's guarantee that a self-update leaves running models alone — the one property the whole systemd
design exists to provide. On a systemd-less host, instances are configured in the UI and
`GET /instances/{id}/command` prints the exact argv and environment to run by hand or under another
supervisor; the gateway, tokens, accounting and status polling all work against that process
unchanged, because they only ever talk to `127.0.0.1:<internal_port>`. `llamaman doctor` states the
situation in one line at the top of its output.

### 11.2 Wizard steps (rows in `wizard_steps`, resumable)

| step | does | skippable |
|---|---|---|
| `password` | argon2id hash with a strength meter, creates the session, stamps the claim | no |
| `toolchain` | probe with per-tool found/version/needed and distro-doc links; "Re-check"; "Continue CPU-only" | to CPU-only |
| `llamacpp` | channel + version picker with the plan preview (§6.3); starts the install or build and streams the log; the user may leave and come back | no |
| `hf` | optional token (validated via whoami), cache-root detection and confirmation, then a scan | yes |
| `models` | shows scan results first ("6 models already on disk"); otherwise HF search with the fit calculator live; the download continues in the background | yes when the scan found GGUFs |
| `instance` | prefilled from the chosen model with flags recommended by the fit calculator, a port suggestion, and an autostart toggle | yes |

Every step is idempotent and re-enterable from `/settings` later; there is no wizard-only
capability. A browser refresh or a daemon restart mid-build does not restart the wizard.

### 11.3 CLI

Authorization for the recovery commands is *filesystem access to the 0600 database* — root or the
service identity — exactly as SPEC §3.9 requires. No password, no network.

**One rule binds every subcommand a root user can invoke** (`install-units`, `status`, `doctor`,
`verify-release`, `version`, and `diagnostics` when run with `sudo`): **they create nothing under the
state directory — not the database, not its `-wal`/`-shm` sidecars, not `secret.key`, not a
directory.** §13 step 7 already gives `install-units` that guarantee because a root-created
`<state_dir>/llamaman.db` would be a 0600 file the service identity cannot write; the same
hazard applies verbatim to a root-created `llamaman.db-wal` or `llamaman.db-shm`, which is why the
guarantee is stated once, here, for all of them rather than for one command.

Two commands are deliberately outside that list because their whole purpose is to **write** the
database with the daemon down — `reset-password` and `restore-db` (§12.4). They may be run by root
or by the database's owner, the euid is checked against `stat`, and when the caller is root every
file they create — a restored `llamaman.db`, a `-wal` or `-shm` they had to open — is `chown`ed to
the database's uid/gid and `chmod`ed 0600 before they exit. A root-owned database file, or a
root-owned sidecar beside one, is a database the service identity can never write again.

Concretely, `status` and `doctor` open the database with
`file:<path>?mode=ro&_pragma=query_only(1)` and **only if `llamaman.db` already exists**:

- **File absent** → no open is attempted at all. `status` prints "not initialized — the daemon has
  not run yet" and exits 1; `doctor` reports every DB-dependent check as `skipped (database not yet
  created)` and still runs the ~20 checks that need no DB (systemd, D-Bus, polkit, unit presence,
  ports, toolchain, GPU, cache writability, disk). This is the state `install.sh` step 8 runs in, and
  it is a normal, successful outcome rather than an error.
- **File present, sidecars absent, caller is not the owner** → a read-only open of a WAL database
  whose `-shm` does not exist would have to create it. Refuse: report
  `database present but not readable without creating WAL sidecars — run as <identity>` and exit 2.
  This is the one case where root deliberately does less than it could.
- **File present, daemon running** → the sidecars already exist, owned by the service identity; root
  reads them and creates nothing. This is the state `install.sh` step 10 runs in.

A CI test runs each root-invocable subcommand as root against an empty state directory owned by a
different uid and asserts, with a directory diff, that **zero** files were created.

| Command | Privilege | Behavior |
|---|---|---|
| `llamaman serve [--scope user\|system] [--port N]` | service identity | the daemon. `--port` comes from the unit (written by `install-units --port`), seeds `ui.port_desired` on a fresh DB, and never overrides a stored value (§11.1 step 6b). `--scope` also comes from the unit (written by `install-units --user-units`) and is **authoritative, not a seed**: it settles `runtime_info.systemd_scope`, which selects the state-directory fallback, the self-update actor, the polkit probe and the `journalctl` scoping. Absent, §11.1 step 1's single fallback decides |
| `llamaman status [--json]` | anyone who can read the DB | works even when the daemon is down (reads the DB **read-only** and systemd directly, creating nothing — see the rule above). Before the claim is stamped it also prints the setup token, read from `<state>/setup-token`, never from the database (§2.2a) |
| `llamaman doctor [--format text\|json] [--skip-db]` | any | systemd, D-Bus, polkit authorization (including whether `manage-unit-files` was granted), the resolved systemd scope and state directory with all three candidates when the resolved one is not writable (§11.1 step 1), **journal readability for the service identity, naming the `usermod -aG systemd-journal` fix when it is denied (D77)**, unit presence, ports, toolchain, GPU, HF cache writability and symlink support, disk space, **the self-update state** — whether `update/pending` exists, the row and versions it names, whether `llamaman-selfupdate.service` is active, whether `<prefix>/llamaman.prev` is present, whether `llamaman.service` carries `OnFailure=llamaman-update-verify.service`, and any **live `self_update` job the marker does not name** (§12.3) — **the database's schema version against this binary's**, printing §12.4's **five-command downgrade procedure** and the newest usable snapshot when the database is ahead — never the `restore-db` line alone, which on its own is a destructive no-op, and never without its `reset-failed` step, without which the final `start` is refused (D94) — **`MAX(schema_migrations.version)` against the same value in the newest `db-backups/` snapshot**, which is what tells a crash-looping host that a migration of this release committed (§12.3 row 11a, the five commands) from one whose first migration failed and moved nothing (row 11b, where re-installing the previous binary alone is the whole remedy and no database is restored), **the digest of `<prefix>/llamaman` against the running daemon's own image** (F25), **whether this boot cleared its unit's start-limit counter** (D93, naming `sudo systemctl reset-failed llamaman.service` when it could not) — raised as a **warning**, not an aside, when the cause is a refused `manage-units` grant, because on that host nothing in the product can clear the counter or protect the budget and the exhaustion has no automatic exit (F26, §11.1a) — and DB integrity **when a database already exists**. DB checks are skipped, not failed, when it does not — and `--skip-db` forces that explicitly. `doctor` only ever *reports*; resolving `update/pending` is the daemon's gate, and restoring a database is `restore-db`'s. The thing to ask for in a bug report |
| `llamaman diagnostics --out FILE` | service identity | a **redacted** support bundle: doctor JSON, unit and polkit files, the last N journal lines per unit, build logs, the schema plus row counts. No secrets, no token values, no session ids, no paths outside the cache root, and the HF token replaced by its hint |
| `llamaman reset-password [--stdin]` | root or the DB owner (euid checked against `stat`) | prompts twice on a TTY, writes a fresh argon2id hash, deletes every session, records an audit row; refuses on a non-TTY without `--stdin` |
| `llamaman install-units --identity U [--prefix DIR] [--port N] [--user-units] [--no-autostart-grant] [--repair-polkit]` | root | writes or repairs the units and polkit files from templates embedded in the binary — each stamped `# llamaman-units: <N>` with the template version it rendered (D95) — then `daemon-reload` (D48; also the F16 repair path). `--prefix` renders `ExecStart=<prefix>/llamaman …` in **both** unit templates (default `/usr/local/bin`; `~<user>/.local/bin` under `--user-units`), `--port N` renders `… serve --port N`, `--user-units` renders `… serve --scope user` (§5.4, §11.1 step 1) and installs into `/etc/systemd/user` with no polkit file. **It renders the scope into the two self-update units as well** — `selfupdate-apply --scope system` on `llamaman-selfupdate.service` (written only in system scope) and `update-verify --scope system\|user` on `llamaman-update-verify.service` (written in both) — because those actors run with the daemon stopped and can learn the scope no other way (§12, §5.2a). `--no-autostart-grant` omits the `manage-unit-files` polkit branch (§5.2). It also adds the identity to the `systemd-journal` group, idempotently, so journal reading works for a system account (D77, §5.3), and prints what it changed. It **never** touches the database or any other file under the state directory, per the rule above |
| `llamaman instance-exec <name>` | unit only | §5.6 |
| `llamaman selfupdate-apply --scope system` | unit only (root, system scope only) | §12 actor 2, **one branch and one refusal**: §12.2 step 0 parses `--scope`, reads `update/pending`, cross-checks its `binary_path` against this process's own `os.Executable()`, re-verifies the tarball's sha256 and its ed25519 signature against the compiled-in key — never trusting the unprivileged staging step, because this is a genuine privilege boundary — and checks `<prefix>` for room. Any failure writes nothing, deletes nothing, **stops nothing** (this actor never stops a unit at all), logs one structured journald line and exits non-zero; the daemon's §12.3 gate is what raises the notification, since this process must never open `llamaman.db` (§11.3). Otherwise: retain `<prefix>/llamaman` as `<prefix>/llamaman.prev` (copy, digest-check, rename), extract the new binary from the re-verified tarball into `<prefix>/llamaman.new.tmp`, `rename` it over `<prefix>/llamaman`, and `systemctl restart --no-block llamaman.service`. Every install is one atomic rename inside one root-owned directory (D89). Refuses to run in user scope, where the daemon performs the same sequence in process (§5.2a) |
| `llamaman update-verify --scope system\|user` | unit only | §12 actor 3, the **revert**, executed **from `<prefix>/llamaman.prev`** (D13) and started by `OnFailure=llamaman-update-verify.service` on `llamaman.service` (D88) — never by a timer, and never by the daemon. Its unit's two `ConditionPathExists=` lines (`<prefix>/llamaman.prev` and `update/pending`) are the whole arming logic. Body: `fstat` its own image and `<prefix>/llamaman` and refuse unless both share an owner and its own image is not group- or world-writable; exec `<systemd.SystemctlPath()> [--user] is-active llamaman.service` and do nothing unless the trimmed **stdout** is `failed` — `is-active` exits 3 for a non-active unit, so the exit status is not the answer; then one `rename(<prefix>/llamaman.prev, <prefix>/llamaman)`. It reads no field of any file, opens no database (not even read-only — §11.3), stops nothing, restores no database and writes no marker (§12.2). `--scope` comes from the unit — `install-units` renders it in both topologies — and selects only whether `systemctl` is addressed with `--user`; because this `ExecStart` runs the *previous* binary, the argument is frozen across versions in both directions. Missing or unparsable → perform nothing, log, exit non-zero; the unit's `ExecStopPost=` still runs `reset-failed` and `start` on `llamaman.service` |
| `llamaman restore-db <snapshot> [--yes] [--json]` | root or the DB owner (euid checked against `stat`) | the explicit, offline database restore of §12.4 — **the only one in this design, it never runs by itself, and on its own it does not complete a downgrade**: it is step 3 of the five-command procedure §12.4 states, and run without steps 1 and 2 it is a destructive no-op that the newer binary migrates straight back (D94). Refuses while `llamaman.lock` is held, naming the PID and printing the `systemctl stop` line; refuses a snapshot outside `db-backups/`, one that fails `PRAGMA integrity_check`, or one whose schema is newer than this binary understands. Prints both databases' path, size, mtime, schema version and recorded daemon version, then the loss — instances, tokens, benchmark runs, downloads, events and notifications present now and absent from the snapshot — plus the warning line when `<prefix>/llamaman`'s schema is newer than the snapshot's, and requires the operator to type the snapshot's file name (`--yes` supplies it for the scripted case the F24 card documents). Then: checkpoint the live database `TRUNCATE`, `VACUUM INTO llamaman.db.superseded-<ts>`, unlink the sidecars, copy the snapshot to `llamaman.db.restore` in the same directory, `chown`/`chmod 0600`/`fsync` it, and `rename` it over `llamaman.db`. Every crash window leaves a working database and a re-runnable command (§12.4). It never **modifies** `<prefix>/llamaman`, `<prefix>/llamaman.prev` or any unit — it reads the installed binary's version, and nothing else about it, only to print that warning |
| `llamaman verify-release <dir>` | any | ed25519 + sha256 verification, used by `install.sh` |
| `llamaman version` | any | version, commit, build date, Go version |

```
$ llamaman status
llamaman v1.1.0 (commit 9f3c2ab) — running, pid 8421, uptime 3d 4h
  UI            http://192.168.1.20:5526   (desired 5526, actual 5526)
  Database      /var/lib/llamaman/llamaman.db  (18.4 MB, schema v14, integrity ok)
  Identity      <service user> (uid 986), systemd scope: system, control: dbus, polkit: ok
  llama.cpp     b10621-cuda-src (active, built 2026-08-20)  |  rollback: b10604-cuda-src
  HF cache      <hub_dir> — 11 models, 214.6 GB   (primary; 1 other root, scan-only)
  Instances     3 total: 2 ready, 1 stopped
                qwen3-8b      ready    :8081 -> 21001   Qwen3-8B-Q4_K_M
                embed         ready    :8082 -> 21002   nomic-embed-text-v1.5-f16
                llama70b      stopped  :8083 -> 21003   Llama-3.3-70B-Q4_K_M
  Jobs          1 running (model_download 41%), 0 queued
  Setup         complete
```

Exit codes: 0 running, 1 not running (or not yet initialized), 2 DB present but unreadable. `--json`
emits the same content for scripts.

Before the claim is stamped, `status` appends a setup block and `--json` carries the same fields:

```
  Setup         NOT COMPLETE — open http://192.168.1.20:5526
                setup token  7Fq2…nR4d   (not needed from this machine)
```

```jsonc
"setup": { "complete": false, "claimed": false,
           "token": "7Fq2…nR4d",        // null when the file is unreadable by this uid
           "token_hint": null,          // e.g. "run as root or llamaman" when token is null
           "url": "http://192.168.1.20:5526" }
```

The value comes from `<state_dir>/setup-token` (§2.2a), which is the only place a plaintext token
ever exists; `setup_claim` holds a sha256 and could never produce it. Once `claimed_at` is set the
file is gone and the block collapses to `Setup complete`.

---

## 12. Self-update

**Forward only, with one safety net.** SPEC §3.8 asks for exactly one thing: check the release feed,
show the changelog, swap the binary, restart `llamaman.service`, and leave running instances alone.
This section is that pipeline, plus one automatic recovery and nothing else — **if the binary that
was just installed never reaches its first migration and never finishes a boot, the binary it
replaced is renamed back over it and the service is started again** (D12, D88). Both halves of that
condition are enforced, and both are deliberate: the confirmation gate runs before `READY=1`, and the
marker that arms the revert is unlinked *before* the first migration is attempted (D92), so the
recovery cannot fire against a version that started.

**The second half is a real narrowing, and it is stated rather than papered over.** The disarm is
keyed to "this boot is about to apply at least one migration", not to "a migration committed", so a
release whose **first** migration fails on this host's data leaves the schema exactly where the
previous binary left it and still gets no automatic revert: the marker is already gone, the judge is
inert, and the host crash-loops until a human intervenes. That is the price of not reopening a crash
window between a migration's commit and the unlink, and D92 states the trade. The exit is manual and
is the shortest one in this document — re-install the previous binary, no database restore — and
§12.3's stop-point row 11b names it.

There is no on-demand rollback endpoint, no rollback marker, no rollback branch in any actor, and no
automatic database restore. Installing an older release is this same pipeline pointed at an older tag;
below a schema bump that is all there is to it, and across one, completing the downgrade is the
offline five-command procedure of §12.4 (D94). D87 records what was removed and why.

**Three actors in system scope (D12)**, because the daemon is unprivileged and cannot overwrite the
installed binary — and because the process that must be judged cannot be the judge. (In the D2
user-scope topology there is no privilege boundary to cross; §5.2a describes the two-actor variant,
which reuses every contract below unchanged.)

| actor | is | does |
|---|---|---|
| the **daemon** | unprivileged, `llamaman.service` | resolves the release, downloads, verifies the signature, probes the new binary, snapshots the database, writes `update/pending`, summons the oneshot (§12.1) |
| **`llamaman-selfupdate.service`** | root, `Type=oneshot`, on the polkit allowlist | re-verifies the signature itself, retains `<prefix>/llamaman` as `<prefix>/llamaman.prev`, extracts and installs the new binary, restarts `llamaman.service` (§12.2) |
| **`llamaman-update-verify.service`** | root, `Type=oneshot`, started by `OnFailure=` on `llamaman.service`, **and it is the retained previous binary** | renames `<prefix>/llamaman.prev` back over `<prefix>/llamaman` when — and only when — an update is unconfirmed and the daemon unit has reached `failed` (§12.2). "Unconfirmed" is exactly "`update/pending` still exists", and §11.1 step 4 removes that file before any migration, so the revert can never land a binary on a database that has moved past it (D92) |

The binary stays at `<prefix>/llamaman`, default `/usr/local/bin`, root-owned 0755 (D15). The binary
it replaces is retained beside it as **`<prefix>/llamaman.prev`**, same owner and mode, in the same
directory (D89): that is what makes both the install and the revert a single `rename()` inside one
filesystem, and it is what puts the judge's own executable somewhere the service identity cannot
write. **No actor hardcodes the path**: `install-units --prefix` writes it into the units, and each
actor derives `<prefix>` from its own `filepath.EvalSymlinks(os.Executable())`, so a
`--prefix /opt/bin` install self-updates `/opt/bin/llamaman` rather than dropping a root-owned binary
into `/usr/local/bin` that nothing runs.

**The public inference ports survive the restart** by the fd-store mechanism of §9.4 (D58); this
section covers the binary swap and the revert judgment only.

#### 12.1 The daemon's half: resolve, verify, snapshot, stage

1. **`planned`.** `POST /api/v1/update/apply {"tag":"v1.2.0"}` refreshes `release_cache` for source
   `llamaman`; the UI shows the rendered changelog and the version delta. The endpoint runs the §12.3
   resolver first, then a guard of exactly four clauses — enumerated identically here, in §3.14 and in
   §15's table-driven fixture, so the three can never drift:
   - `409 job_in_flight` while a build is running (D4) **or any `self_update` job is live**
     (`interrupted` counts — §2.3);
   - `409 selfupdate_unavailable` when `systemd_control='unavailable'` (§11.1a) — the swap needs a
     service manager, and this is the row §11.1a already reports as `self_update: no`;
   - `409 selfupdate_unsupported` when **the swap actor cannot be summoned**: in system scope,
     `llamaman-selfupdate.service` is absent or masked. That covers both hosts that reach this clause —
     one installed from a pre-v1.0.0 release, which never had the unit and for which the `install.sh`
     one-liner is the correct upgrade path, and one whose unit was deleted or masked, for which the
     line is `sudo llamaman install-units --identity <user>`. In user scope the clause is inapplicable
     and never fires: there is no oneshot, because the daemon performs the swap in process (§5.2a
     item 2). **The predicate is a fact about the installed unit, not about this binary**: "this binary
     has no `selfupdate-apply` subcommand" was the earlier wording and it is a compile-time constant
     evaluated by the very binary that would have to contain the endpoint to evaluate it — it could
     never be true, and it left the one thing worth checking unchecked;
   - `409 revert_unavailable` when the installed `llamaman.service` carries no
     `OnFailure=llamaman-update-verify.service`, or `llamaman-update-verify.service` is absent or
     masked, naming `sudo llamaman install-units --identity <user>`. **An update is never staged
     without a working revert**, and this is the one place that is decided — by the process that can
     still talk to a human, before anything on disk has moved.

   The last two clauses read the installed units' own directives (§5.4a), never a template hash: a
   host that self-updated across a release which changed a unit template is `drift: stale` and must
   still be able to update (D95).

   **The guard and the two inserts are one `BEGIN IMMEDIATE` transaction (D97).**
   `idx_jobs_one_live_per_subject` cannot enforce this on its own — `jobs.subject_id` for a self-update
   is a fresh `self_updates.id`, so two concurrent applies have different subjects and the index is
   silent, exactly the hole D70 closed for builds and D75 for benches. Here there is one producer in
   one process (`flock`, F11), so SQLite's writer serialization is the whole mechanism, provided the
   `job_in_flight` read and the row-and-job insert sit inside the same immediate transaction rather
   than either side of it. **`update/` is emptied only after that transaction commits** — of everything
   except a live `pending`, which the guard has just proved absent — so every stage begins from a clean
   directory, no scratch file can outlive the update that created it, and a second apply can never
   delete the tarball the first one is still downloading.
2. **`downloading`.** Fetch `llamaman_<ver>_linux_<arch>.tar.gz`, `checksums.txt` and
   `checksums.txt.sig` into `update/`.
3. **`verifying`.** The tarball's SHA-256 must match `checksums.txt`, and `checksums.txt` must verify
   against an **ed25519 public key compiled into the running binary** (the binary embeds a current key
   *and* a "next" key, so rotation needs no flag day). A signature failure aborts hard: this is the one
   place where a compromised download would be catastrophic. Then extract `llamaman` to
   `update/llamaman.new`, run `update/llamaman.new version` — the subcommand of §11.3, there being no
   `--version` flag anywhere — and require it to print **exactly** the requested tag. Equality rather
   than "at least" is deliberate: it catches a wrong-architecture or corrupt build before anything is
   swapped, *and* it is what lets a deliberate downgrade through (§12.4, D90). Then unlink the file.
   Nothing ever installs it: the privileged actor extracts its own copy from the tarball it re-verified
   (§12.2), so a file the service identity could rewrite after verification is never the file that
   lands on `<prefix>` (D89).
4. **`VACUUM INTO db-backups/llamaman-<current-version>-<ts>.db` (D14).** **Retention, stated once:**
   the nightly `maintenance` job keeps the newest **7** snapshots, deleting oldest first, and **never
   deletes the newest snapshot**, whatever the count is tuned to. The predicate is the newest one and
   not "the newest for the version currently installed" because of how these files are produced: a
   snapshot is written only here, immediately before an update, and is labeled with the version being
   replaced — so the newest snapshot is always the database as the version now at
   `<prefix>/llamaman.prev` left it, which is exactly the schema §12.4's downgrade procedure needs, and
   a snapshot labeled with the *installed* version either does not exist yet or carries a schema the
   running binary can already open. Rollback depth is exactly one (SPEC §5 assumption 6), so one
   protected file is the whole requirement. Migrations are forward-only, so this snapshot is the only
   thing that makes a downgrade across a schema bump possible at all; §12.4 says who restores it, when,
   and in which order, and the answer is never an actor.
5. Commit `verifying → staged`. **This commit is the cancel cut-off** (D96): up to here
   `POST /jobs/{id}/cancel` moves row and job to `canceled` in one transaction and clears `update/`
   scratch; from here on it answers `409 selfupdate_not_cancelable`, because the next step writes a
   marker to disk and the step after that hands the work to systemd, and nothing downstream reads
   `cancel_requested`.
6. **Write `update/pending`**: a temp file in `update/`, `fsync`, `rename` to `pending`, `fsync` the
   directory. The marker is therefore complete or absent on disk, never half-written.
7. Commit `staged → swapping`; then §9.4 steps 2–6 — flush the `202` to the client, drain, checkpoint
   the WAL, hand the listeners to the fd store — then `ResetFailedUnit("llamaman.service")` (D93, so
   the swap and the revert deadline that follows it begin with the full `StartLimitBurst=` budget
   rather than whatever this boot has already spent), and then
   `StartNoWait llamaman-selfupdate.service` over D-Bus from a detached goroutine. Non-blocking for the reason §5.3 gives: a `Type=oneshot` start job
   does not complete until its `ExecStart` exits, and this one's `ExecStart` ends by restarting the
   very process that would be waiting on the job. **Step 7 is also where §9.4's two callers part
   (D79): on this path the daemon does not exit.** It stops serving and waits to be SIGTERMed by the
   oneshot's own `systemctl restart`, because `Restart=always`/`RestartSec=2` would otherwise bring the
   *old* binary straight back while `selfupdate-apply` was still verifying.

   In the **D2 user-scope topology** there is no oneshot: the daemon performs §12.2's swap sequence
   itself and then exits normally, letting `Restart=always` start the new binary (§5.2a item 2).

The three commits and the one file write are ordered so that a kill at any point lands in a state
exactly one rule in §12.3 resolves; the stop-point table there walks every one of them.

**`update/pending` is written by the daemon, and that is not negotiable**: the oneshot's only trigger
is `ConditionPathExists=%S/llamaman/update/pending`, so a file the daemon did not create is a unit
that reports "condition failed" and an update that can never begin.

```jsonc
{ "format": 1, "self_update_id": "01J…", "from_version": "v1.1.0", "target_version": "v1.2.0",
  "binary_path": "/usr/local/bin/llamaman", "staged_at": 1788012345678 }
```

**One marker, and only one reader parses it.** The judge reads nothing out of `pending` — its verdict
turns on the file's *existence* and on the unit state systemd reports, so no format it does not
understand can disarm it (D13). The one parser is the confirmation gate of §12.3, and it reads the
marker in both directions across versions: a newer binary confirming an update, and — after a
downgrade — an *older* binary reading a marker a newer one wrote. So the freeze rules stay, for this
one file: **fields may be added, never removed and never retyped**; a reader ignores fields it does
not know. A `pending` that is unreadable, malformed or at an unknown `format` is **swept** exactly
like any other whose actor is gone (§12.3), rather than deferred to forever — which is safe because
the sweep's precondition is a fact about processes, not about file contents (D91). `staged_at` is
informational: it is what `GET /update/status` renders and what the journal line quotes, and **no
decision anywhere in this protocol is measured from it**. `binary_path` exists so the actor can
cross-check the daemon's view of `<prefix>` against its own `os.Executable()` and refuse on a
disagreement rather than guess.

#### 12.2 The privileged half: the swap, and the judge

**`llamaman-selfupdate.service` (root, `Type=oneshot`).**

```ini
# llamaman-units: <N>
[Unit]
Description=Llama Man self-update (privileged swap)
# One trigger, one branch. On a fresh install the file is absent, the unit reports "condition failed",
# and that is a normal non-failing outcome.
ConditionPathExists=%S/llamaman/update/pending
[Service]
Type=oneshot
# `--scope system` is not decoration: this actor must refuse to run in user scope at all, and it
# cannot learn the scope any other way (it never opens the database). install-units writes this unit
# ONLY in system scope, so the argument is a literal here rather than a substitution.
ExecStart=@PREFIX@/llamaman selfupdate-apply --scope system
# D91: this bound is what makes §12.3's "an actor is active" deferral a promise systemd enforces
# rather than one this design asserts. The work below is a hash, a copy, an untar and two renames.
TimeoutStartSec=120
# However this unit ends — cleanly, in failure, or with its main process SIGKILLed mid-swap — the host
# must not be left without a daemon. `reset-failed` first, because a unit that exhausted its start
# limit refuses a plain `start`. Neither line runs when ConditionPathExists= skipped the unit.
#
# These two lines are a BACKSTOP, not the ordinary exit, and §12.3's table names the difference: while
# the daemon is still alive — blocked in §12.1 step 7 waiting for a SIGTERM — llamaman.service is
# `active` and `start` returns EALREADY without queuing a job, so nothing here recovers anything. They
# earn their place in the two states where the daemon is gone: `failed` (the revert's own exit) and
# `inactive`.
ExecStopPost=-@SYSTEMCTL@ reset-failed llamaman.service
ExecStopPost=@SYSTEMCTL@ start --no-block llamaman.service
```

`%S/llamaman` is the state directory in both scopes (D72). `@SYSTEMCTL@` is an `install-units`
substitution beside `@PREFIX@`: an absolute `systemctl` path (systemd requires an absolute first
token), plus `--user` in the D2 topology. It is resolved by a **deterministic two-candidate probe —
`/usr/bin/systemctl`, else `/bin/systemctl`, else refuse to install** — never by a `PATH` search, so
§5.4a's drift check, which re-renders every unit and compares hashes, agrees with whatever
`install-units` wrote on any host.

**That probe is one function, `systemd.SystemctlPath()`, and it is the only producer of a `systemctl`
path anywhere in this design.** `install-units` renders its result into `@SYSTEMCTL@`; §5.4a's drift
render calls it; and the judge, whose check 2 execs `systemctl` directly, calls it too rather than
searching `PATH`. The component that has to work when everything else is broken must not depend on the
environment it inherits, and the three callers must not be able to disagree about which binary they
mean.

**The swap sequence**, run by `selfupdate-apply` in system scope and by the daemon in process in user
scope (§5.2a). Every step is either a check with no side effect or one atomic rename:

0. **Preflight — every refusal is decided here, and nothing is touched.** Parse `--scope` and refuse
   if it is missing or unparsable; read `update/pending` and refuse if it is absent, malformed or at
   an unknown `format`; refuse if `pending.binary_path` disagrees with this process's own
   `filepath.EvalSymlinks(os.Executable())`; re-verify `update/checksums.txt` against the compiled-in
   ed25519 key and the tarball's sha256 against it — **never trusting the unprivileged staging step,
   because this is a genuine privilege boundary**; and check that `<prefix>` has room for two more
   copies of the binary. Any failure writes nothing, deletes nothing, **stops nothing**, logs one
   structured line to journald and exits non-zero. Note what is *not* in this list and never was in
   this design's successor: nothing here stops `llamaman.service`, at this step or any later one. The
   swap actor never stops the daemon — the daemon has already stopped serving itself (§12.1 step 7)
   and is waiting to be restarted.
1. **Retain the current binary.** Copy `<prefix>/llamaman` to `<prefix>/llamaman.prev.tmp` with the
   same owner and mode, `fsync`, verify the copy's sha256 equals the digest of the bytes just read,
   then `rename` it over `<prefix>/llamaman.prev`. Same directory, so the rename is atomic and cannot
   return `EXDEV`; any previous retained binary is replaced in one step, which is why `llamaman.prev`
   never accumulates and never goes stale (SPEC §5 assumption 6: rollback depth is exactly one).
2. **Extract the new binary into `<prefix>`.** From the tarball step 0 re-verified, extract `llamaman`
   to `<prefix>/llamaman.new.tmp`, 0755, owned like `<prefix>/llamaman`, `fsync`. The staged
   `update/llamaman.new` is never used and no longer exists — the daemon unlinked it after its version
   probe (D89 (c)).
3. **The swap: `rename(<prefix>/llamaman.new.tmp, <prefix>/llamaman)`.** One atomic rename between two
   names in one root-owned directory. Before this instant the installed binary is wholly the old one;
   after it, wholly the new one; a power loss during it leaves one or the other and never a fragment,
   because both files were `fsync`ed first.
**A failure returned at steps 1, 2 or 3 is handled exactly like a kill at that point**: log one
structured journald line and exit non-zero. The unit's `ExecStopPost=` runs, and on this path it does
nothing useful — the daemon is still alive, blocked in §12.1 step 7, so `llamaman.service` is `active`
and `start` returns EALREADY without queuing a job. **What actually gets the host moving again is
§9.4 step 7's 120 s failsafe**, after which the daemon exits and `Restart=always` brings a binary
back; `ExecStopPost=` is the backstop for the states where there is no daemon to be already running.
The actor never attempts to undo a partial step, because there is no partial step to undo — each of
the three either completed its rename or did not, and §12.3's branch 3 closes the update out either
way (rows 6 and 7 of the stop-point table, which name the failsafe explicitly).

4. `systemctl restart --no-block llamaman.service`. That is SPEC §3.8's literal "restarts
   `llamaman.service`". Instance units are untouched and keep serving throughout — their
   `llama-server` processes `execve`'d away from the llamaman binary long ago, so replacing that file
   has no effect on them — and the public gateway ports stay open across the restart by §9.4, which is
   what makes the claim true for API clients too.

**`llamaman-update-verify.service` — the judge (`Type=oneshot`; root in system scope, the service
identity in its own manager in user scope).**

```ini
# llamaman-units: <N>
[Unit]
Description=Llama Man self-update revert
# Both conditions are plain (both must hold), and together they are the whole arming logic. On a
# fresh install, and on every host where no update is in flight, at least one is false and this unit
# is inert — which is why `OnFailure=` on llamaman.service can point at it unconditionally.
ConditionPathExists=@PREFIX@/llamaman.prev
ConditionPathExists=%S/llamaman/update/pending
# Bounded retry: a judge whose rename keeps failing (a read-only /usr, say) must not loop forever.
StartLimitIntervalSec=3600
StartLimitBurst=5
[Service]
Type=oneshot
# @SCOPE_ARG@ is `--scope system` or `--scope user`, rendered by install-units — the one component
# that knows the answer. This unit is installed in BOTH topologies, so unlike the oneshot above it is
# a substitution rather than a literal. It is how the judge knows whether `systemctl` needs `--user`.
ExecStart=@PREFIX@/llamaman.prev update-verify @SCOPE_ARG@
TimeoutStartSec=60
ExecStopPost=-@SYSTEMCTL@ reset-failed llamaman.service
ExecStopPost=@SYSTEMCTL@ start --no-block llamaman.service
```

**The judge execs the previous binary, never the installed one (D13)** — a judge that is itself the
binary under judgment cannot run when the verdict is "it does not start". Its `ExecStart` is
`<prefix>/llamaman.prev`, the file §12.2 step 1 retained, in the directory only root writes.

**Its trigger is `OnFailure=llamaman-update-verify.service` on `llamaman.service` (D88)**, and that
one line replaces the timer, the arming call, the disarming call, the polkit entry the arming call
needed and the two deadline constants the old design froze across binaries. systemd starts an
`OnFailure=` unit when the named unit enters the **`failed` state** — which, under `Restart=always`,
happens only when the restart attempts are exhausted, not on each individual failed start. So:

| `llamaman.service` | the judge |
|---|---|
| starts, sends `READY=1`, confirms the update | never started — `OnFailure=` does not fire, and the gate deleted `pending` *before* `READY=1` (§11.1 step 11), so the unit's own condition is false anyway |
| starts but is slow (a long migration, `integrity_check` on a large database) | never started — the daemon sends `EXTEND_TIMEOUT_USEC=` every 10 s while either is running, so systemd waits instead of killing it (D88). **The "healthy but merely slow daemon" case simply cannot reach the judge**, which is what lets the judge's own body be three checks and a rename |
| **will not start** — exits immediately, panics, cannot exec, or hangs past `TimeoutStartSec` without extending it | `Restart=always` retries; `StartLimitBurst=5` in `StartLimitIntervalSec=600` is exhausted after at most `5 × (45 + 2) = 235 s`; the unit enters `failed`; systemd starts the judge |
| **starts far enough to attempt a migration, then fails** — a panic after the schema moved, including one inside the gate itself, **or** a first migration that failed and moved nothing | started, and inert: §11.1 step 4 unlinked `update/pending` before the first migration ran, so `ConditionPathExists=` skips the unit (D92). Without that rule the judge would rename back a binary that can no longer open the database, and the *second* failure would find `llamaman.prev` consumed, the unit skipped and its `ExecStopPost=` not run — a host with no daemon. The exit is now a plain crash loop, §12.3's stop-point rows 11a (the schema moved: §12.4's five commands) and 11b (it did not: re-install the previous binary alone). The second is the conservative disarm costing a revert that would have been safe, and D92 states the trade |
| fails months later for an unrelated reason (a corrupt database, say) | started, and immediately inert: `update/pending` does not exist, so `ConditionPathExists=` skips the unit |
| is restarted three or four times in ten minutes by a human doing configuration work | never started — a start that stayed healthy for 60 s clears the unit's start-limit counter (D93, §11.1 step 12), so ordinary restarts do not accumulate toward `failed` at all. That limit counts *consecutive starts that never became healthy*, which is the only thing the revert is about |

**The deadline is therefore a property of the rendered unit, not a constant anyone has to agree
about.** §5.4 pins `TimeoutStartSec=45`, `RestartSec=2`, `StartLimitIntervalSec=600` and
`StartLimitBurst=5`, and a CI test asserts both the values and the arithmetic
`StartLimitBurst × (TimeoutStartSec + RestartSec) < StartLimitIntervalSec` — the second half being
what stops the burst counter from resetting between attempts, which would leave a hanging daemon
looping forever in `activating` and never reaching `failed`. The values are only half the story,
because systemd's start rate limit counts **every** start attempt and not only the failed ones; D93's
`ResetFailedUnit` after a healthy boot is what makes the arithmetic a statement about a *bad binary*
rather than a budget the restart button spends.

**The judge's whole body**, in order, with nothing between the last check and the rename:

1. **`fstat` its own image** (`/proc/self/exe`) and `<prefix>/llamaman`: refuse unless both are owned
   by the same uid and its own image is not group- or world-writable. Defense in depth — `<prefix>` is
   root-only in system scope, so a planted `llamaman.prev` is already impossible, and in user scope
   every file involved is one uid — but it is two `fstat`s, and it is the check that would have
   mattered when this file lived in the service-identity-owned `update/` directory (D89).
2. **Ask systemd for the unit state**, by **exec**, not over D-Bus: run
   `<systemd.SystemctlPath()> [--user] is-active llamaman.service` — the same path `@SYSTEMCTL@` was
   rendered from, with `--user` exactly when `@SCOPE_ARG@` is `--scope user`. Exec rather than the bus
   because this component runs when the daemon is gone and must carry no client library, no
   authorization step and no assumption about which bus exists; and never a `PATH` search, for the
   reason stated above. **Read the verdict from stdout, never from the exit status**: `is-active`
   exits **3** for any unit that is not active while printing the state on stdout, so treating a
   non-zero exit as an error inverts this check on the one input that matters. Only the trimmed
   literal `failed` authorizes step 3. Anything else — `active`, `activating`, `inactive`,
   `deactivating`, an unrecognized word, or an exec that failed outright — and the judge **does
   nothing**, logs the state it observed (or the exec error) and exits 0. `active` means a daemon is
   running and this is not the judge's business; `inactive` means a human or a shutdown stopped the
   service deliberately; `activating` means a start is still in progress. The check is a re-assert of
   the condition `OnFailure=` fired on, and it is what keeps a hand-run `systemctl start
   llamaman-update-verify.service` from reverting a healthy host.
3. **`rename(<prefix>/llamaman.prev, <prefix>/llamaman)`** — one atomic rename, same directory,
   consuming the retained binary. Log the revert at `warning` with both version strings, the reason,
   and the fact that `<prefix>/llamaman.prev` is now gone.

Then `ExecStopPost=` runs `reset-failed` and `start --no-block` on `llamaman.service` — however the
judge ended, including SIGKILL, and including the case where it decided to do nothing. That is
deliberate and it is safe for one reason: the unit's two `ConditionPathExists=` lines mean these
commands run **only** while an update is genuinely unconfirmed, so "start the daemon back up" is never
issued against a host whose service a human deliberately stopped in the ordinary course of things. `reset-failed` is not optional: the unit is in `failed` state with its
start limit exhausted, and a plain `start` on such a unit is refused with "start request repeated too
quickly". The revert therefore ends with a running daemon in every case where the old binary works,
and where it does not, the unit fails again, `OnFailure=` fires again, and the judge's own
`ConditionPathExists=@PREFIX@/llamaman.prev` is now false — so it is inert and there is no loop.

**What the judge deliberately does not do**, each because the simpler protocol does not need it:

- **It never stops anything.** Its trigger is the unit's `failed` state, and a unit in that state has
  no process left under it, so there is nothing to stop. That single fact removes the entire
  verify-before-stop ordering problem, the stop-then-refuse hole, and the "a refusing actor leaves the
  host dark" class with it. (The stronger claim — that its trigger means "no daemon could start" — is
  *made true* by D92 rather than assumed: `OnFailure=` also fires for a daemon that started, migrated
  and then failed, and it is §11.1 step 4's unlink that keeps the judge from being armed in that case.)
- **It never touches the database — not even to read it.** No `chown`, no snapshot restore, no WAL
  sidecar surgery, no schema comparison, no reasoning about whether a live process holds the file. Not
  even a read-only open: a root process that opens `llamaman.db` creates a root-owned `-wal`/`-shm`
  beside it, which is a database the service identity can never write again (§11.3). This is why the
  judge cannot be given a "refuse when the schema is ahead of `llamaman.prev`" check, and why that
  question is answered by the daemon instead, at the one moment it has the answer (D92). Restoring a
  database is §12.4's procedure and a human's decision (D90).
- **It never writes a marker.** The old design needed one so the restored daemon could tell "I was
  reverted" from "the swap never happened" — but both mean the same thing to the daemon and produce
  the same action (§12.3 branch 3), and the journal tail the F24 card carries says which. Deleting the
  marker deleted a file whose contents could not reconstruct the row it was supposed to write.
- **It never notifies.** It cannot: §11.3 forbids a root process from opening `llamaman.db`, because a
  root-created `-wal`/`-shm` beside it is a database the service identity can no longer write. The
  actors state facts in the journal; the daemon owns the `notifications` row.

**Neither privileged actor ever opens the database**, and the scope comes from the `--scope` argument
`install-units` renders into both units — the same SPEC §3.9 argument as `serve --scope`: a flag our
installer writes into our own unit, not a config file and not an environment variable. They need it
before they could open anything, `runtime_info.systemd_scope` is unreachable to them, §11.1's bus
probe would interrogate the unit they are about to restart, and §5.4 rejects the euid as a signal.
Two consequences follow:

- **The judge's argument crosses versions**, because units are written once at install time and are
  *not* rewritten by a self-update, and because the judge's `ExecStart` is the *previous* binary. Like
  the `format` field it ships from the v1.0.0 floor and may never be removed or retyped; a CI test
  renders the units from each supported release and runs every combination. The oneshot's is a literal
  in a unit installed only in system scope, so it crosses nothing.
- **A missing or unparsable `--scope` is a refusal, not a guess.** The actor leaves everything in
  place, logs one structured journald line and exits non-zero; the repair line
  `sudo llamaman install-units --identity <user> [--user-units]` reaches the human through the F24
  card the daemon raises, which carries that unit's journal tail (D77).

#### 12.3 The confirmation gate, and every way this stops

One routine, **`ResolveUpdateMarkers`**, with three callers: the boot gate (§11.1 step 11), a **30 s
ticker** that runs only while `update/pending` exists and is stopped by the §9.4 shutdown along with
the other background workers, and `POST /update/apply` immediately before its guard. `llamaman doctor`
reports the same facts read-only and writes nothing. The routine is idempotent and does all of its
writing in one transaction, so two callers racing produce one resolution and one notification.

Boot alone was never enough — after a refusal, §9.4 step 7's 120 s failsafe returns *this* daemon to
service and the next boot may be weeks away — and the endpoint caller is what lets the next **Update**
click start from a clean directory instead of being refused by debris.

**There is one marker and one question about it.** `update/pending` is read — or, on the first call of
a boot that applied migrations, taken from the in-memory copy §11.1 step 4 made before it unlinked the
file (D92); the two are the same input and the branches below do not distinguish them. If there is no
marker at all the routine goes straight to the closing pass. Otherwise, in this order:

1. **`pending.target_version` equals this binary's version → the update took.** In one transaction
   mark the `self_updates` row `succeeded` and its job `succeeded` and emit the `events` row; then
   unlink `update/pending`; then remove any `update/` scratch. Commit first and unlink second, so a
   kill between them leaves a terminal row beside a marker the next call resolves as a no-op — the
   branch is idempotent for a row already `succeeded`. `<prefix>/llamaman.prev` is deliberately kept:
   it is the emergency manual restore the F24 card names, it costs one binary's disk, and it is
   replaced wholesale by the next update's step 1, so it has a writer and can never go stale.
2. **`pending.target_version` is not this binary's version, and `llamaman-selfupdate.service` reports
   `activating` or `active` → an actor is working. Do nothing.** (`deactivating` is deliberately *not*
   in that set: it means the `ExecStart` process is already gone and only `ExecStopPost=` is left, so
   the swap is decided one way or the other and branch 3 is the right answer.) Leave the marker, leave the `self_updates` row in
   `staged`/`swapping` and its job `interrupted`, and log at `info` that a swap to `<target_version>`
   is in flight. This is the only deferral in the protocol, it exists for the intermediate boot §9.4
   step 7's failsafe can produce, and it is **bounded by systemd**: that unit carries
   `TimeoutStartSec=120`, after which systemd kills it, `ExecStopPost=` runs and the unit is no longer
   active — so the next caller takes branch 3 (D91). In user scope this branch is unreachable: the
   swap is performed in process by the daemon, so a daemon that is running is not simultaneously
   mid-swap.
3. **`pending.target_version` is not this binary's version and no actor is active → the update did not
   take.** Either the swap never happened, or it happened and the judge reverted it; both mean the
   same thing, and the journal tail says which. In one transaction close the `self_updates` row
   `failed` with an `error_message` naming both versions, and its job `failed` with
   `error_code='update_not_applied'` (§2.3a: the job carries the code, the domain row the message);
   then unlink `update/pending`; then remove `update/` scratch; then raise **F24** carrying
   the journal tail of `llamaman-selfupdate.service` and `llamaman-update-verify.service` (or the F23
   hint when `journal_read` is denied) and stating which version is actually installed. Deleting the
   marker here also disarms the judge — its `ConditionPathExists=` no longer holds — which is correct
   in both readings: if the swap never happened there is nothing to revert, and if the judge already
   reverted, `<prefix>/llamaman.prev` is gone anyway.

   An `update/pending` that is unreadable, malformed or at an unknown `format` takes this branch too,
   naming the file rather than a version. It is safe to sweep a file this binary cannot read precisely
   because the precondition is about processes, not contents: no actor is running, so no actor is
   waiting for it (D91).

**A marker whose `self_update_id` names no `self_updates` row is a no-op in both writing branches.**
The branch performs no domain write, still unlinks the marker, still clears `update/` scratch, and —
in branch 3 — still raises F24, taking both version strings from the marker rather than from a row.
That state is reachable on the ordinary path and must not abort a boot: §11.1 step 3's
`PRAGMA integrity_check` fails, F12 takes its "else start a fresh DB" arm (a truncated WAL after a
power loss during the swap is the obvious producer), and step 11 then reads a marker whose id matches
nothing; a `llamaman restore-db` to a snapshot older than the update produces the same shape. The
closing pass carefully handles the inverse — a restored database resurrecting a row no marker names —
and this is its mirror, stated because it is the branch that runs immediately after the most
disruptive recovery in the design.

**Then, in the same transaction, whichever branch matched — the closing pass.** Every non-terminal
`self_updates` row whose paired `self_update` job is **`interrupted`** and which a surviving
`update/pending` does not name is closed `failed` / `error_code='daemon_restarted'`, row and job
together. Two things produce such an orphan and neither leaves a marker: a plain daemon restart during
`downloading` or `verifying`, and a database restore — F12's boot recovery from `db-backups/`, or
`llamaman restore-db` — that resurrects a `self_updates` row from a snapshot taken mid-update.
`interrupted` means the lease belongs to a boot that is gone (§2.3), which is what makes the pass safe
in all three callers rather than at boot alone: it can never close work the calling process is itself
performing, including a forward update that is `downloading` on this boot's own lease. It cannot touch
branch 2's deferral, because that marker names that row. And it cannot touch the row a matched branch
just resolved, because that row is now terminal. Without the pass, such a row's live job would refuse
every future update at `409 job_in_flight` with no marker for any caller to find.

**Every way this protocol stops, and what gets out of it.** Each row was walked as a kill, a power
loss or an error return between the two steps named; every one ends in a state that the next daemon
boot, the judge, or a documented command exits from:

| # | stops where | what is on disk | what gets out of it |
|---|---|---|---|
| 1 | daemon dies anywhere in §12.1 steps 1–4 (before `staged` commits) — including `ENOSPC` on the tarball or the D14 snapshot | row `planned`/`downloading`/`verifying`, job `interrupted`, `update/` scratch, **no marker** | the closing pass closes row and job `failed`/`daemon_restarted`; the next `POST /update/apply` empties `update/` before it stages, so the scratch has a deleter |
| 2 | between §12.1 steps 5 and 6 — row committed `staged`, marker not yet written | row `staged`, job `interrupted`, no marker | the same closing pass |
| 3 | between §12.1 steps 6 and 7 — marker written, `swapping` not committed | row `staged`, job `interrupted`, marker present, no actor ever started | branch 3: row and job `failed`/`update_not_applied`, marker deleted, **F24** |
| 4 | after §12.1 step 7's commit, before or while the oneshot starts | row `swapping`, job `interrupted`, marker present | branch 2 while `llamaman-selfupdate.service` is active — bounded by its `TimeoutStartSec=120` — then branch 3 |
| 5 | actor refuses in §12.2 step 0 (bad `--scope`, unreadable marker, `binary_path` disagrees, signature fails, `<prefix>` too full) | nothing written; marker intact; **`llamaman.service` was never touched by this actor** | the daemon is at this moment blocked in §9.4 step 7 waiting for a SIGTERM that will now never come, so `ExecStopPost=`'s `start` is a no-op; the exit is the failsafe, which fires 120 s later, exits, and lets `Restart=always` bring the same binary back for an ordinary boot whose gate takes branch 3 → **F24** carrying the actor's own journal line. The visible cost of a refusal is therefore up to two minutes of no management UI, with the gateway sockets held by the fd store and `llama-server` unaffected throughout |
| 6 | actor killed between §12.2 steps 1 and 2 | `<prefix>/llamaman.prev` is a fresh copy of the binary that is still installed; nothing swapped | the same exit as row 5, and for the same reason: the daemon is alive and blocked in §12.1 step 7, so `llamaman.service` is `active` and `ExecStopPost=`'s `start` returns EALREADY without queuing a job. The 120 s failsafe fires, the daemon exits, `Restart=always` brings the **old** binary back, and its gate takes branch 3 → **F24**. Same visible cost as row 5: up to two minutes of no management UI, gateway sockets held by the fd store throughout. The retained copy is harmless — byte-identical to `<prefix>/llamaman` — and the next update's step 1 replaces it |
| 7 | actor killed between §12.2 steps 2 and 3 | `<prefix>/llamaman` unchanged; `<prefix>/llamaman.new.tmp` left behind | as row 6: the failsafe, then the old binary, then branch 3 → **F24**. Branch 3 deletes the marker, so the judge is inert. `llamaman.new.tmp` is opened `O_TRUNC` by the next actor run, so it has a writer that reclaims it; nothing ever executes it, and `doctor` reports it |
| 8 | power loss **exactly at** §12.2 step 3's `rename` | atomic: `<prefix>/llamaman` is wholly the old binary or wholly the new one, both `fsync`ed beforehand | the boot starts whichever it is — new → branch 1 confirms; old → branch 3 |
| 9 | actor killed between §12.2 steps 3 and 4 — swapped, no restart issued | new binary installed, marker present | again the failsafe, not `ExecStopPost=`: the daemon is still alive and waiting, so `start` is a no-op. After 120 s it exits and `Restart=always` starts `<prefix>/llamaman`, which is now the **new** binary; if it works, branch 1 confirms; if it does not, the unit reaches `failed` and `OnFailure=` starts the judge. Cost: the same up-to-two-minutes of no management UI |
| 10 | **the new binary will not start at all** (the case F20 is for, including a downgrade whose schema gate refuses — §12.4) | unit `failed`, marker present, `<prefix>/llamaman.prev` present | the judge: `is-active` reads `failed` **on stdout**, one rename restores the previous binary, `ExecStopPost=` `reset-failed` + `start`; the old daemon's gate takes branch 3 → **F20**/F24 |
| 11a | **the new binary starts, commits at least one migration, then fails** — a panic between the first migration's commit and step 11's commit, including one inside `ResolveUpdateMarkers` itself, reproducing on every attempt | unit `failed`, **no marker** (step 4 unlinked it before migrating, D92), `<prefix>/llamaman.prev` present, database **migrated past it** | the judge is **skipped** — `ConditionPathExists=%S/llamaman/update/pending` is false — so the previous binary is *not* renamed back over a database it could not open, which is the whole point of D92. What is left is an ordinary crash loop on the new binary, and the exit is manual and named: `doctor` reports the installed version, the schema, the newest usable snapshot and the journal tail, and the operator runs §12.4's five commands — the schema has moved, so re-installing the previous binary alone would only trade this crash loop for a `schema_ahead` one, and the snapshot that procedure restores is the one §12.1 step 4 took just before this very update and D14 protects as the newest. Note that step 4 of that procedure, `reset-failed`, is not optional here: reaching this row *is* exhausting the start limit. The `self_updates` row is closed `failed`/`daemon_restarted` by the next boot's closing pass even though the swap itself succeeded — the one mislabeled history row D92 buys the host with |
| 11b | **the new binary starts and its FIRST migration fails before committing** — a `UNIQUE` violation or a `NOT NULL` backfill that this host's real data refuses, reproducing on every attempt. The likeliest reason a newly installed binary will not boot | unit `failed`, **no marker** (step 4 unlinked it before *attempting* the migration, D92), `<prefix>/llamaman.prev` present, database schema **unmoved** — each migration is one transaction, so a failed one commits nothing | the judge is skipped for the same reason, and here that is the conservative disarm costing the host a revert that would have been correct: the previous binary can still open this database. The exit is therefore shorter than row 11a's and **restores nothing**: `sudo systemctl stop llamaman.service`, `install.sh --version <previous> --no-start`, `sudo systemctl reset-failed llamaman.service`, `sudo systemctl start llamaman.service`. **No `restore-db`, no snapshot, no data discard** — nothing created since the snapshot is lost, because the database never moved. `doctor` decides between this row and 11a from `MAX(schema_migrations.version)`, which it already reads: **equal** to the same value in the newest `db-backups/` snapshot — the one §12.1 step 4 took immediately before this update, which D14 protects — means no migration of this release committed and this row applies; **greater** means row 11a and the full five commands. The `self_updates` row closes exactly as in row 11a |
| 12 | judge refuses at its check 1 or 2, or its `rename` fails (a read-only `<prefix>`, `EACCES`) | **nothing changed** — the host is exactly as the judge found it | one structured journald line; the unit is `failed`, so here `ExecStopPost=` genuinely does start the service, which fails again, so `OnFailure=` retries the judge, bounded by its own `StartLimitBurst=5` per hour; a human reading `journalctl -u llamaman-update-verify.service` gets the exact manual line, which is the same one rename: `sudo mv <prefix>/llamaman.prev <prefix>/llamaman && sudo systemctl reset-failed llamaman.service && sudo systemctl start llamaman.service` |
| 13 | judge killed before its rename | nothing changed | as row 12 |
| 14 | power loss **exactly at** the judge's `rename` | atomic: `<prefix>/llamaman` is wholly the new binary (and `llamaman.prev` survives) or wholly the old one (and `llamaman.prev` is gone) | the boot starts `llamaman.service`: old → branch 3; new → it fails again, `OnFailure=` starts the judge again, and `llamaman.prev` is still there to install. **This is the row a `+180 s` timer could not cover**, because nothing re-arms a timer across a reboot (D88) |
| 15 | the daemon is killed after the judge's revert, before its gate runs | old binary installed, marker present, `llamaman.prev` consumed | `Restart=always` or the next boot; branch 3, which is idempotent |

Two properties make the table exhaustive rather than merely long, and both are structural: **no step
in this protocol stops a unit** — the daemon stops serving itself at §12.1 step 7 and the actors only
ever start things — and **every step that changes the installed binary is a single `rename()` between
two names in one directory**, so there is no intermediate on-disk state for a crash to land in.

**One consequence of the first property is worth reading off the table**, because three rows used to
name the wrong mechanism: while the daemon is alive and blocked in §12.1 step 7, `llamaman.service` is
`active`, so an actor's `ExecStopPost=` `start` is an EALREADY no-op and recovers nothing. In rows 5,
6, 7 and 9 the thing that actually gets the host moving again is §9.4 step 7's **120 s failsafe** — the
daemon gives up waiting, exits, and `Restart=always` brings a binary back. `ExecStopPost=` earns its
place in rows 12–14, where the unit is `failed` and there is no daemon to be already running. The cost
in all four failsafe rows is identical and is stated in each: up to two minutes with no management UI,
with the gateway sockets held by the fd store and every `llama-server` unaffected throughout.

#### 12.4 Downgrades, the schema consequence, and `llamaman restore-db`

**Installing an older release is the ordinary update flow (D90).** `GET /api/v1/update/releases`
already lists every release, `POST /update/apply {"tag":"v1.1.0"}` accepts any of them, and §12.1
step 3's probe requires the extracted binary to print *exactly* that tag. Nothing in §12.1–§12.3
distinguishes a downgrade from an upgrade, and neither does the judge.

**The honest consequence is the database.** Migrations are forward-only, so after a downgrade the
schema may be *ahead* of the binary that is now installed. Two gates refuse rather than guess:

- The **daemon** (§11.1 step 4): if `MAX(schema_migrations.version)` exceeds the highest migration
  embedded in this binary, it does not open the database for writing. It logs one journald line naming
  both versions and the newest matching `db-backups/` snapshot, and exits non-zero.
- **`instance-exec`** (§5.6a): the same comparison, exit **75** with `error_code='schema_ahead'`.

What happens next is worth stating plainly, because it is the whole reason a separate rollback
mechanism is not needed: the daemon's refusal is a start failure, so `llamaman.service` reaches
`failed` and **the judge automatically puts the newer binary back** (§12.2, row 10 of the stop-point
table). A downgrade across a schema bump therefore self-corrects within ~235 s, the host is left
running the version it was running before, and the F24 card explains why — the journal tail it carries
is the older binary's own `schema_ahead` line.

**And the judge's rename consumes `<prefix>/llamaman.prev`.** That is the fact the rest of this
section turns on: at the moment the F24 card appears, the older binary's bytes exist nowhere on the
host — `update/llamaman.new` was unlinked after the version probe (§12.1 step 3) and
`llamaman.new.tmp` was renamed onto `<prefix>/llamaman` and then overwritten by the revert. So:

> **`llamaman restore-db` on its own does not complete a downgrade — it is a destructive no-op (D94).**
> Run at that moment it passes its own precondition trivially (the snapshot's schema is not newer than
> the *newer* binary now running), restores the old database, and is immediately undone: the newer
> binary starts, §11.1 step 4 migrates the restored database forward, and the only lasting effect is
> that every instance, token, benchmark run, download, event and notification created since the
> snapshot is gone. The operator typed the file name to confirm and got nothing they asked for.

**Making the downgrade stick is an explicit, offline, confirmed, five-command act**, and it is the one
place in this protocol where a human is required. The order is not interchangeable:

```
# 1. Stop the daemon. A deliberate stop is not a failure, so `OnFailure=` does not fire and the
#    judge stays unarmed for everything that follows.
$ sudo systemctl stop llamaman.service

# 2. Put the older binary back on disk, with the service still down. `--no-start` is what keeps
#    step 11 of install.sh from restarting into a database this binary cannot open yet.
$ curl -fsSL https://raw.githubusercontent.com/jlbyh2o/llamaman/main/install.sh \
    | sudo sh -s -- --version v1.1.0 --no-start

# 3. Restore the database the older schema belongs to. Now the precondition means something: the
#    binary running this command IS the older one.
$ sudo llamaman restore-db /var/lib/llamaman/db-backups/llamaman-v1.1.0-1788012345.db

# 4. Clear the unit's start-limit counter. Every state this procedure is printed for was reached by
#    a unit that reached `failed` — which under Restart=always means it exhausted
#    StartLimitBurst=5 starts in StartLimitIntervalSec=600 — and `systemctl stop` clears the failed
#    state but NOT the rate limit. Without this line step 5 is refused with "start request repeated
#    too quickly" for the remainder of the 600 s window, which a binary that panics in seconds
#    leaves nearly whole and which is longer than steps 1-3 take. This is the same
#    `reset-failed`-before-`start` pairing both actors' ExecStopPost= carry (§12.2) and the same one
#    §12.3 rows 12-14 and F20 print for the manual rename. It is harmless when the unit is not
#    failed and the counter is clear, which is why it is unconditional rather than a branch.
$ sudo systemctl reset-failed llamaman.service

# 5. Start it.
$ sudo systemctl start llamaman.service
```

Steps 1 and 2 are what the earlier reading of this section left out, and without them step 3 is the
destructive no-op above; step 4 is what the earlier reading left out at the other end, and without it
step 5 fails with an opaque systemd message during an outage. There is no in-window shortcut worth
documenting: racing the ~235 s crash-loop before the judge fires is not a procedure, and the five
commands above work from any state — the self-corrected one, and the crash-loop state of §12.3 row
11a with the start limit fully spent.

`GET /api/v1/update/status` and the update dialog print these five lines verbatim for a target older
than the running version whose release notes flag a schema change, and so do the F24 card and
`llamaman doctor` when they find a database ahead of the installed binary. **Nothing prints the
`restore-db` line alone, and nothing prints the procedure without step 4.**

| `restore-db` | contract |
|---|---|
| **Privilege** | root or the database's owner; the euid is checked against `stat`, exactly like `reset-password` (§11.3) |
| **Preconditions**, all refusals, none of them overridable by a flag | `llamaman.lock` is free — else it refuses naming the holding PID and printing `sudo systemctl stop llamaman.service`, because the daemon must be down; the snapshot exists under `db-backups/` and passes `PRAGMA integrity_check`; and the snapshot's `MAX(schema_migrations.version)` is **less than or equal to** the highest migration embedded in the binary running this command — else it refuses naming both numbers, since restoring a schema this binary still cannot open would only move the failure |
| **What it prints, before doing anything** | the snapshot's path, size, mtime, schema version and the `runtime_info.daemon_version` recorded in it; the same five facts for the current `llamaman.db`; and the **loss**, counted by opening both read-only: instances, tokens, benchmark runs, downloads, events and notifications present in the current database and absent from the snapshot, one line each. **Plus one warning line whenever the binary that will start next would migrate the restore straight back forward** — that is, when this binary's highest embedded migration exceeds the snapshot's `MAX(schema_migrations.version)`: *"`<prefix>/llamaman` is `<version>`; it will migrate this database forward on its next start. If you meant to downgrade, install the older release first — see §12.4."* That is the exact shape of D94's destructive no-op, and printing it is what turns it into an informed choice rather than a silent one. It is a warning, not a refusal: restoring an older snapshot on the same version is a legitimate data-recovery operation and stays available. Then it requires the operator to type the snapshot's file name to proceed (`--yes` supplies it non-interactively, for the one scripted case the F24 card documents) |
| **What it does** | (1) opens the current database read-write and runs `PRAGMA wal_checkpoint(TRUNCATE)`, so the main file is complete and its sidecars are redundant; (2) `VACUUM INTO llamaman.db.superseded-<ts>` beside it, so the discarded database is recoverable by re-running this command against *it*; (3) unlinks `llamaman.db-wal` and `llamaman.db-shm`; (4) copies the snapshot to `llamaman.db.restore` **in the same directory**, `chown`s it to the current database's uid/gid, `chmod 0600`, `fsync`s it and the directory; (5) `rename`s it over `llamaman.db`. Steps 4 and 5 are the D14 ownership rule: a root-created 0600 file the service identity cannot write is a database that never opens again |
| **Crash safety** | walked step by step: after (1) the live database is complete and its sidecars are redundant; a kill during (2) leaves a partial `llamaman.db.superseded-<ts>`, which is debris the 30-day prune removes and nothing ever reads; after (3) and before (5) the live file is still the complete old database with no sidecars, so a daemon started at that instant works and re-running the command is idempotent; a partial `llamaman.db.restore` is overwritten by the next run and is never the target of the rename; after (5) the restore is done. Every window lands on a database that opens |
| **It never runs automatically** | not from the judge, not from the confirmation gate, not from `POST /update/apply`, not from F12's boot recovery — which is a different path, for a *corrupt* database, and restores the newest snapshot rather than one the operator chose. Nothing in §12 restores a database |

`llamaman.db.superseded-<ts>` is pruned by the nightly `maintenance` job after 30 days, and
`restore-db` prints its path so the operator can keep it.

After step 5 the older binary starts, its schema gate is satisfied, and the confirmation gate's closing
pass closes whatever `self_updates` row the snapshot resurrected (§12.3). Release notes flag
schema-changing releases so the UI can warn *before* one is applied in either direction, and the update
dialog says, for a target older than the running version, exactly what the paragraphs above say: the
one-click downgrade will self-correct and consume the retained binary, completing it takes the five
commands, and `restore-db` alone is not one of them.

---

## 13. `install.sh` design

One file, POSIX `sh` (`#!/bin/sh`, `set -eu`), no bashisms, shellcheck-clean, served from
`raw.githubusercontent.com/jlbyh2o/llamaman/main/install.sh`.

**The entire script is wrapped in a `main "$@"` function invoked on the last line (D48)**, so a
truncated `curl … | sh` download cannot execute half a script — the classic curl-to-shell hazard.

**Flags**: `--version <tag>` (default: latest), `--user <name>` (default `SUDO_USER`),
`--dedicated-user`, `--user-units` (D2), `--port <n>`, `--prefix <dir>` (default `/usr/local/bin`;
default `~<user>/.local/bin` under `--user-units`), `--no-autostart-grant` (§5.2), `--no-start`,
`--repair-polkit`, `--uninstall`, `--purge`, `--dry-run`.

**`--prefix` is threaded, not decorative.** It is passed to `llamaman install-units --prefix`, which
renders `ExecStart=<prefix>/llamaman` into **both** the daemon unit and the instance template; the
self-update actors resolve the same path from `os.Executable()` rather than a constant (§12, D15).
There is no place in the design where `/usr/local/bin` appears as a literal that `--prefix` cannot
move.

**Sequence**

1. **Preconditions** — `id -u` = 0, else print the exact `sudo sh -c "curl … | sh"` line and exit
   (the script never re-execs itself under sudo: under `curl | sh` there is no file to re-exec).
   `[ -d /run/systemd/system ]` else "requires systemd". `uname -s` = Linux; `uname -m` ∈
   {x86_64→amd64, aarch64→arm64}; `curl` or `wget` present; ≥ 500 MB free in `/var/lib`.
2. **Resolve artifacts through the `releases/latest/download/` redirect**, not the GitHub API
   (D48) — the 60/hour anonymous API limit makes the installer fail intermittently on shared or
   NAT'd networks. `--version` uses `releases/download/<tag>/`.
3. **Download and verify** into a `mktemp -d` cleaned by a trap: `sha256sum -c` (or `shasum -a 256`)
   against `checksums.txt`; the ed25519 signature is then verified by the extracted binary itself
   (`llamaman verify-release`), so no external signing tool is required and there is never a silent
   skip.
4. **Identity** — default the invoking user, whose home must be readable; `--dedicated-user` runs
   `useradd --system --home-dir /var/lib/llamaman --shell /usr/sbin/nologin llamaman` idempotently
   and additionally pre-creates `/var/lib/llamaman/hf-cache/hub` owned by that account, mode 0750, so
   the daemon's §7.2 rule-4 detection finds it already present on first boot. No environment variable
   is set for it — SPEC §3.9 forbids requiring one, and rule 4 infers the topology from
   `$HOME == state_dir` instead. Group membership for the journal is **not** arranged here: it is
   `install-units`' job in step 7 (D77), so that the F16 repair path re-applies it too.
5. **Install the binary** — create `<prefix>` first (`install -d -m 0755`, owned by `<user>` under
   `--user-units`, where `~<user>/.local/bin` routinely does not exist on a fresh account and the
   `install` in the next breath would otherwise fail before step 6 ever ran), then `install -m 0755`
   to a temp file in `<prefix>` and `mv` over `<prefix>/llamaman` (atomic, and safe while the old one
   is running). **Ownership is topology-dependent**: root-owned `root:root` in system scope (D15 —
   a service-user-writable file on root's `PATH` is an escalation trap), and `chown <user>:<group>`
   under `--user-units`, because there the unprivileged daemon performs its own self-update by
   `rename`ing over this very path (§5.2a item 2) and D15's rationale does not apply — the binary is
   not on root's `PATH`. The script is running as root in both cases, so a missing `chown` here left
   a root-owned binary the D2 daemon could never replace. `<prefix>` is also where the retained
   previous binary and the swap actor's `llamaman.new.tmp` live (D89), so the script checks that the
   directory has room for three copies and **warns, without refusing**, when `<prefix>` is
   group-writable — a `root:staff 2775 /usr/local/bin` is the common case, the hazard predates Llama
   Man (anyone who can write `llamaman.prev` there can already write `llamaman`), and `doctor`
   reports the same fact.
6. **Directories** — `install -d -o <user> -g <group> -m 0750 <state_dir>` and its
   `{versions,src,build,logs,db-backups,update,tmp}` children, where `<state_dir>` is
   `/var/lib/llamaman` by default and `~<user>/.local/state/llamaman` under `--user-units`, matching
   what `StateDirectory=llamaman` will resolve to in each scope (D72, §11.1 step 1). Under
   `--user-units` the parent chain (`~<user>/.local`, `~<user>/.local/state`) is created the same way
   and owned by `<user>`. The resolved path is printed, and `--uninstall` prints the same one.
7. **Units and polkit** — `llamaman install-units --identity <user> --prefix <prefix> [--port N]
   [--user-units] [--no-autostart-grant]`. **The binary writes the unit and polkit content, not the
   shell script**: one source of truth, testable in Go, and the same command is the F16 repair path.
   Every unit it writes carries a `# llamaman-units: <N>` stamp naming the template version it was
   rendered from, which is what lets §5.4a tell a hand-edit from a host whose units simply predate the
   binary now running (D95).
   `--user-units` also makes it render `serve --scope user` into `ExecStart=` (§5.4, §11.1 step 1), so
   the daemon is told its topology rather than inferring it. It additionally adds `<user>` to the
   `systemd-journal` group (D77) — the one place that grant belongs, since it is a root-only,
   idempotent change that the F16 repair must re-apply.

   Then reload the right manager: `systemctl daemon-reload` in system scope, and under
   `--user-units` the `loginctl enable-linger` + `runuser … systemctl --user daemon-reload` half of
   the §5.2a item (3) sequence — root's own `systemctl --user` would talk to *root's* manager and
   silently do nothing useful.

   `install-units` runs as **root** and creates nothing under the state directory — see the blanket
   rule in §11.3, which covers the database, its WAL/SHM sidecars and `secret.key` alike, and which
   applies to every root-invoked subcommand in this script rather than to this one alone. The port
   preference reaches the DB later, from the unprivileged daemon, as the one-time seed of §11.1
   step 6b.
8. **Toolchain report** — `llamaman doctor --format=text`, so detection logic is never reimplemented
   in shell. At this point the database does **not** exist, and that is a normal outcome, not an
   error: `doctor` reports its DB-dependent checks as `skipped (database not yet created)` and runs
   the rest. It opens nothing, so it cannot leave a root-owned `llamaman.db`, `-wal` or `-shm`
   behind. Missing tools are reported with per-distro package names (read from `/etc/os-release`
   purely to print the right names). **No package manager is ever invoked.**
9. **Start** — **in the scope this install actually used**, because the two managers are different
   programs and an unconditional system-scope command would enable nothing on a `--user-units` host:
   - system scope: `systemctl enable --now llamaman-instances.target llamaman.service`;
   - `--user-units`: the second half of the §5.2a item (3) sequence, whose canonical form lives there
     and is not restated here —
     `runuser -u "$LM_USER" -- env XDG_RUNTIME_DIR=… DBUS_SESSION_BUS_ADDRESS=… systemctl --user
     enable --now llamaman-instances.target llamaman.service`, after the script has polled up to 10 s
     for `/run/user/<uid>/bus` to appear.

   Then poll
   `http://127.0.0.1:<p>/api/v1/meta` for up to 30 s over `p ∈ [base, base+20]`, where
   `base = --port` when given and 5526 otherwise. The range **follows the requested port** rather
   than being hard-coded, and it is the same `+20` walk the daemon performs in §11.1 step 7, so
   `--port 9000` polls 9000–9020 and prints a correct URL. The first port answering `/api/v1/meta`
   wins; the poll also cross-checks the returned `ui_port` field. If none answers, the installer
   prints `journalctl -u llamaman -n 50` rather than a URL it cannot verify.
10. **Print** the resolved URL, the one-time setup token and the next step — by running plain
    `llamaman status` and echoing its `Setup` block verbatim. **No `--json`, and therefore no
    `jq`**: step 1's preconditions require only `curl` or `wget`, and hand-parsing JSON in POSIX
    `sh` is exactly the fragility this script avoids elsewhere. The plain-text block already
    carries both the URL and the token (§11.3), which by then is a plain file read of
    `<state_dir>/setup-token` (§2.2a — the resolved directory of D72, which under `--user-units` is
    not `/var/lib/llamaman` at all) — the daemon minted it during *its* boot at step 8 of
    §11.1, wrote it to disk, and logged it to journald. The installer therefore neither scrapes
    journald nor asks the database for something the database does not hold. This step runs **after**
    the daemon started, so the DB and its WAL sidecars already exist owned by the service identity
    and root's read-only open creates nothing (§11.3). If the block reads `Setup complete`
    (already claimed) or carries the `run as root or <identity>` hint instead of a token, the
    installer prints the URL alone and says the token was already used.
11. **Idempotent upgrade** — re-running installs the new binary, re-runs `install-units` (which
    rewrites only what changed, and stamps every unit it writes with the new binary's
    `# llamaman-units: <N>`, clearing any `drift: stale` — D95), and restarts `llamaman.service`.
    **Instance units are never touched**, so an upgrade does not interrupt inference; the public
    gateway ports survive the restart through the fd store (§9.4). The DB, models and instances are
    untouched. **`--no-start` suppresses both step 9's start and this restart**, which is what makes
    this command usable as step 2 of §12.4's downgrade procedure: the older binary has to be on disk
    before the database is restored, and it must not be started in between, because until step 3 runs
    it would refuse the schema it finds and crash-loop (§11.1 step 4).
12. **`--uninstall`** — stop and disable `llamaman.service` and `llamaman-instances.target`, stop
    every `llamaman-instance@*.service`, remove units and polkit files, `daemon-reload`, remove
    `<prefix>/llamaman` **and `<prefix>/llamaman.prev`** — the retained previous binary is ours and
    lives in a directory that is not (§6.1, D89), so leaving it behind would leave a stray root-owned
    file on root's `PATH`. `/var/lib/llamaman` is kept (and its path printed) unless `--purge`.
    **It never touches the HF cache** — those models belong to the user, not to us — except that
    `--purge` on a `--dedicated-user` install prints the size of `/var/lib/llamaman/hf-cache` and
    requires a second explicit `--purge-models` before removing it, because that directory *is* the
    user's model library even though it lives under the state path.

Every user-visible message is prefixed `llamaman:` so failures are greppable, and an `err()` trap
prints the failing line.

---

## 14. Third-party dependencies

**Go** — the rule is stdlib-first, pure Go, and never anything that forces cgo.

| Module | Why |
|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite. The single-static-binary constraint rules out `mattn/go-sqlite3` (cgo, needs a C toolchain, breaks cross-compiling to arm64, produces a dynamically linked binary). |
| `github.com/coreos/go-systemd/v22` (`dbus` only) | Native D-Bus client: push-based unit state, typed properties, job completion (§5.3). Pure Go. `sdjournal` is deliberately **not** used (cgo). |
| `golang.org/x/crypto` | `argon2` for the admin password; `ed25519` is stdlib. |
| `golang.org/x/sys/unix` | `Statfs`, `Flock`, and `execve`/signal details not in stdlib. |
| `golang.org/x/sync` | `errgroup` and `semaphore` for the download and build pools. |
| `github.com/oklog/ulid/v2` | Sortable, coordination-free ids that double as SSE cursors. |
| `github.com/yuin/goldmark` | Renders HF model cards and release changelogs to HTML **server-side** (D35). |
| `github.com/microcosm-cc/bluemonday` | Sanitizes that HTML. Non-negotiable: model cards are attacker-controlled markdown containing raw HTML, and rendering them unsanitized in the origin that holds the admin session cookie is a stored-XSS hole. |
| `github.com/google/go-cmp` (test only) | Readable diffs in table-driven tests. |

Notably **absent**: `github.com/ebitengine/purego` (D16 — NVML via `dlopen` is untestable ABI risk in
a statically linked binary), a web framework (stdlib `ServeMux` with Go 1.22+ patterns plus ~150
lines of middleware is enough), an ORM (hand-written SQL against a schema this central is clearer,
and `sqlc` remains a reasonable build-time-only addition later), `cobra` (twelve subcommands; `flag`
suffices), a migration library (a ~120-line runner over embedded SQL has no failure modes we do not
want to own), `gguf-parser-go` (the HTTP-Range capability we need does not exist there — §8.5), a
minisign library (ed25519 verification of a checksums file is ~40 lines against stdlib), `creack/pty`
(no PTY is needed once builds are plain child processes), and `testify` (go-cmp plus stdlib keeps
the module graph small).

**npm** (Node 24)

| Package | Why |
|---|---|
| `react`, `react-dom` (19) | SPEC §5.4, current stable major per the owner's latest-stable directive (D45). |
| `typescript` (7.x), `vite` (8.x), `@vitejs/plugin-react` | Build tooling per SPEC §5.4, current stable majors per the same directive (D45). |
| `@tanstack/react-router` | Type-safe routes **and** search params — filters and comparisons belong in the URL. |
| `@tanstack/react-query` | Server-state cache that SSE frames can patch (§4). |
| `zustand` | ~1 KB store for wizard, draft and UI state. |
| `tailwindcss` (v4) + `@tailwindcss/vite` | Token-based dark-first theme, no runtime cost. |
| `@radix-ui/react-{dialog,dropdown-menu,tabs,tooltip,select,popover,switch,progress,toast}` | Accessible headless primitives; we own all styling. |
| `lucide-react` | Tree-shaken MIT icon set. |
| `react-hook-form` + `zod` + `@hookform/resolvers` | ~40 interdependent optional fields on the instance form. |
| `uplot` | Canvas charts for bench history at thousands of points, ~45 KB (D44). |
| `clsx` + `tailwind-merge` | Conditional class composition. |
| dev: `vitest`, `@testing-library/react`, `@playwright/test`, `eslint`, `typescript-eslint`, `prettier`, `openapi-typescript` | Test, lint, and generated API types. |

Versions are pinned in `package-lock.json` and `go.sum`; this document names major lines only,
because an unverifiable table of exact patch pins is worse than none. No CDN reference appears
anywhere; the Inter and JetBrains Mono subsets are vendored into `ui/public` and served from the
binary (SPEC §5.4).

---

## 15. Testing strategy

**Unit — fast, no I/O, where most of the value is.**

- `internal/model`: every transition table is table-driven — legal transitions succeed, all others
  return `ErrIllegalTransition`. (There is no generic state-machine engine to test; D42.)
- `internal/fit`: golden tests against ~20 recorded real loads in `testdata/fit/` (model shape +
  flags + observed buffer sizes from actual llama.cpp logs), asserting ±10% and, critically, that a
  verdict **never** says "fits" for a load that actually OOM'd. Dedicated cases cover a per-layer
  `head_count_kv` array, a sliding-window model, an MoE, and `-ngl all` versus `-ngl L`.
- `internal/gguf`: checked-in truncated headers (a few hundred KB each) for llama, qwen3, gemma3
  (per-layer KV heads + SWA), an MoE, a sharded set and an mmproj; plus `FuzzParseHeader`.
- `internal/instances`: argv rendering as byte-exact golden files — including one asserting that
  `n_gpu_layers.mode=="auto"` emits **no** `-ngl` argument (D51) — `config_hash` stability under JSON
  key reordering **and under a changed `internal_port`** (D52), the §2.8 port rules, rejection of
  forbidden `extra_flags`, and that no rendering path ever emits `CUDA_VISIBLE_DEVICES` (D66).
  `RenderBenchArgv` gets its own golden pair against the same `FlagSet` (D62), plus a case asserting
  it emits none of `-c`, `--host`, `--port`, `--alias`, `--props`, `--slots`, `--metrics` and that
  `flash_attn: "auto"` becomes `-fa 1` with a recorded note (§10.1).
- `internal/model`: the safe-start override patch — that it changes the rendered argv, that
  `effective_config_hash != config_hash`, and that `flags_json`, `config_hash` and `generation` are
  all untouched by it (D61).
- `internal/fit`: `SWALayers` against the upstream convention — pattern 6 over 26 layers, a window
  with no pattern (all layers full, note emitted), a pattern with no window (SWA off) — and the
  per-GPU verdict on an asymmetric pair (23 GB + 4 GB) asserting `wont_run` where a Σ-free-VRAM test
  would have said `fits` (§8.7).
- `internal/hw`: the MiB→bytes conversion at the `nvidia-smi` boundary, asserting a 24576-MiB card
  becomes `25769803776` (§8.6).
- `internal/model`: the derived-flag functions (`restart_required`, `stale_version`, `inhibited` +
  `inhibit_reason`, `draft_unverified`) as a truth table over `(state, config_hash, exe_version_id,
  restart_policy, last closed start's outcome, open row's override)`, asserting in particular that a
  `stale_version` or `inhibited` instance can still be `ready`, that an instance with **no** closed
  start row is none of the four, that a safe-started instance reads `restart_required` **while its
  override row is open**, and — the regression this table exists for — that once that row is closed
  and an ordinary start has taken over, `restart_required` is **false** again: the flag reads
  `THE_OPEN_ROW`, never `LAST_CLOSED`, so a single safe start cannot latch it forever (§2.8).
- `internal/instances`: `RenderArgv` purity as an executable property — a build tag'd test compiles
  the package with `internal/fit`, `internal/hw`, `os` and `time` stubbed to panic, renders every
  golden fixture, and asserts no panic; plus `-ngl auto` against a version row with
  `supports_fit=0` emitting `-ngl 999` and against `supports_fit=1` emitting nothing (§5.7), and
  `UnknownFlags` returning empty rather than everything when `help_flags_json IS NULL`.
- `internal/api`: the route registry is materialized into a real `net/http.ServeMux` inside a
  `recover()`, asserting no pattern panics at registration — the guard for the `{repo...}`-not-final
  class of mistake §3.6 fixes.
- `internal/llamacpp/github`: the `AssetArch` mapping table (`amd64→x64`, `arm64→arm64`), plus a
  case asserting that the Go arch string is never used verbatim in an asset name (§6.3).
- `internal/hf/cache`: path construction — **including `LockPath` against a fixture tree** — blob
  refcounting, delete planning, scan classification, symlink-less fallback, the six-rule hub
  detection chain over a table of environments (`HF_HUB_CACHE` set, legacy variables, `$HOME ==
  state_dir`, none of them), and a scan over a repo directory with **three** snapshot directories
  asserting three distinct `models` rows whose `revision` is the directory name and whose `ref_name`
  is set only for the one `refs/main` points at (§7.2).
- `internal/tokens`: mint/verify/epoch invalidation and constant-time behavior.
- `internal/bench`: sweep expansion (cross-product size and ordering) and llama-bench JSON parsing
  against recorded outputs.
- `internal/llamacpp/prebuilt`: `.gnu.version_r` parsing against checked-in ELF fixtures, asserting
  the "requires GLIBC_2.38, host has 2.36" message (D18).
- An **import-graph test** enforcing the D49 invariants.

**Integration — real SQLite, real HTTP, fake externals.**

- `store`: every migration applied to a fresh file *and* to a seeded older file; foreign keys and
  `CHECK` constraints asserted by attempting illegal writes.
- API: `httptest.Server` over the real router and a temp DB — auth/CSRF/lockout, the full instance
  CRUD → start → status path against a fake `Controller`, pagination, idempotency replay, and the
  **OpenAPI response-conformance middleware (D43)**: an undocumented endpoint, a missing documented
  field, or an extra field fails the suite.
- HF client and downloader: an `httptest` server serving canned `/api/models`, `/tree` and a
  Range-capable `resolve`, plus adversarial modes — ignore the range, truncate mid-stream, wrong
  ETag, 429 with `Retry-After`, and a redirect to a second host asserting the `Authorization` header
  is dropped. **Resume specifically**: an origin whose `ETag` differs from `x-linked-etag`, an origin
  that returns a *different* `ETag` on the second request, and an origin that returns a weak `W/`
  validator — each asserting that the request either carries a byte-exact strong validator or no
  `If-Range` at all, that the response is `206` rather than `200`, and that the final SHA-256
  verifies. A regression guard asserts the de-quoted blob name is never sent in any header. Resume
  correctness is asserted by hashing the final file. An optional job (skipped when `hf` is absent)
  asserts the real `hf` CLI recognizes the tree we wrote and — with a Go-side `flock` held — is
  actually blocked by it, which is the only test that can catch a correct path taken with
  `fcntl(F_SETLK)` (D27).
- Gateway: a stub upstream streaming SSE slowly — pass-through fidelity **including the
  no-`Accept-Encoding` case that D36 exists for**, immediate flushing, auth rejection shapes,
  accounting counters, client-disconnect propagation. Plus the usage tap (§9.3): a non-streamed JSON
  response whose `usage` lands inside the tail ring, one whose `usage` is pushed out of it (asserting
  `NULL`, not a wrong number), an SSE response with and without `include_usage`, and a 200 MB
  response asserting constant memory and byte-identical output — the proof that "extract usage" did
  not quietly become "buffer the response".
- Restart continuity (§9.4): a stub instance serving a token-authenticated stream while the daemon is
  restarted, asserting zero connection-refused, the socket adopted from `LISTEN_FDNAMES`, and the
  in-flight stream completing inside `gateway.drain_sec`; and the same run with the fd store forced
  unavailable, asserting `listener_continuity='none'` is reported rather than silently degraded.
- llama.cpp manager: a fake GitHub API plus a fake source tree whose `cmake` is a shell script
  printing Ninja-style progress and exiting 0 or 1 on demand — this exercises the whole phase
  machine, log streaming, cancellation, the symlink flip and rollback **without compiling
  llama.cpp**, and it is why this area can be tested at all.
- `instance-exec`: run against a **stub `llama-server`** (a small Go binary built by the test that
  serves `/health` 503→200, `/props`, `/slots`, `/metrics`, streams completions, and can be told to
  crash or hang). Asserts argv, env, and **every exit code in the §5.6 table** — each with a closed
  `instance_starts` row carrying the right `outcome`, `exit_code` and `error_code`, because a
  preflight failure that records nothing is the specific bug D54 exists to prevent. Also asserts
  that `pending_trigger` **and `pending_override_json`** are consumed and cleared in one transaction,
  that a second start after a safe start renders the saved configuration, and that an unstamped start
  records `external`. The schema gate (§5.6a) is exercised three ways: DB behind the binary and the
  daemon migrates during the wait (proceeds), DB behind and no daemon appears (exit 75 after the
  bounded wait, ledger row closed), DB ahead of the binary (immediate exit 75).
- Supervisor: a table-driven reconciler suite over `(host_boot_id changed?, autostart,
  desired_state, unit state)` asserting the D53 coupling — an `autostart=1` instance is **not**
  stopped on the first pass after a host boot, an `autostart=0` instance is **not** started, and a
  daemon restart inside one host boot still repairs a crash; and `ReassignInternalPort` leaving
  `generation` and `config_hash` untouched. **The whole boot sequence is driven, not just the
  reconciler**, with a specific assertion that §11.1 step 9 leaves `runtime_info.host_boot_id`
  unchanged and that §5.8 step 1 is the row's only writer: a fake `/proc` whose `boot_id` changes
  between two daemon starts must fire the coupling exactly once, and a suite variant that lets step 9
  persist the value asserts the coupling is missed — the failure mode is reproduced deliberately so a
  future refactor cannot reintroduce it silently.
- Instance state machine: the `ready → failed` row exercised through the stub server exiting **0**
  with no stop requested, asserting `instance_status.state='failed'`, the ledger row closed
  `outcome='stopped'`, `restart_policy='on-failure'` declining, and the derived
  `inhibit_reason='clean_exit'` actually reachable (§2.8); plus `crash-looping → stopped` from
  reset-failed and from safe-start, and `unknown` re-deriving to the observed state when properties
  become readable again.
- `config_hash` maintenance (D69): activating a second llama.cpp version rewrites `config_hash` for
  every non-deleted instance inside the activation transaction, leaves `generation` and
  `applied_config_hash` alone, and makes `restart_required` true for every running instance — and a
  download completing rewrites it for exactly the instances referencing that model and no others.
- Build lease (D70): two `interrupted` `llamacpp_install` jobs on different version ids retried
  simultaneously produce exactly one `running` build, the other staying `queued`; a lease whose
  `owner` is a dead `boot_id` is reclaimed at boot; and cancelling the holder releases it.
- Bench lease (D75), the same suite over `bench_lease`: two `bench_run` jobs on **different** runs
  started simultaneously produce exactly one `running` sweep and one `queued` "waiting for the running
  benchmark" — the case `idx_jobs_one_live_per_subject` cannot express and that would otherwise have
  let two sweeps overwrite each other's `stopped_instances_json`; a lease whose owner is a dead
  `boot_id` is reclaimed at boot **but only after** the restore finalizer has set `restore_done=1`;
  and §6.6 step 1 refuses an activation both while the lease is held and while any run has
  `restore_done=0 AND stopped_instances_json IS NOT NULL`, with the lease released in the first case
  and not the second.
- Canary revert (D24/F19), asserted as a *row* revert and not only a symlink one: a version flip whose
  canary never reaches `/health` leaves `is_active`/`previous_active` at their pre-activation values,
  leaves `config_hash` equal to what it was before the activation for **every** non-deleted instance
  (so no instance is left with a false `restart_required` against a rolled-back version), enqueues no
  `llamacpp_delete` for the outgoing `previous_active` row, and — the regression this case exists for
  — a **second daemon boot afterwards does not re-activate the failed build**, which is exactly what
  §6.6's "the row wins" reconciliation would have done had only the symlink been reverted.
- Activation interruption (§2.3/§6.6): a `llamacpp_activate` job whose lease owner died becomes
  `interrupted` with the version rows untouched, and boot reconciliation closes it `succeeded` when
  the step-3 transaction had committed and `failed`/`daemon_restarted` when it had not — the two
  states §2.3a's `interrupted` cell asserts are reachable.
- Forced rebuild of the active version (D78): `force_rebuild` on the `is_active=1` id with no running
  instances installs into `versions/<id>.staging`, never writes into the directory `versions/active`
  resolves to, swaps with two renames at `publish`, and — asserted with a start attempted mid-rebuild
  — the supervisor takes no start action and a hand-run `instance-exec` exits **69** with
  `runtime_rebuilding` rather than exec'ing a partially installed `llama-server`.
- Version identity (D71): re-posting a `failed` id reuse-and-resets the same row to `pending` and
  rotates its log; re-posting a `ready` id with identical options returns `200 reused`; with
  different `cmake_extra` returns `409 version_options_differ`; while live returns
  `409 build_in_flight`; and two custom builds from different `git_url`s resolving to the same short
  SHA produce two distinct rows.
- Instance deletion (D68): a soft delete frees the name and both ports for immediate reuse, keeps
  `instance_usage_daily` and `instance_starts` readable through `?include_deleted=true`, removes the
  gateway listener, disables the unit, and excludes the row from the reconciler, the port walk and
  the model in-use guard; `?purge=true` cascades all of it away. Three further cases cover the gaps
  that a happy-path-only suite hid: deleting a **running** instance leaves its open
  `instance_starts` row closed `stopped` by the **supervisor** and not by the handler (asserted by a
  writer-tagged fake, and by the row surviving a handler that crashes right after `StopUnit`);
  deleting on a host with `autostart_control: false` completes, skips `DisableUnitFiles`, and raises
  `unit_still_enabled` with the manual command; and starting the still-enabled unit of a deleted
  instance exits **64** without launching anything.
- Cache-root detach against soft-deleted instances (§7.2a): a root whose only referencing instance is
  soft-deleted returns `409 model_in_use` naming it — **not** a foreign-key error from inside the
  transaction — and succeeds after that instance is purged. The case is written against a real SQLite
  file with `foreign_keys=ON`, because the bug it guards is precisely a `RESTRICT` the guard did not
  anticipate.
- Autostart under a withheld grant (§5.8, §11.1a): with `polkit_unit_files=0`, an instance whose
  `autostart` disagrees with its unit's enable state produces **zero** `EnableUnitFiles` calls across
  many reconcile passes (asserted by call count on the fake Controller) and exactly one
  `autostart_unmanaged` notification — the error loop with no terminal state that an ungated
  corrective action would have produced.
- Job orphan recovery (§2.3): a `bench_run` job whose lease owner died becomes **`interrupted`** with
  `bench_runs.state` untouched, and the boot finalizer restores the stopped production instances,
  sets `restore_done=1` and closes both rows — the same assertion repeated with the run row forced to
  `canceled` and to `failed`, proving the sweep keys off `restore_done`, not off `state`; and a
  `self_update` job likewise survives as `interrupted` until §11.1 step 11 resolves it, with a case
  asserting the row is `succeeded` (not `failed`) after a confirmed update.
- Start-ledger semantics (D63/D64), as their own suite because they are where an implementer would
  otherwise guess: exactly one row per run with `outcome` written once; `/health` 200 stamping
  `ready_at` and leaving the row open; a crash after ready closing that same row as `failed` rather
  than opening a second; `applied_config_hash` written by the supervisor at ready and **not** by
  `instance-exec` (a launcher that execs and then dies leaves it unchanged); `on-failure` restarting
  after `outcome='failed'` and not after `'stopped'`; and the crash-loop counter asserting that six
  user start/stop cycles do **not** trip it, that `inhibited` rows do not accumulate toward it, that
  a rolling restart plus a bench stop/restore plus a user restart stay under `restart_max=5`, and
  that `reset-failed` and a 60-second-healthy run each clear the window. Three additions cover the
  ledger's own invariants, each of which used to be a comment rather than a constraint: the **unique**
  `idx_instance_starts_open` asserted first by inserting a second open row directly and expecting the
  constraint to fire, then through the real path — a launcher whose closing `UPDATE` is forced to fail,
  followed by a new start that closes the survivor `launcher_superseded` and proceeds normally; that a
  `launcher_superseded` row is **not** counted by the crash-loop query; and that an instance held
  `inhibited` across 200 reconcile passes accumulates exactly **one** `inhibited` row, that
  `LAST_CLOSED` still names the run before it, and that `inhibit_reason='clean_exit'` therefore stays
  true for as long as the instance is inhibited instead of flickering off one pass after it appeared.
- Cache roots: `SetPrimaryRoot` called from all three entry points (settings PATCH, promote, wizard)
  leaves `hf_cache_roots`, `settings['hf.hub_dir']`, `settings['hf.home']` and
  `runtime_info.hf_hub_dir`/`hf_home` in agreement — including an `HF_HUB_CACHE`-style root with no
  `/hub` suffix, where `hf.home` must be empty rather than wrong — keeps the old root's models
  resolvable, and refuses to detach the primary.
- Idempotency (D65): a repeat inside the window returns the same job with `200`, a different body
  under the same key is `422`, and the **same key after the window creates a new job** — the case a
  permanent unique index could never pass.
- Root-invocable subcommands: each of `install-units`, `status`, `doctor`, `verify-release`,
  `version` run as root against an empty state directory owned by another uid, asserting by directory
  diff that **zero** files were created — no `llamaman.db`, no `-wal`, no `-shm`, no `secret.key`
  (§11.3) — and that `doctor` reports its DB checks as `skipped` rather than failing.
- Setup token (§2.2a): minted to a 0600 file on first boot, printed by `status --json`, absent from
  `VACUUM INTO` snapshots and from a `diagnostics` bundle, unlinked on claim, re-minted when deleted
  while unclaimed, and a stale file removed at boot when the claim is already stamped.
- **Self-update, the happy path (§12.1–§12.3)**: stage → verify → snapshot → swap → restart →
  confirm, asserting that `update/pending` is written by the **daemon** (a test that deletes it after
  staging asserts `llamaman-selfupdate.service` reports "condition failed" and the update never
  starts), that the D14 snapshot exists and that retention keeps the newest 7 **and never the newest
  one, whatever the count is tuned to** — with a fixture of eight updates in a week asserting the
  snapshot labeled with `<prefix>/llamaman.prev`'s version survives, which is the case the earlier
  "newest for the installed version" predicate protected in name only — that the actor installs a
  binary it extracted from the tarball
  *it* re-verified rather than the daemon's `update/llamaman.new` (the staged file is asserted already
  unlinked at that point, which is the D89 (c) regression test), that `<prefix>/llamaman.prev` ends
  byte-identical to the binary that was replaced, and that the gate marks the row and its job
  `succeeded` and deletes the marker. A `pending` format fixture per historical shape is replayed
  against the current gate, plus one at `"format":99` asserting the **sweep** branch — not a
  do-nothing branch — which is the property that stops a file no reader understands from outliving
  every process that does.
- **The swap is atomic and same-filesystem (D89)**: a harness with `<prefix>` and the state directory
  on **separate mounts** runs the whole pipeline and asserts it succeeds — the direct regression test
  for the `EXDEV` the previous design would have hit on any host with a separate `/var` — and asserts
  that `<prefix>` never contains a partially written `llamaman` at any instant, by racing a reader
  that hashes it in a loop against the swap and accepting only the two known digests.
- **The revert (§12.2, D88)**, driven for real under a user manager in the `systemd-user` CI job
  because the trigger is a systemd state transition a fake cannot produce: an update whose new binary
  `exit 1`s immediately, and a second whose new binary sleeps forever without sending `READY=1`. Both
  assert that `llamaman.service` reaches `failed`, that `OnFailure=` starts
  `llamaman-update-verify.service`, that the judge performs **one** rename, that
  `<prefix>/llamaman.prev` is gone afterwards, that `ExecStopPost=` runs `reset-failed` **before**
  `start` (asserted by stripping `reset-failed` from the rendered unit and observing the "start
  request repeated too quickly" failure the ordering exists to avoid), and that the old daemon is
  serving again with the row closed `failed`/`update_not_applied` and **F20** raised.
- **The revert's trigger truth table**, the assertion that keeps it from firing when it must not: with
  `update/pending` present and `<prefix>/llamaman.prev` present, the judge is invoked by hand against
  each of `active`, `activating`, `inactive`, `deactivating` and `failed`, and asserts it renames on
  **`failed` alone** and exits 0 having touched nothing in the other four. Each of those five runs is
  driven through a `systemctl` stub that exits **3** while printing the state, asserting the verdict is
  read from stdout and that a non-zero exit is never treated as an error — the misreading that would
  invert this component — plus a sixth run where the stub is absent entirely, asserting the judge does
  nothing and exits 0. The `active` row is the one the previous design could not have passed: a
  healthy-but-slow daemon. Its companion asserts the reason it cannot arise at all — a boot whose
  migration takes 200 s sends `EXTEND_TIMEOUT_USEC=` and is asserted **not** to be killed, with no
  judge run recorded. A directory diff after every one of these runs asserts the judge created no
  `llamaman.db-wal` or `-shm`, i.e. that it never opened the database even to read a schema version.
- **The judge is disarmed across a migration (D92)**, driven under the user manager because it is a
  systemd state transition: an update whose new binary applies a migration, sends `READY=1` and then
  panics on every subsequent start asserts that `update/pending` is **absent** from the moment the
  first migration commits, that `llamaman.service` reaches `failed`, that
  `llamaman-update-verify.service` reports "condition failed" and performs **no** rename, that
  `<prefix>/llamaman.prev` still exists, that the host is left crash-looping on the new binary rather
  than dark, and that `doctor` names §12.4's five commands with the snapshot §12.1 step 4 just took. The negative control is the case D92 exists to prevent: the same run with the
  step-4 unlink disabled asserts the judge renames, the older binary then refuses with `schema_ahead`,
  the unit fails again, the judge is skipped, `ExecStopPost=` does not run and **no daemon is left** —
  the failure mode is reproduced deliberately so a refactor cannot reintroduce it silently. A third
  case asserts the same update with **no** schema bump still reverts normally, since there the revert
  is safe and correct. A fourth is the **cost** of the conservative disarm, asserted rather than
  assumed (§12.3 row 11b): a new binary whose **first** migration fails on seeded data that violates
  it — so nothing commits — is asserted to leave `update/pending` already unlinked, the judge skipped,
  the host crash-looping, and `MAX(schema_migrations.version)` **equal** to the newest snapshot's;
  `doctor` is asserted to name the shorter remedy for exactly that reading — `stop`,
  `install.sh --version <previous> --no-start`, `reset-failed`, `start`, and **no `restore-db` line**
  — and that sequence is then run and asserted to leave the older binary serving with every instance,
  token and benchmark created since the snapshot **still present**.
- **The revert deadline is a unit property, not a constant**: the rendered `llamaman.service` is
  asserted to carry `OnFailure=llamaman-update-verify.service`, `TimeoutStartSec=45`, `RestartSec=2`,
  `StartLimitIntervalSec=600` and `StartLimitBurst=5`, and the arithmetic
  `StartLimitBurst × (TimeoutStartSec + RestartSec) < StartLimitIntervalSec` is asserted from the
  parsed unit rather than from constants in the test — the second half being what stops a hanging
  daemon from looping in `activating` forever without ever reaching `failed`. A negative case strips
  `OnFailure=` from the installed unit and asserts `POST /update/apply` answers
  `409 revert_unavailable` rather than staging an unguarded update, and a second strips
  `llamaman-selfupdate.service` (and repeats with it masked) asserting `409 selfupdate_unsupported`
  **before** anything is staged — with the state directory diffed to prove `update/` was never
  touched.
- **Ordinary restarts do not consume the revert deadline (D93)**, under the user manager: six
  `POST /system/restart` calls spaced across ten minutes leave the unit `active` throughout and never
  produce "start request repeated too quickly", with `ResetFailedUnit` asserted to have been called
  once per boot and **not before** 60 s of readiness. The negative control disables the reset and
  asserts the unit does reach `failed` — and, with no `update/pending`, that the judge is inert, its
  `ExecStopPost=` never runs and nothing brings the daemon back, which is the outage D93 removes. A
  third case asserts `POST /system/restart` answers `429 restart_rate_limited` within the first 60 s
  of a boot and `202` after it, and a fourth asserts a binary that reaches `READY=1` and panics at
  50 s still reaches `failed` — the reason the reset waits. A fifth covers the **denied-grant
  topology (F26)**: with the `manage-units` grant withheld, `POST /system/restart` is asserted to
  answer `409 systemd_denied` — **never** `429`, since the endpoint cannot spend a start it is not
  authorized to make, and the two calls share one polkit action — and `doctor` is asserted to raise
  the start-limit check as a **warning** carrying `sudo systemctl reset-failed llamaman.service`. The
  residual itself is asserted rather than fixed: five out-of-band `systemctl restart`s inside the
  window leave the unit `failed`, the judge inert and no daemon, and the fixture exists so that a
  later claim to have closed it has to change this test.
- **Stop points (§12.3's table)**: all sixteen rows are driven for real — the daemon SIGKILLed in
  each of the four windows of §12.1, the actor refused at step 0 five ways (`--scope` removed from the
  unit, `pending` truncated, `pending.binary_path` pointed elsewhere, `checksums.txt.sig` corrupted,
  `<prefix>` filled to `ENOSPC`), the actor SIGKILLed in each of its three windows, a new binary that
  migrates and then panics (row 11a) and one whose first migration fails and commits nothing (row 11b), the judge refused and SIGKILLed, and the two `rename`s
  interrupted by a hard power-cut harness (`dm-flakey` over the loop device holding `<prefix>`). Each
  asserts the exact on-disk state its row names, that the actor wrote and restored **nothing** on
  every refusal, that **no step anywhere called `StopUnit` on `llamaman.service`** — a fake Controller
  asserts the verb is never issued by any actor in the whole suite, which is the structural property
  that replaced the previous design's verify-before-stop ordering — and that the state it left is
  exited by the mechanism its row names. **Rows 5, 6, 7 and 9 assert that mechanism is §9.4 step 7's
  120 s failsafe and not `ExecStopPost=`**: the actor's `systemctl start` is asserted to return with
  the unit already `active` and to queue no job, the daemon is asserted still blocked at that instant,
  and the recovery is timed to the failsafe — the assertion that would have failed against the earlier
  reading, which named a command that does nothing while a daemon is alive. Rows 12–14 assert the
  converse, that `ExecStopPost=` is what recovers a unit in `failed`. Row 14 is run twice, once with
  the machine rebooted between the power-cut and the assertion, because "nothing re-arms a timer
  across a reboot" is exactly what `OnFailure=` fixed.
- **The confirmation gate's three branches (§12.3)**: `target_version` equal to this binary's;
  not equal with `llamaman-selfupdate.service` **active** (asserting the deferral leaves row, job and
  marker untouched, and that a fake clock advanced by an hour does **not** change the answer — the
  regression test for measuring liveness with a clock at all); and not equal with no actor active
  (asserting the row closed `failed`/`update_not_applied`, the marker deleted and F24 raised). The
  deferral case is then re-run with the oneshot killed by its own `TimeoutStartSec=120` to assert the
  bound is systemd's: the very next gate call takes branch 3. Two further cases cover the marker whose
  `self_update_id` names **no row** — built by driving F12's fresh-DB arm under a surviving marker, and
  by `restore-db` to a snapshot older than the update — asserting each writing branch performs no
  domain write, still unlinks the marker, still clears scratch, still raises F24 with the versions read
  from the marker, and above all does **not** abort the boot. A third asserts the gate runs **before**
  `READY=1`: a probe that watches `$NOTIFY_SOCKET` asserts `update/pending` is already gone at the
  instant `READY=1` is observed.
- **The closing pass**: each of the three branches is re-run with an extra non-terminal `self_updates`
  row and `interrupted` job that the surviving marker does not name, asserting that row closed
  `failed`/`daemon_restarted` while the branch's own row and marker are resolved exactly as above, and
  — for the deferral branch — that the row **its** marker names is left untouched. The database-restore
  shape is built the way §12.1 step 4 builds it (a row in `verifying` beside a `running` job on a boot
  id that no longer exists) and driven through both producers: F12's boot recovery and
  `llamaman restore-db`. After each, the suite asserts no live `self_update` job survives and a
  subsequent `POST /update/apply` is **accepted** rather than answering `409 job_in_flight`.
- **The update guard (§12.1 step 1 / §3.14)**: a table-driven case over
  (job state × `systemd_control` × `llamaman-selfupdate.service` present/absent/masked ×
  `OnFailure=` present) asserting the four clauses and their four codes, run against both the endpoint
  and the prose's enumeration — the two lists are generated from one table in the test, so they cannot
  drift into disagreeing about how many clauses there are. The `selfupdate_unsupported` axis is
  asserted to be a fact about the **installed unit**, not about the running binary: a case with every
  unit present asserts the clause never fires, which no self-referential "this binary has no
  `selfupdate-apply`" predicate could ever have failed.
- **Self-update exclusivity (D97)**: two `POST /update/apply` calls issued concurrently with distinct
  `Idempotency-Key`s produce exactly one `self_updates` row and one `202`, the other answering
  `409 job_in_flight`; and the losing call is asserted to have deleted **nothing** from `update/` —
  the harness holds a file handle on the winner's in-progress tarball and asserts it is neither
  truncated nor unlinked. A variant with the guard deliberately moved outside the insert transaction
  asserts both calls succeed and the surviving marker names a deleted tarball, which is the hole the
  transaction closes.
- **Cancel (D96)**: `POST /jobs/{id}/cancel` on a `self_update` in `downloading` moves row and job to
  `canceled` in one transaction, clears `update/` scratch, and leaves no marker — with the `CHECK`
  asserted to accept `canceled` and §2.3a's invariant re-asserted. The same call at `staged` and at
  `swapping` answers `409 selfupdate_not_cancelable`, and the swap is asserted to proceed to a
  confirmed update regardless, since after that commit nothing reads `cancel_requested`.
- **Unit template drift is `stale`, not F16 (D95)**: units rendered by an older `install-units` (an
  older `# llamaman-units:` stamp) against today's binary are asserted to report `drift: stale`, raise
  **no** F16, and leave `POST /update/apply` **accepted**; the same files with the stamp bumped to the
  current value are asserted to raise F16. `self_update_revert` and the swap-unit clause are asserted
  to be computed from the units' own directives, by a case where the file is byte-different from the
  template but still carries `OnFailure=llamaman-update-verify.service` — the capability stays true and
  the update is staged.
- **Downgrade as an ordinary update (§12.4, D90/D94)**: `POST /update/apply` with an *older* tag runs
  the identical pipeline and is asserted to reach `succeeded` when no schema bump is involved. With
  one, the suite asserts the whole self-correcting chain end to end: the older binary refuses at §11.1
  step 4, `llamaman.service` reaches `failed`, the judge reverts, the newer binary is serving again,
  `<prefix>/llamaman.prev` is asserted **gone**, and the F24 card carries the older binary's own
  `schema_ahead` journal line **and §12.4's five commands**. Then two runs from that state:
  - the **destructive no-op**, asserted rather than assumed — `restore-db` alone against the v1.1.0
    snapshot succeeds, the newer binary starts, §11.1 step 4 migrates the restored database forward,
    the installed version is asserted **unchanged**, and the instances/tokens/benchmarks created since
    the snapshot are asserted gone. This is the case the earlier "then `restore-db` is run and the
    downgrade is asserted to stick" could never have passed;
  - the **procedure**, asserted to stick: `systemctl stop`, `install.sh --version <older> --no-start`,
    `restore-db`, `systemctl reset-failed`, `systemctl start` — after which `GET /api/v1/meta` reports
    the older version, the schema matches, `instance-exec` no longer exits 75, and the judge is
    asserted never to have run. A companion asserts `restore-db` printed its "will migrate forward"
    warning in the first run and did **not** print it in the third step of the second.

    **The five commands are run from both states the design prints them for, not only the
    self-corrected one.** The second driver starts from §12.3 row 11a — a new binary that commits a
    migration and then panics on every start, so `llamaman.service` sits in `failed` with its start
    limit **exhausted** and the judge inert — and asserts the same end state from there. Two
    assertions in that run are the point of the fifth command: with `reset-failed` **omitted**, the
    final `systemctl start` is asserted to be refused with "start request repeated too quickly" and
    the host to be left with no daemon, which is the failure the four-command reading shipped; with it
    present, the start is asserted to succeed **immediately**, without waiting out any part of the
    600 s window. The suite also asserts `reset-failed` is a harmless no-op in the self-corrected run,
    where the unit is `active` — which is why the procedure carries it unconditionally rather than as
    a branch a human has to evaluate mid-outage. A third driver covers §12.3 row 11b, below.
- **`llamaman restore-db` (§12.4)**: each precondition asserted to refuse — the lock held (naming the
  PID), a snapshot outside `db-backups/`, one failing `integrity_check`, one whose schema is newer
  than the binary — the "`<prefix>/llamaman` will migrate this forward" warning asserted to be printed
  when the installed binary's schema is newer than the snapshot's and **absent** when it is not, and
  the confirmation asserted to be required (a non-TTY without `--yes` refuses).
  The loss summary is asserted against a fixture with known deltas. A crash harness kills the command
  between each pair of its five steps and asserts, after every one, that a daemon started next opens a
  **working** database and that re-running the command completes. Run as root, the resulting
  `llamaman.db` and any sidecar are asserted owned by the database's uid/gid and 0600, by the same
  directory-diff harness the create-nothing test uses. A separate assertion is that **nothing else in
  the suite ever restores a database**: the judge, the gate and the endpoints are all asserted never
  to open a `db-backups/` file.
- **Actor scope argument (§12.2, §5.4)**: the rendered units are asserted to carry
  `selfupdate-apply --scope system` and `update-verify --scope system|user` matching the topology
  `install-units` was asked for, and two `ExecStopPost=` lines whose `@SYSTEMCTL@` is an **absolute**
  path (systemd rejects anything else) carrying `--user` in exactly the user-scope renderings, with
  `reset-failed` first. Each actor invoked **without** the argument is asserted to refuse, restore
  nothing, leave the marker and exit non-zero **without touching the database** (the state directory
  is diffed for a root-created `-wal`/`-shm` exactly as in the create-nothing test), with the
  notification asserted to come from the daemon's later gate instead; the F16 drift check is asserted
  to render the scope, so a correctly installed unit does **not** report drift and a hand-edited
  `--scope` does (§5.4a); and the judge's cross-version case is run both ways — a unit rendered by an
  older `install-units` driving today's `update-verify`, and today's unit driving a previous release's
  binary — since that `ExecStart` is `llamaman.prev`.
- **Installed binary versus running binary (F25)**: the boot check is driven by renaming a different
  build over `<prefix>/llamaman` under a running daemon and asserting the notification names both
  versions and offers Restart, and by the ordinary post-update boot asserting it does **not** fire.
- Schema gate accounting (§5.6a): an exit-75 run produces a **supervisor-synthesized** closed row
  with NULL `argv_json` and no row written by the launcher, and five such rows in one window do
  **not** move the instance to `crash-looping`, while five exit-72 rows do.
- Gateway accounting: an `auth_mode='none'` instance accrues `instance_usage_daily` rows with no
  token present (D56), and a token instance accrues both tables with the token table a strict subset.

**systemd (D47).** The D-Bus suite runs per-PR against a `systemd --user` manager inside
`dbus-run-session` on a stock GitHub runner, with the same unit templates rendered for user scope. It
exercises the identical `StartUnit` / `JobRemoved` / sub-state / `EnableUnitFiles` paths — no
privileged container, keeping SPEC §1's no-Docker posture clean even in CI. Covered end to end:
create → start → ready → token-authenticated streaming request through the gateway → accounting
rows; crash-loop → `crash-looping` → reset-failed; safe start through `pending_override_json` and the
next start reverting to the saved config; version flip → canary failure → **automatic revert of the
rows and both symlinks**, with a subsequent daemon restart asserted not to re-activate the failed
build; self-update apply → confirm, and apply → a new binary that will not start → `llamaman.service`
reaching `failed` → `OnFailure=` → the judge's one rename → the previous binary serving again; a
downgrade to an older tag confirming normally, one across a schema bump self-correcting through the
same revert and then completed by §12.4's five commands, and one whose new binary migrates before it
fails, asserting the judge is **skipped** rather than reverting across the migration (D92); six
restarts across ten minutes leaving the unit `active` throughout (D93); download resume across a
killed daemon; and a daemon restart asserting the gateway socket survives it (§9.4).

**The user-scope job exercises the D2 topology as a topology, not only as a bus.** Earlier drafts ran
user scope for the D-Bus verbs alone, which is exactly why several `--user-units` inconsistencies
survived review. It now additionally asserts: the daemon unit has **no** `After=user@%U.service`
(that ordering is impossible from inside `user@<uid>.service` and would be circular); **no rendered
user unit mentions `network-online.target`** in any `After=` or `Wants=`, including the instance
template and the target (§5.2a item 1); the binary is under `~/.local/bin`, **owned by the service
identity, mode 0755**, in a `~/.local/bin` the installer itself created on an account that did not
have one (§13 step 5 — a root-owned binary there is one the in-process self-update could never
`rename` over, and a missing directory is an install that fails before it creates the state tree);
`install-units --prefix` rendered both `ExecStart=` lines accordingly **and rendered
`serve --scope user`**, with `runtime_info.systemd_scope='user'` asserted to come from that flag
rather than from a guess (§11.1 step 1); **the state directory is
`$XDG_STATE_HOME/llamaman`** — the daemon's `runtime_info.state_dir`, the unit's `%S/llamaman`
`WorkingDirectory=`/`ReadWritePaths=`, `$STATE_DIRECTORY` and the `flock`ed `llamaman.lock` all
naming the same existing directory, with an explicit assertion that nothing anywhere resolves to
`~/var/lib/llamaman` **or to `/var/lib/llamaman`** (D72) — while the judge's `ExecStart=` names
`~/.local/bin/llamaman.prev`, under the **prefix**, which is the one self-update path in this design
that is deliberately not state-directory-relative (D89); `llamaman-selfupdate.service` is **absent**
and `selfupdate-apply` refuses to run; the in-process self-update path completes end to end,
including a run whose new binary will not start, which the user manager's own `OnFailure=` reverts,
with the single ownership predicate (the same uid as `<prefix>/llamaman`, not a literal uid 0)
asserted to **accept** the daemon-made `llamaman.prev` — the case that would otherwise refuse
forever; and that a root-invoked
`install.sh --user-units --user <other>` enables the units in *that* user's manager via the
`enable-linger` + `runuser` sequence of §5.2a, asserted with `systemctl --user is-enabled` run as
that user and with no system-scope `enable` issued.

A second job on `ubuntu-24.04` installs the **system-scope** units via the real `install.sh` and
repeats the smoke path — additionally with `--prefix /opt/lm-bin`, asserting the units point there,
the daemon starts, and a self-update replaces `/opt/lm-bin/llamaman` and not `/usr/local/bin` — so
both topologies and both prefixes are exercised. A `--dedicated-user` matrix leg additionally asserts
the journal grant end to end (D77): the account is created with `useradd --system`, `install-units`
puts it in `systemd-journal`, `runtime_info.journal_read` is `ok`, and
`GET /instances/{id}/logs` returns lines rather than an empty stream — with the grant deliberately
removed in a second run to assert `journal_read='denied'`, the `409 journal_unavailable` and the F23
card, which is the failure this leg exists to make impossible to ship silently. A third, short job runs the daemon in a container
with **no systemd at all**, asserting it starts, serves the UI, completes a cache scan and a fit
estimate, and returns `409 systemd_unavailable` from every instance control (F10/§11.1a).

**End-to-end.** Vitest + Testing Library for components, hooks and the event-stream reducer;
Playwright against a real binary built with `-tags e2e` using the stub llama-server and a fake HF
server, walking wizard → install runtime → download model → create instance → issue token → call the
gateway → run a bench → export. Screenshots and traces on failure.

**`install.sh`.** shellcheck, plus the `ubuntu-24.04` job above, which also asserts that a truncated
copy of the script (D48) executes nothing.

**What CI enforces.** `go vet`, `staticcheck`, `golangci-lint`, `gofumpt -l`, `govulncheck`,
`tsc --noEmit`, `eslint`, `prettier --check`, shellcheck, `go test -race ./...`, an `openapi.json`
drift check (`make openapi && git diff --exit-code`), a **unit/subcommand cross-check** that parses
every `ExecStart=` in the embedded unit templates and asserts each names a subcommand the dispatcher
in `cmd/llamaman` actually registers (this is what makes the §1 list authoritative rather than
aspirational, and it is why `selfupdate-apply` cannot go missing again), a bundle-size budget, and
coverage floors of 80% overall with 95% on `internal/{model,fit,gguf}`.

**Not testable in CI, with compensating controls.** No GPU runners exist, so a real CUDA compile and
real GPU detection cannot run per-PR. Instead: a `cuda-configure` job in an
`nvidia/cuda:*-devel` container runs our exact cmake configure plus a time-boxed partial compile
(the only per-PR tripwire for CUDA flag breakage in the whole design); `nvidia-smi` parsing is tested
through an injected `Runner` with fixture outputs including malformed lines and a non-zero exit
(proving F14); and a nightly workflow re-resolves the live stable and nightly llama.cpp tags to
catch `nightly-tag.txt` or asset-name drift before a user does.

---

## 16. Build and release pipeline

### 16.1 Makefile

| Target | Does |
|---|---|
| `make dev` | `vite dev` on 5173 beside `go run ./cmd/llamaman serve`, with `LLAMAMAN_DEV_UI=http://localhost:5173` — the only environment variable in the project, developer-only, never needed by a user |
| `make ui` | `cd ui && npm ci && npm run build`, then sync `ui/dist` → `internal/web/dist` |
| `make build` | `make ui` + `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X …buildinfo.Version=…"` → `dist/llamaman` |
| `make build-all` | the above for `linux/amd64` and `linux/arm64` |
| `make test` | `go test -race ./...` and `npm run test` (the ui `test` script carries `--passWithNoTests` so the gate is green on a scaffold with no suites yet; drop the flag once the first real suite lands, so an empty run is again a failure) |
| `make e2e` | Playwright against a `-tags e2e` binary |
| `make lint` | gofumpt, vet, staticcheck, golangci-lint, govulncheck, eslint, prettier, shellcheck |
| `make openapi` | regenerate `api/openapi.json` from the route registry and `ui/src/api/schema.d.ts` |
| `make migrate-new name=add_x` | scaffold `internal/store/migrations/00NN_add_x.sql` |
| `make release-snapshot` | full local artifacts (tarballs + `checksums.txt`) without publishing |
| `make install-local` | build, then run `installer/install.sh --version local` against `dist/` |
| `make clean` | remove `dist/`, `ui/dist`, `internal/web/dist` |

`internal/web/dist` is gitignored except for a checked-in stub `index.html`, so `go build ./...`
works on a clean checkout and tells the developer to run `make ui`.

### 16.2 GitHub Actions

`ci.yml` (push and PR):

| job | steps |
|---|---|
| `lint` | Go 1.27 + Node 24 with caching; the full `make lint` set |
| `test-go` | `go test -race -coverprofile ./...`; coverage gate; artifact upload |
| `test-ui` | `npm ci && npm run test -- --coverage`; bundle-size budget |
| `openapi-drift` | `make openapi && git diff --exit-code` |
| `build` | `make build-all`; uploads binaries |
| `cuda-configure` | `nvidia/cuda:*-devel` container; clones the pinned llama.cpp; runs our exact configure plus a time-boxed partial compile |
| `systemd-user` | `dbus-run-session` + `systemd --user`; the full D-Bus control-plane suite **and the D2 topology assertions** (D47, §15) |
| `install-system` | `ubuntu-24.04`; real `install.sh` → system units → stub instance → `status` → `--uninstall` leaves the state dir intact; a second matrix leg with `--prefix /opt/lm-bin`; a third with `--dedicated-user`, asserting the journal grant and `runtime_info.journal_read='ok'` (D77) |
| `no-systemd` | plain container, no PID 1 manager; asserts the daemon starts degraded and returns `409 systemd_unavailable` from the control endpoints (F10) |
| `e2e` | on `main`, tags, or a label: Playwright with traces on failure |

`nightly.yml`: re-resolves the live llama.cpp stable and nightly tags and asserts the asset names and
`nightly-tag.txt` still parse; re-runs the published `install.sh` against the newest release.

`release.yml` (on tag `v*`):

1. Re-run `lint`, `test-go`, `test-ui`, `build`.
2. `make build-all` with `-trimpath` and version ldflags; produce
   `llamaman_<ver>_linux_amd64.tar.gz` and `_arm64.tar.gz` (binary + LICENSE + README).
3. Generate `checksums.txt`; sign it with the ed25519 release key held in repository secrets →
   `checksums.txt.sig`. The **public** key is committed and compiled into the binary (§12), together
   with the "next" key for rotation.
4. Generate release notes from Conventional Commits since the previous tag; publish the release with
   every artifact and `install.sh`.
5. A post-publish job installs the **published** tarball on a clean `ubuntu-24.04` runner via the
   **published** `install.sh` and asserts `/healthz` — so a broken release is caught before anyone
   else runs the one-liner.
6. SLSA-style provenance via `actions/attest-build-provenance` (published for external auditors; the
   trust root for self-update remains the embedded ed25519 key, because verification must work
   offline with no external tooling).

Reproducibility: `-trimpath`, `CGO_ENABLED=0`, pinned Go and Node versions, `npm ci` against a
committed lockfile, a committed `go.sum`, and `SOURCE_DATE_EPOCH` passed explicitly so the release
timestamp is the only non-reproducible input.

---

## 17. Failure and recovery matrix

Every row is a designed behavior with a UI remediation card, and every row has a test.

| # | Failure | Detection | Automatic action | User-visible recovery |
|---|---|---|---|---|
| F1 | Model load OOM (VRAM) | `cudaMalloc failed` / `out of memory` in the journal before ready | none — a restart would loop | record actual vs. predicted in `fit_observations` with `oom=1`; offer "retry with `-ngl` reduced to the fitting value" computed by §8 |
| F2 | Host OOM kill | `OOMPolicy=stop`, `oom-kill` in the kernel log | unit stops; no restart storm | suggest a lower `-c`, `--no-mmap`, or turning `--mlock` off |
| F3 | Crash loop | > `restart_max` **failed** starts in `restart_window_sec`, counting only `outcome='failed'` rows after `restart_window_reset_at` (D8/D64) | supervisor stops restarting; state `crash-looping` | **Safe start** — a one-shot start with `-ngl 0 -c 2048` delivered through the `pending_override_json` channel (D61, §3.10b), never persisted — plus **Reset failed**, which stamps `restart_window_reset_at` so the state genuinely clears |
| F4 | Model file deleted underneath | `instance-exec` exit 72 | none | one-click re-download from the recorded repo + revision, or re-point |
| F5 | Internal port taken | exit 78, closing the ledger row opened before preflight (D54) with `detail_json` carrying both ports | the **supervisor** reassigns from the 21000–21999 pool before the next start (`ReassignInternalPort`: no `generation` bump, no `config_hash` change — §5.8) | the start history shows the failed attempt with both ports and the new port on the next row; pool exhaustion stops the retries and raises a notification |
| F6 | Public gateway port taken by an unrelated process | listener bind error (the predictable collisions — management port, internal pool, another instance — are already `422 port_unavailable` at save time, §2.8) | the instance keeps serving internally; **the daemon still starts** | per-instance banner plus a port picker with a live bind probe |
| F7 | Active version deleted or corrupt | exit 69 | none | one-click rollback to `previous` |
| F8 | Version flipped under running instances | `/proc/<pid>/exe` mismatch (D25) | none | derived `stale_version` badge → "restart to apply llama.cpp b10621" **while the instance stays `ready` and keeps serving** (§2.8); the same signal guards GC |
| F9 | D-Bus or polkit denied | `CheckAuthorization` at boot; `AccessDenied` on a job | degrade to a read-only control plane. When only `manage-unit-files` is missing, the three call sites that need it are **gated rather than retried** (§11.1a): the autostart toggle returns `409 autostart_unavailable`, the supervisor skips its enable/disable action and raises `autostart_unmanaged` once, and instance deletion completes without `DisableUnitFiles` and raises `unit_still_enabled` | remediation cards: `sudo llamaman install-units --repair-polkit` for the denial itself, and the exact `sudo systemctl enable\|disable llamaman-instance@<name>.service` lines for the two gated cases |
| F10 | systemd absent | boot probe | **degrade, do not refuse** (D67): `systemd_control='unavailable'`, control plane read-only, everything else runs (§11.1a) | every instance control returns `409 systemd_unavailable` carrying the equivalent manual command; `GET /instances/{id}/command` prints the exact argv and env; `llamaman doctor` explains in one line at the top. There is **no** silent child-process fallback — the daemon never forks `llama-server`, because putting model processes inside the daemon's lifetime is precisely what SPEC §3.8 forbids |
| F11 | Two daemons (unit + hand-run) | `flock(llamaman.lock)` | the second exits 70 | the message names the holding PID |
| F12 | DB corruption | `PRAGMA integrity_check` at boot | move aside, restore the newest `db-backups/` entry whose schema this binary can open, else start fresh. This is the **only** automatic database restore in the design, it is for a database that will not open at all, and it chooses the newest usable snapshot rather than one the operator picked — `llamaman restore-db` (§12.4) is the deliberate kind and never runs by itself (D90) | notification listing what was lost; **instances keep running**. §12.3's closing pass closes any `self_update` row the snapshot resurrected |
| F13 | Disk full during a download or build | `ENOSPC`, plus the pre-flight free-space check | pause the download queue / fail the build cleanly | free-space breakdown with delete candidates and the "will free N GB" preview |
| F14 | GPU vanishes (driver reload) | `nvidia-smi` non-zero | mark GPUs `unknown`; **never fabricate numbers** | banner; the fit calculator degrades to RAM-only verdicts |
| F15 | journald volatile or rotated | build output missing from the journal | build output is also teed to `logs/build/<id>.log` | the log viewer falls back to the file |
| F16 | Unit or polkit files deleted, or hand-edited at the current template version | boot check and `doctor` (§5.4a): a missing file, or a content hash that differs while the `# llamaman-units: <N>` stamp matches this binary's | none | `sudo llamaman install-units` regenerates them. **An older or absent stamp is not this row**: it is `drift: stale`, the ordinary state of a host that self-updated across a release which changed a template, reported at `info` with the same command and blocking nothing — no F16, no refused update (D95) |
| F17 | HF cache on a filesystem without symlinks | probe at cache-root registration | fall back to copy-mode writes | warning explaining the extra disk cost |
| F18 | Build interrupted by a daemon restart | job owner ≠ boot id | job → `interrupted`, build directory kept (D4) | **Retry** re-runs against warm objects |
| F19 | Canary fails after a version flip | `/health` never reaches 200 | **automatic** revert of the activation: `is_active`/`previous_active` restored, `RecomputeConfigHash` re-run for every non-deleted instance, both symlinks repaired from the restored rows, canary restarted onto the old build, no `llamacpp_delete` enqueued (D24, §6.6 step 5) | notification with the captured journal tail (or the F23 hint when the journal is unreadable); no other instance was touched, and no instance is left wearing `restart_required` for a version that was rolled back |
| F20 | New binary will not start after self-update | `llamaman.service` exhausts `StartLimitBurst=5` starts of at most `TimeoutStartSec=45` and enters the **`failed`** state — at most ~235 s after the swap (D88, §5.4). That limit counts only consecutive starts that never became healthy, because a boot that stayed ready 60 s clears its counter (D93) | systemd starts `llamaman-update-verify.service` through `OnFailure=`. That unit **is** the retained previous binary (`<prefix>/llamaman.prev`, D13/D89); it checks its own ownership, re-asserts `is-active llamaman.service == failed` **read from stdout, not from the exit status**, and performs **one atomic rename** of `llamaman.prev` over `<prefix>/llamaman`. Its `ExecStopPost=` then runs `reset-failed` and `start`. It stops nothing, restores no database, opens no database and writes no marker: its trigger is the unit's `failed` state, and a unit in that state has no process left under it (§12.2). **The one case where `OnFailure=` fires for a daemon that *did* start — one that applied a migration and then failed — is disarmed upstream** by §11.1 step 4, which unlinks `update/pending` before the first migration runs, so the judge is skipped rather than restoring a binary into a database it can no longer open (D92, §12.3 rows 11a and 11b) | notification from the restored daemon, raised by §12.3's branch 3 when it finds an `update/pending` naming a version it is not: the row closes `failed`/`update_not_applied`, the card links the failed release's notes and carries the judge's own journal line, which is what distinguishes "reverted" from "the swap never happened". A judge that **refuses** changes nothing at all and says so in the journal; a judge whose rename fails is retried by `OnFailure=` up to its own `StartLimitBurst=5` per hour, and the manual equivalent is the same one rename: `sudo mv <prefix>/llamaman.prev <prefix>/llamaman && sudo systemctl reset-failed llamaman.service && sudo systemctl start llamaman.service`. In the disarmed migration case there is no revert and no card from a restored daemon — the exit is `doctor` plus §12.4's five commands, which §12.3 row 11a names — or, when the first migration failed and moved nothing, the shorter re-install of row 11b |
| F21 | Unexpected `*.wants` link, `*.requires` link or masked unit appears | boot unit-drift check and `doctor` enumerate them and diff against what `install-units` wrote (§5.2) | none — reporting only | remediation card naming the unit; this is the compensating control for the unscopeable `manage-unit-files` grant, and `install-units --no-autostart-grant` removes the grant entirely |
| F22 | GPU order changed under a configured instance | `instance_status.device_map_json` disagrees with the `device_uuids` recorded in `flags_json` (§5.7) | none — the instance keeps running on whatever it was given | banner: "`CUDA1` is now <name>, not the card this instance was configured for", with one click to re-resolve the UUIDs to current indices and a `restart_required` flag |
| F23 | The service identity cannot read the journal (D77) | boot probe into `runtime_info.journal_read` (§5.3, §11.1 step 6): `journalctl` runs and returns nothing for a unit that has demonstrably logged | none — the grant is a root-only change; every journal-derived feature degrades **loudly** instead of returning empty: `409 journal_unavailable` from `/system/journal` and `/instances/{id}/logs`, the §5.8 fit observation skipped (reports stay `confidence: "modeled"`), F19's notification carrying a hint instead of a tail | remediation card with `sudo usermod -aG systemd-journal <identity> && sudo systemctl restart llamaman.service`; `sudo llamaman install-units --identity <user>` applies the same grant. The common trigger is `--dedicated-user`, whose `useradd --system` account has uid < 1000 and therefore does not get its own `SplitMode=uid` journal |
| F24 | A self-update did not take — `update/pending` survived with no actor working on it | §12.3's gate, run at boot, on a 30 s ticker while the marker exists, and ahead of `POST /update/apply`: `pending.target_version` is not this binary's version **and** `llamaman-selfupdate.service` is not active (D91). An unreadable or unknown-`format` marker takes the same branch | close the `self_updates` row `failed` with the job's `error_code='update_not_applied'`, delete the marker, clear `update/` scratch — in one transaction, then the closing pass for any row a restored database resurrected. A marker whose `self_update_id` names no row is a no-op that still deletes the marker and still raises this card (§12.3). Nothing else is touched: not `<prefix>/llamaman`, not `<prefix>/llamaman.prev`, not `db-backups/` | notification carrying the journal tail of both actor units (or the F23 hint) and stating **which version is actually installed**, with Retry (`POST /update/apply`) on the card. This is the one row that covers every way §12.3's table stops short of a confirmed update, including a swap that was never attempted, an actor that refused at its preflight, and a revert that already happened. When the cause was a **downgrade** whose schema gate refused, the card prints §12.4's **five-command procedure** and the snapshot to use — and says why: the judge's rename consumed `<prefix>/llamaman.prev`, so the older binary has to be re-installed before any database restore means anything, and `restore-db` on its own would destroy everything created since the snapshot and change nothing else (D94) |
| F25 | The installed binary is not the one the daemon is running | boot check and `doctor`: sha256 of `<prefix>/llamaman` against the running process's own image (`/proc/self/exe`) | none — reporting only. The running process is unaffected; its executable is already mapped | notification naming both versions and offering Restart. It catches an admin who copied a binary into place under a live daemon, a half-finished manual repair, and the one narrow window §12 leaves: a human running `systemctl start llamaman.service` in the microseconds between the judge's `is-active` check and its rename. Cheap, general, and it turns the only residual race in the self-update protocol into a card instead of a silent version mismatch discovered at the next restart |
| F26 | **Start-limit exhaustion on a host that may not clear its own counter** — the `manage-units` grant refused (F9), so D93's `ResetFailedUnit` never runs and `StartLimitBurst=5` in `StartLimitIntervalSec=600` is the whole budget | boot: `CheckAuthorization` records the denial. `doctor` re-checks it on every run and reports it as a **warning** rather than an aside; `GET /system/capabilities` already carries the denial as `instance_control: false` | **none is possible** — the one call that would fix it is the call that was refused, and `POST /system/restart` answers `409 systemd_denied` rather than spending budget it cannot replace. The budget is therefore consumed only out of band: a human's `systemctl restart llamaman.service`, and each `install.sh` re-run (§13 step 11) | the stated residual (§11.1a): five such starts in ten minutes leave the unit `failed` with "start request repeated too quickly", the judge inert (no `update/pending`), its `ExecStopPost=` unrun, no daemon, and — once the unit fully fails, `FileDescriptorStorePreserve=restart` — the public gateway ports released. The remediation card carries the one line that recovers it and the one that prevents it, `sudo systemctl reset-failed llamaman.service`, plus `sudo llamaman install-units --repair-polkit` to remove the condition and the `--user-units` topology for a host that will not grant it |

---

## 18. Risks and build order

**Top risks and their mitigations.**

1. *Fit-calculator accuracy* — the calibration table (§8.7), llama.cpp's own `--fit` shown as ground
   truth, and the golden rule that a verdict may never claim "fits" for a recorded OOM. Never
   promise; always show the margin.
2. *polkit availability and correctness* — a boot-time `CheckAuthorization` self-test, a `.pkla`
   fallback for pre-0.106, the `--repair-polkit` command, and the `--user-units` topology as an
   escape hatch. The `manage-units` grant is name-scoped and fails closed, and `StartTransientUnit`
   is not in the interface. Residual, and stated rather than hidden: `manage-unit-files` carries no
   `unit` detail and therefore **cannot** be scoped by any rule; §5.2 names exactly what that grants
   (enable/link/mask any unit, but not start it — so the escalation is deferred to the next boot),
   pairs it with the F21 drift check, and offers `--no-autostart-grant` for hosts that decline the
   trade.
3. *llama.cpp flag churn across ~10 nightlies a day* — `flags_json` rather than columns (D41), the
   `extra_flags` escape hatch, argv validated against the per-version `--help` capture, and unknown
   flags surfaced as warnings rather than errors.
4. *Shared HF cache races with other tools* — the exact `huggingface_hub` lock path (D27) built by
   one function and asserted by a test, honoring pre-existing blobs, single-stream `.incomplete`
   files that either tool can resume, and a startup sweep that only touches partials we own.
   Residual: HF's Xet-backed storage migration could change the cache layout; that is tracked, and
   the layout lives behind `internal/hf/cache` so it is one package to change.
5. *Distro portability of prebuilt binaries* — D18's execute-on-this-host acceptance test with the
   glibc-version diagnosis and automatic fallback to a source build.
6. *Self-update bricking the host* — the external judge that **is** the retained previous binary and
   is started by `OnFailure=` on `llamaman.service`, so its trigger is the failure itself and needs no
   daemon, no timer and no clock (D12/D13/D88); the marker unlinked **before** the first migration, so
   that trigger can never mean "restore a binary into a database it can no longer open" (D92); the
   start-limit counter cleared after a healthy boot, so the revert deadline counts bad starts rather
   than a budget the restart button spends (D93); the install and the revert each being one atomic
   `rename()` inside one root-owned directory, so there is no intermediate on-disk state to crash into
   and no `EXDEV` to fail on (D89); the rule that **no step in the protocol stops a unit**, which is
   what makes "a refusing actor leaves the host as it found it" true by construction rather than by
   ordering; the guards that refuse to stage an update at all when either privileged actor is not
   installed (`409 revert_unavailable`, `409 selfupdate_unsupported`); deferral bounded by a unit's own
   `TimeoutStartSec=` rather than by two constants two releases must agree about (D91); the pre-update
   DB snapshot that is never restored by any actor, only by a human, and only as step 3 of the
   five-command procedure that actually completes a downgrade (D14/D90/D94); the closing pass that
   resolves a `self_update` job a restored database resurrected and no marker names; and the fd-store
   listener continuity
   that makes "running instances unaffected" true for API clients and not only for `llama-server`
   (D58, §9.4). The integration test self-updates while a stub instance serves a token-authenticated
   stream and asserts **zero connection-refused errors and zero requests dropped inside
   `gateway.drain_sec`** — a claim this architecture can actually meet, unlike the unqualified "zero
   dropped requests" an in-process listener set could never have satisfied. The
   `listener_continuity='none'` fallback is tested separately and asserted to report itself.
7. *Cross-version contracts owned by the older binary* — reduced, deliberately, to **one file with
   one parser**: `update/pending`, plus the `--scope` argument on the judge's `ExecStart` and the
   `db-backups` naming. §12.1 states the freeze rules (add-only, ignore-unknown) and, more usefully,
   removes the class rather than managing it: the judge parses **nothing**, deciding on the marker's
   existence and a unit state alone (D13), and an unknown `format` is *swept* rather than deferred to,
   because the precondition for sweeping is a fact about processes rather than about file contents
   (D91). The one remaining reader is the confirmation gate, and it reads in both directions — a newer
   binary confirming an update, an older one after a downgrade. CI keeps a fixture of every historical
   `pending` shape in `testdata/update/`, replayed against the current gate, and renders the judge unit
   from each supported release against each supported binary.

**Build order** — each stage is independently useful and independently demoable:

1. Schema + migrations + job queue + events/SSE.
2. Auth, session, wizard password, port walk, `llamaman status`, `doctor`.
3. systemd controller + `instance-exec` + instance CRUD + supervisor, against the stub llama-server.
4. llama.cpp manager: prebuilt path first (with D18), then source and CUDA.
5. GGUF parser + cache scan (this alone makes existing models visible, which is the first moment the
   product feels real).
6. HF client + downloader.
7. Fit calculator + calibration.
8. Gateway + tokens.
9. Bench runner.
10. Self-update.
11. `install.sh`.
12. Polish: presets, exports, comparisons, diagnostics bundle, accessibility pass.

---

## 19. Known open points

**None blocking.** The self-update marker protocol was rewritten rather than repaired. Three
consecutive adversarial passes over its previous form each closed the reported holes and each was
followed by new ones of the same two classes — a fact on disk outliving the process that gave it
meaning, and two actors disagreeing about which state the other was in — so D87 removed the machinery
that generated them instead of patching it again: the on-demand rollback endpoint, three of the four
`update/` files, both actors' second branch, four of the gate's five branches, the two frozen deadline
constants, the four-caller sweep, `llamaman update-clear`, three `self_updates` columns, two of its
states, and one failure row. SPEC §3.8 asks for one-click **forward** self-update; that is now what
§12 describes, plus one revert.

The sections that carry it are §2.3/§2.3a (what an `interrupted` `self_update` may pair with, and the
one cancel cut-off), §2.11 (the eight-state enum and the retention rule), §3.14 (one endpoint, four
guard clauses), §5.2 (a narrower polkit allowlist), §5.2a (the user-scope topology and the single
ownership predicate), §5.4 (`OnFailure=` and the start limit that *is* the deadline), §5.4a (the
template stamp, the drift check and the two directives that gate staging), §5.6a and §11.1 step 4 (the
two schema gates and the disarm that precedes the migration), §11.1 steps 11–12 and §12.3 (the gate
ahead of `READY=1`, its three branches, its closing pass and the sixteen-row stop-point table), §12.1
(the pipeline, the one transaction and the one frozen file), §12.2 (the swap and the judge), §12.4
(downgrades, the five-command procedure and `restore-db`), §11.3, §13, F12/F16/F20/F24/F25/F26 in §17,
and the fixtures in §15.

**A fourth adversarial pass over the simplified protocol found eleven defects and none of them needed
new protocol**: they were closed by D92–D97 plus corrections to D14, D88 and D90 — one unlink moved
earlier, one counter cleared after a healthy boot, one snapshot predicate replaced with a simpler and
correct one, one procedure written down that had been assumed, three prose claims about `ExecStopPost=`
and about `is-active` made to match what those commands actually do, one guard clause given a real
predicate, one guard moved inside the transaction it was always meant to be in, and two undefined
branches defined. No marker was added, no actor gained a branch, and nothing removed by D87 came back.

**A fifth pass found five more, and again none needed new protocol** — they were closed by D98 plus
amendments to D92, D94 and D87 and by finishing four printed artifacts. What changed, and what is
therefore no longer open: the downgrade procedure is **five** commands, not four — `reset-failed`
before the final `start`, matching the four other places this design already handles that systemd
semantic, since every state the procedure is printed for was reached by exhausting the start limit and
`stop` does not clear the rate limit (D94, §12.4, and §15 now drives the procedure from the row-11a
crash-loop state as well as the self-corrected one); the conservative disarm's **cost** is stated
rather than implied, §12's opening claim no longer promises an automatic revert for a first migration
that fails, and §12.3's row 11 is split into 11a (a migration committed — the five commands) and 11b
(the first migration failed and moved nothing — re-install the previous binary alone, no restore, no
data discard), which `doctor` tells apart from `MAX(schema_migrations.version)` against the newest
snapshot's (D92); `POST /system/restart` has a defined polkit-denied response that wins over the 429,
and the 429's unreachable `ResetFailedUnit` sub-reason is gone (D98); D87's removal list no longer
claims a failure row that is live under the same number, which was reassigned; and the
`# llamaman-units: <N>` stamp is present in all four printed unit templates with its position fixed,
without which every host would have read as a permanent `drift: stale` and F16 would have been
unreachable (D95, §5.4).

**One residual came out of that pass, and it is in the design body rather than here** because it is a
stated limitation, not an open question: on a host that withholds the name-scoped `manage-units`
grant, nothing in the product can clear or protect the unit's start-limit budget, so out-of-band
starts can still exhaust it and leave the unit `failed` with the judge inert and no daemon. §11.1a
states it, F26 gives it a row and a remediation card, D98 records the decision to name it rather than
pretend to guard it, and §15 asserts it so a later claim to have closed it has to change a test.

**What was resolved by removal**, and is therefore no longer an open question: whether a stale
`rollback-info.json` can make a Roll back button restore a pre-update database over an unchanged
binary (there is no button and no such file); whether `rolled-back.json`'s frozen payload can
reconstruct the row it must insert (there is no such file, and no row is ever reconstructed from
disk); whether a refusing actor can leave the host with no daemon (no actor stops one); whether the
automatic revert can be configured off by raising a settings value (its deadline is a unit property,
not a clock started by the daemon); whether two markers can coexist incoherently (there is one
marker); and whether an offline repair command's closing pass is reachable (the command is gone, and
its one genuine job — resolving a marker the daemon can see — always was the daemon's).

**Five properties to preserve when implementing this, and to re-check on every change under
`update/`:**

- **No step in the protocol stops a unit.** The daemon stops serving itself (§9.4 step 7); the actors
  only ever start things. That is what makes "an actor that refuses leaves the host exactly as it
  found it" true by construction rather than by an ordering argument, and it is why the judge can be
  three checks and a rename. A new step that stops a unit is a change to this property and to §12.3's
  table, not only to the code. Its corollary is that an actor's `ExecStopPost=` `start` is a backstop
  for a unit that is `failed` or `inactive` and a no-op while the daemon is alive — §12.3 rows 5, 6, 7
  and 9 are exited by §9.4 step 7's failsafe, and any row that claims otherwise is wrong.
- **Every change to the installed binary is one `rename()` between two names in one directory.** No
  copy over a live path, no cross-filesystem move, no two-step install. That is what makes the
  power-loss rows of §12.3's table one line each. A new install path is a change to D89.
- **Liveness is asked of systemd, never of a clock.** Deferral is bounded by the deferred-to unit's
  own `TimeoutStartSec=`, and the revert's deadline is the daemon unit's own start limit. A new
  deadline expressed as a constant compiled into two binaries is a change to D88 and D91, and it is
  the exact shape of the defect that took three passes to stop reproducing.
- **The revert is disarmed before the database moves past the binary it would restore.** §11.1 step 4
  unlinks `update/pending` before the first migration is *attempted*, and the gate runs before
  `READY=1`, so `OnFailure=` can only ever fire for a daemon that never finished a boot (D92). Any new
  code between the schema gate and that unlink, and any change that moves the gate back after
  `READY=1`, re-opens the one path in this design that ends with a host that has no daemon at all.
  The disarm is deliberately keyed to "about to migrate" rather than to the first migration that
  commits, and the price — a boot whose first migration fails loses a revert that would have been safe
  (§12.3 row 11b) — is paid knowingly; moving it later to reclaim that case is a change to D92 and
  re-opens exactly the window above.
- **The privileged actors never open `llamaman.db`, not even read-only.** A root-created `-wal`/`-shm`
  is a database the service identity can never write again (§11.3). That is why the judge cannot check
  a schema version itself, and why the answer to "would the previous binary still open this?" is
  computed by the daemon at the one moment it has it.
