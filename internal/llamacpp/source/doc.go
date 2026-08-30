// Package source builds llama.cpp from source: git clone and worktree
// management, the cmake configure and build steps, installation into the
// versions directory, and streaming of the build log line by line so the UI can
// show it live (DESIGN section 1).
//
// Builder.Build runs DESIGN section 6.5's eight phases in order — preflight,
// space, fetch, configure, compile, install, verify, publish — reporting each
// one through an Observer (which the `llamacpp_install` worker implements over
// `jobs.progress_json`) and writing every line of every child's output to the
// build log. Every failure is a *Failure naming the phase, the code, the
// child's exit status and the first line of the log that explains it, and it
// carries the `llamacpp_versions.state` and `failing_step` the version row must
// move to, so that mapping lives in one place rather than two.
//
// # What this package does not do
//
// It writes no SQL, resolves no channel, and holds no job. The worker resolves
// the version row, reads the settings, probes the GPUs and hands the answers in
// through a Request; this package touches only the filesystem and child
// processes. That is what makes the whole phase machine testable against a
// `cmake` that is a shell script — which is exactly what section 15 asks for.
//
// # The decisions this package IS
//
//   - D20 — parallelism is `min(NumCPU, max(2, MemAvailableGiB/2))`, and a
//     compile the kernel killed is retried ONCE at -j1 with the reason stated.
//     An OOM-killed compile is the most common CUDA build failure on a
//     workstation that is also serving models, and the retry turns it from a
//     hard error into a slow success.
//   - D21 — CMAKE_CUDA_ARCHITECTURES comes from the compute capabilities
//     actually detected. `native` and `all` are refused: the first silently
//     produces a binary that will not run if the GPU set changes, the second
//     multiplies compile time.
//   - D22/D23 — the configure flags. `LLAMA_BUILD_TOOLS=ON` because llama-bench
//     lives under tools/ upstream and is a headline feature; the
//     `$ORIGIN/../lib` install RPATH because it is what makes each
//     `versions/<id>/` relocatable, and relocatable is what makes symlink
//     activation and rollback safe.
//   - D78 — every install lands in `versions/<id>.staging` and is renamed into
//     place. A rebuild of an id that already exists swaps with two renames
//     after re-checking that no live process is executing out of it (D25), so
//     `versions/active` is correct before and after and no directory it can
//     resolve into is ever written in place.
//   - D18/D19 — the binaries are RUN before they are published: `--version`
//     must exit 0 on this host, and a CUDA build must report at least one CUDA
//     device, because a CUDA build that silently fell back to the CPU backend
//     is worse than no build.
//   - D4 — a build directory is kept, and Resume re-runs `cmake --build`
//     against the warm objects. Only a cancellation removes it (Discard), which
//     is why that is a separate call the worker makes deliberately.
package source
