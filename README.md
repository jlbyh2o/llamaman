# Llama Man

Web-based management for [llama.cpp](https://github.com/ggml-org/llama.cpp) on Linux — install, update, configure, serve, and benchmark, all from the browser.

> **Status: pre-alpha.** Llama Man is in active development and is not yet installable. Watch the repo for the first release.

## What it does (v1 scope)

Llama Man is a single-binary system service with a web UI for operating llama.cpp on a dedicated machine:

- **llama.cpp lifecycle** — install and update llama.cpp from the web UI: stable and nightly release channels, or build any fork, branch, or commit from source. CUDA builds are compiled on-host (no Linux CUDA prebuilts exist upstream), with one-click rollback to the previous build.
- **Model management** — browse and search Hugging Face with or without a token, read model cards, see every quantization of a model with true file sizes, and download with resume support. Models live in the standard Hugging Face cache, so anything you've already downloaded with other tools is detected and ready to use. Per-model disk usage is always visible, and models can be deleted from disk in the UI.
- **Fit calculator** — per-quantization verdicts (fits in VRAM / partial offload into system RAM / won't run) computed from GGUF metadata, your GPUs' actual VRAM, the chosen context length, and KV-cache type — before you download anything.
- **Instances** — run multiple named llama-server processes, each with first-class access to every load flag (context size, GPU offload, flash attention, KV-cache quantization, parallel slots, speculative-decoding draft models, multimodal projectors, and more), autostart on boot via systemd, and live status, memory use, and logs.
- **API gateway** — issue and revoke inference API tokens instantly without reloading a model, or disable auth per instance entirely. llama-server itself never listens beyond localhost.
- **Benchmarking** — orchestrated llama-bench sweeps (GPU-only versus partial offload, batch sizes, cache types) with persistent history, side-by-side comparisons across models, settings, and llama.cpp versions, and JSON/CSV/markdown export.
- **Zero configuration** — no config files and no environment variables, ever. Download it, start it, and do everything — including first-run setup — in the browser.

**What it deliberately is not:** a chat UI. Llama Man manages what's loaded and serving; inference belongs to your own clients via the OpenAI-compatible API.

## Requirements (v1)

- Linux with systemd
- NVIDIA GPU (CUDA, with a build toolchain for on-host compiles) or CPU-only
- AMD and Intel GPU support is planned for v2

## Design

The full v1 specification lives in [SPEC.md](SPEC.md).

## License

[MIT](LICENSE)
