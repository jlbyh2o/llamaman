# Llama Man — v1 Specification

A system-level web application for managing llama.cpp on a Linux host: installing and updating llama.cpp itself, downloading GGUF models from Hugging Face, configuring and supervising `llama-server` instances, issuing API tokens, and benchmarking. **No chat interface, no inference UI — management only.**

Status: agreed via interview on 2026-08-28. Decisions below are settled; items under "Assumptions (veto window)" are proposed defaults awaiting objection; items under "Open" are consciously deferred.

---

## 1. Settled decisions

| Area | Decision |
|---|---|
| Stack | Go backend, React + TypeScript frontend embedded via `go:embed` — ships as one static binary |
| Compute targets (v1) | NVIDIA CUDA + CPU-only. AMD (ROCm/Vulkan) and Intel/generic Vulkan deferred to v2 |
| NVIDIA path | CUDA **source build only** — no Vulkan quick-start shortcut; the wizard verifies the toolchain up front |
| llama.cpp acquisition | Prebuilt release binaries where they exist (CPU) + source builds (CUDA, forks) |
| llama.cpp versioning | **One global active version** — all instances run it; updating swaps everyone |
| Release channels | Stable (semver `v0.x.y`) + nightly (`b####`) + custom (any git URL / tag / commit, e.g. forks) |
| Serving model | **Multiple named instances** — several `llama-server` processes on separate ports, each with its own saved settings and autostart flag |
| Model scope (v1) | Text-gen GGUFs + embedding models + multimodal (mmproj pairing) + speculative-decoding draft models. LoRA adapters deferred to v2 |
| Model storage | **Standard Hugging Face cache** (`$HF_HOME`, default `~/.cache/huggingface/hub`); models already downloaded by other tools are detected and shown as available; Llama Man downloads use the same layout |
| API tokens | **Built-in reverse proxy**: Llama Man owns the public inference ports, proxies to llama-server on internal localhost ports; instant issue/revoke without model reload; per-token stats; "no auth" is a per-instance toggle |
| Exposure | Trusted LAN, plain HTTP. TLS/WAN is the user's own reverse proxy (Caddy/nginx/Tailscale) |
| Login | Single admin password (set in wizard), session cookie, aggressive rate-limiting and lockout |
| Benchmarking | llama-bench orchestration with **persistent history + comparisons + export** (JSON/CSV/markdown) |
| Self-update | One-click self-update from the web UI (release feed + changelog); running instances unaffected |
| Installer | One-line `curl | sh`, **distro-agnostic**: drops binary + systemd units, creates service user, *checks* for toolchain (gcc/cmake/CUDA toolkit) and reports what's missing with guidance — never drives a package manager |
| Deployment | Native system-level install. No Docker, ever |
| Hosting | **GitHub from day one**: `github.com/jlbyh2o/llamaman` (personal account, repo created 2026-08-28, MIT license file included). Releases + installer served from GitHub |
| License | **MIT** |
| Configuration | **Zero hand-edited files** — no config files, no dotenv, no required environment variables. Built-in defaults + SQLite + the web UI cover every setting; download it, start it, do everything from the browser |
| UI feel | Professional, slightly technical |

## 2. Architecture

```
                     ┌────────────────────────────────────────────┐
 browser ── :5526 ──▶│  llamaman (Go daemon, systemd service)     │
                     │  ├─ web UI (embedded React)                │
                     │  ├─ REST API (session-cookie auth)         │
                     │  ├─ inference gateway (token-checking      │
 API clients ──────▶ │  │   reverse proxy, one public port per    │
  :port-per-instance │  │   instance ─▶ 127.0.0.1:internal-port)  │
                     │  ├─ llama.cpp manager (releases, builds)   │
                     │  ├─ HF client (browse, download, fit calc) │
                     │  ├─ instance supervisor (via systemd D-Bus)│
                     │  └─ bench runner (llama-bench, history DB) │
                     └────────────────────────────────────────────┘
                                      │ manages
                     llamaman-instance@<name>.service (one per instance)
                          └─ llama-server -m … --port <internal> --host 127.0.0.1
```

- **Instances are systemd template units** (`llamaman-instance@<name>.service`), not children of the llamaman process. This is why self-update and llamaman restarts never interrupt a running model. llamaman generates unit state and controls start/stop/enable via the systemd D-Bus API. Autostart-on-boot = the unit is enabled.
- llama-server always binds `127.0.0.1` on an internal port; the only way in from the network is through the token-checking gateway. `/health` and `/metrics` are polled by llamaman for status dashboards.
- State lives in SQLite (single file): instance configs, hashed tokens, benchmark history, download ledger, settings.

## 3. Feature areas

### 3.1 llama.cpp lifecycle
- Channels: **stable** resolves `v0.x.y` → its pinned `b####` nightly via the release's `nightly-tag.txt`; **nightly** lists recent `b####` prerelease tags; **custom** accepts any git URL + ref (fork support).
- CPU-only hosts: prebuilt `llama-<tag>-bin-ubuntu-x64.tar.gz` when applicable. CUDA hosts: always source build (`cmake -DGGML_CUDA=ON`), since no Linux CUDA prebuilts exist.
- Builds land in versioned directories (`versions/<tag>/`) with an `active` symlink. Although only one version is *active*, the previous build is retained for **one-click rollback**. Updating = build/download new → flip symlink → rolling restart of running instances (with confirmation).
- Build output streams live into the UI (it's a several-minute compile); failures show the log with the failing step highlighted.
- Toolchain preflight: detects gcc/g++, cmake, CUDA toolkit (`nvcc`), driver + NVML; reports versions and what's missing with links to distro docs. Never installs packages itself.

### 3.2 Model management (Hugging Face)
- Browse/search HF filtered to GGUF (`/api/models?filter=gguf`), works anonymous or with a stored token (needed for gated/private repos). Gated repos are detected (`gated != false`) and the UI links to the model page to request access — that step is browser-only on HF's side.
- Model detail view: rendered model card (README), repo stats, and **all quantizations** listed with true file sizes (HF tree API — `lfs.size`, not the pointer size), including multi-shard GGUFs grouped as one logical model and mmproj files paired automatically.
- **Fit calculator** per quant, per detected GPU: weights (file size) + KV cache (`n_layer × n_ctx × n_head_kv × (head_dim_k + head_dim_v) × bytes/elem`, honoring the `-ctk`/`-ctv` cache-type setting) + compute-buffer estimate + safety margin, against live VRAM (NVML) and system RAM. Verdict per quant: **fits in VRAM / partial offload (spills to RAM) / won't run**, with a recommended quant highlighted. Architecture and max context come free from HF's server-computed `gguf` metadata field; after download the local GGUF is parsed for exact numbers, and llama-server's own `--fit` projection is surfaced as ground truth at load time.
- **Storage = the standard Hugging Face cache.** Models live in the HF hub cache layout (`$HF_HOME`, default `~/.cache/huggingface/hub`, `models--{org}--{name}/snapshots/{rev}/…`) — not a private directory. On first run (and on demand) Llama Man **scans the cache and surfaces already-downloaded GGUFs as available**, mapped back to their HF repos with card/quant info fetched live. Llama Man's own downloads are written in the same layout (blobs + snapshot links), so the `hf` CLI, Python libs, and any other HF-aware tool see one shared cache. (llama-server's `-hf` flag and its separate `~/.cache/llama.cpp` dir are not used; instances always get explicit `-m` paths into the HF cache.)
- Downloads: resumable (HTTP Range), parallel-safe, progress with speed/ETA, checksum verification, download queue.
- On-disk management: per-model disk usage, totals, delete-from-disk following HF cache semantics — remove the snapshot and any orphaned blobs (with "in use by instance X" guard); also flags non-HF stray GGUFs found in the cache.

### 3.3 Instances
- Create/edit named instances: model + **first-class exposure of every llama-server load flag** — context size, `-ngl` (number/auto/all), batch/ubatch, parallel slots, flash attention, KV cache types, split mode/tensor split, mmproj, draft model + draft params (speculative decoding), embedding mode, aliases, and a free-form "extra flags" escape hatch so no upstream flag is ever unreachable.
- Each instance: public port, token policy (which tokens are valid, or auth disabled), autostart on boot, restart-on-crash policy.
- Live status: state (loading/ready/failed), VRAM/RAM in use, slots busy, requests served, logs (journald) streamed into the UI.
- Flag-set presets: save a settings bundle and apply it to other instances/models.

### 3.4 API gateway & tokens
- Tokens: `lm_…` random secrets shown once, stored hashed; per-token name, created/last-used timestamps, request counts; enable/disable/revoke instantly. Global or per-instance scoping. Per-instance "no auth required" toggle.
- Gateway passes through the OpenAI-compatible API unmodified; adds only auth + accounting. `/health` stays unauthenticated (matches llama-server behavior).

### 3.5 Benchmarking
- Wraps `llama-bench` with `-o json`; sweep builder for the cross-product flags (`-ngl` for the "GPU alone vs. GPU+system" story, batch sizes, threads, fa on/off, cache types, `-p`/`-n` sizes, `-d` depth).
- Guard: refuses to launch a bench while it would collide with a loaded instance on the same GPU (or offers to stop/restart around it).
- Every run persisted: model, quant, full flag set, llama.cpp tag, driver version, GPU, results with stddev. UI: history charts (pp/tg over time), side-by-side comparisons across models/quants/settings/llama.cpp versions, export JSON/CSV/markdown.

### 3.6 Setup wizard (first boot)
1. Set admin password.
2. Toolchain preflight (CUDA detection; report gaps and wait/re-check until satisfied, or proceed CPU-only).
3. Pick channel + version; build/install llama.cpp (streamed progress).
4. Optional HF token; detect the HF cache location and **scan it — any models already on disk are listed as ready to use**.
5. Search and download first model (fit calculator active during selection; download continues in background) — skippable when the scan already found models.
6. Create first instance from a downloaded model, sane defaults prefilled; optionally enable autostart.

### 3.7 Installer (one-liner)
- `curl -fsSL https://raw.githubusercontent.com/jlbyh2o/llamaman/main/install.sh | sh` → verifies systemd + arch, downloads the llamaman release binary from GitHub Releases, installs `/etc/systemd/system/llamaman.service` + instance template unit (service identity per §5.1b), creates `/var/lib/llamaman/`, enables + starts the service, prints the wizard URL.
- Idempotent (re-running = upgrade path); `--uninstall` supported. No package-manager calls.

### 3.8 Self-update
- Checks the release feed, shows changelog, swaps its own binary, restarts `llamaman.service`. Instances (separate units) keep serving throughout.

### 3.9 Zero-config operation
- **No hand-edited configuration, ever.** There is no config file, no `.env`, and no environment variable a user must set. Every setting lives in the SQLite database and is changed through the web UI (or the first-run wizard). The acceptance test: download → start → open browser → done.
- The daemon boots on built-in defaults. If the default UI port (5526) is occupied, it walks forward to the next free port; the installer and `llamaman status` print the actual URL, and it's logged to journald.
- The systemd units are generated and owned by the installer/app. Anything that requires a unit change (service identity, cache relocation) is performed through the UI or installer flags, which regenerate the units — users never open an editor.
- Environment variables like `HF_HOME` are *respected for initial detection* if present, but never required; the resolved path is persisted in the database and editable in the UI thereafter.
- Because there's no config file to fall back on, recovery paths are explicit CLI commands on the host: `llamaman reset-password` (root/service user only) and `llamaman status`.

## 4. Security model
- Trusted-LAN posture: management UI and gateway over plain HTTP; user brings their own TLS proxy if desired.
- Admin password hashed with argon2id; login rate-limited with lockout; sessions are HttpOnly cookies; CSRF protection on the management API.
- API tokens hashed at rest; HF token stored root/llamaman-readable only (0600).
- llama-server never listens beyond localhost; the service user owns only `/var/lib/llamaman`.

## 5. Assumptions (veto window — will proceed with these unless you object)
1. **Paths**: models in the standard HF cache (`$HF_HOME`, default `~/.cache/huggingface/hub` — auto-detected, overridable in the wizard); llamaman state in `/var/lib/llamaman/{versions,db}`; binary at `/usr/local/bin/llamaman`.
1b. **Service identity**: because the HF cache lives in a user's home directory, the installer defaults to running `llamaman.service` (and instance units) as the **installing user** rather than a dedicated system user — that's how it sees your existing models on the server. A dedicated `llamaman` user with its own cache path remains an installer option for locked-down hosts.
2. **Management UI port**: default **5526** ("LLAMA" on a phone keypad), configurable. Instance public ports chosen per instance at creation.
3. **SQLite** for all state (pure-Go driver, keeps the single-binary story).
4. **Frontend**: React 18 + TypeScript + Vite; dark-first professional theme with a light mode; no external CDN dependencies (fully self-hosted assets).
5. **Instance supervision via systemd template units** as described in §2 (this is what makes self-update non-disruptive).
6. **Rollback depth**: keep exactly the previous llama.cpp build alongside the active one.
7. **Benchmark/instance exclusivity** guard as in §3.5.
8. **No telemetry** of any kind.
9. Sharded-GGUF and mmproj pairing handled automatically by the downloader (§3.2).
10. Naming: binary/service/user all `llamaman`; product name "Llama Man".

## 6. Deferred to v2
- AMD (ROCm + Vulkan) and Intel / generic Vulkan backends (and their VRAM detection).
- LoRA adapter management.
- TLS/WAN-hardened exposure, TOTP.
- Multi-node management, and evaluation of llama-server's native router mode as an alternate serving strategy.

## 7. Open items
None — all resolved. Go module path: `github.com/jlbyh2o/llamaman`.

## 8. Reference facts (researched 2026-08-28)
- llama.cpp releases: semver stable tags (`v0.3.0` → `nightly-tag.txt` → `b10621`) over `b####` prerelease nightlies (~10/day). `releases/latest` returns the semver tag, not the newest build.
- Linux prebuilts per nightly: CPU (x64/arm64/s390x), Vulkan, ROCm, SYCL, OpenVINO. **No Linux CUDA tarball**; CUDA prebuilts are Windows-only.
- llama-server: `--api-key` (comma-separated multiple) / `--api-key-file`; `/health` unauthenticated; `/metrics` (with `--metrics`), `/props`, `/slots`; native router/multi-model mode (`--models-dir`, `/models/load|unload`) exists as of late 2025.
- Memory fitting: llama.cpp `--fit` (default on, `--fit-target` 1024 MiB/GPU margin) projects per-device memory; disabled when `-ngl`/`--tensor-split` set explicitly. `gguf-parser-go` is the maintained offline estimator reference.
- HF API: `/api/models/{id}` includes a server-computed `gguf` field (architecture, context_length, totals); `/api/models/{id}/tree/main` gives files where LFS true size is `lfs.size`; `resolve/` URLs support Range resume; gated repos: metadata public, files 401/403 with `x-error-code: GatedRepo`.
- Landscape: llamactl (closest overlap), llama-swap, Ollama, LM Studio, Paddler, GPUStack. Differentiators here: llama.cpp install/update/version lifecycle, benchmark history, fit calculator tied to actual load flags, native no-Docker system integration.
