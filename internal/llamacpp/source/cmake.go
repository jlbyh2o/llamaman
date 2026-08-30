package source

import (
	"strconv"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// InstallRPATH is D22 in one string. `$ORIGIN/../lib` is written literally —
// there is no shell anywhere in this pipeline, so nothing expands it before
// cmake sees it, and cmake is the one that must.
//
// Paired with CMAKE_BUILD_WITH_INSTALL_RPATH=ON below, it is what makes every
// `versions/<id>/` self-contained and relocatable: without the pairing, cmake
// strips the build RPATH at install time and the installed `llama-server`
// cannot find libggml*.so in its own tree. That property is what makes symlink
// activation and rollback safe, and it is why the launcher sets no
// LD_LIBRARY_PATH at all.
const InstallRPATH = "$ORIGIN/../lib"

// ConfigureOptions is everything the configure command line needs. It is a
// struct rather than eight parameters so the golden test can state a whole
// configuration in one literal.
type ConfigureOptions struct {
	// SourceDir is the git worktree (`-S`).
	SourceDir string
	// BuildDir is the cmake binary directory (`-B`).
	BuildDir string
	// InstallPrefix is `versions/<id>.staging`, NEVER `versions/<id>` (D78).
	InstallPrefix string
	// Generator is "Ninja" or "Unix Makefiles".
	Generator string
	// Backend adds the CUDA block when it is cuda.
	Backend model.Backend
	// CUDAArchs is CMAKE_CUDA_ARCHITECTURES, from detection (D21).
	CUDAArchs []string
	// CCache, when non-empty, sets both compiler launchers.
	CCache string
	// ExtraFlags are the settings' extra flags followed by the request's, last
	// so they can override anything above.
	ExtraFlags []string
}

// ConfigureArgs renders DESIGN section 6.5's configure command line, in the
// order the design writes it.
//
// Every flag here is load-bearing and three of them are decisions:
//
//   - LLAMA_BUILD_TOOLS=ON (D23) — `llama-bench` lives under tools/ upstream, so
//     without it the headline SPEC §3.5 benchmark feature is simply not built.
//   - BUILD_SHARED_LIBS=ON with the install RPATH (D22) — see InstallRPATH.
//   - LLAMA_CURL=OFF — SPEC §3.2 forbids `-hf` model fetching, so libcurl would
//     be a build-time system dependency bought for a feature we never use, and
//     it is a common source of source-build failures on minimal hosts.
//
// GGML_NATIVE=ON is on both backends because the build host is the run host;
// the resulting CPU flag set is recorded so a state directory moved to another
// machine raises "rebuild recommended" rather than an illegal-instruction crash.
func ConfigureArgs(o ConfigureOptions) []string {
	gen := o.Generator
	if gen == "" {
		gen = "Ninja"
	}
	args := []string{
		"-S", o.SourceDir,
		"-B", o.BuildDir,
		"-G", gen,
		"-DCMAKE_BUILD_TYPE=Release",
		"-DCMAKE_INSTALL_PREFIX=" + o.InstallPrefix,
		"-DLLAMA_BUILD_SERVER=ON",
		"-DLLAMA_BUILD_TOOLS=ON",
		"-DLLAMA_BUILD_TESTS=OFF",
		"-DLLAMA_BUILD_EXAMPLES=OFF",
		"-DLLAMA_CURL=OFF",
		"-DBUILD_SHARED_LIBS=ON",
		"-DGGML_NATIVE=ON",
		"-DCMAKE_INSTALL_RPATH=" + InstallRPATH,
		"-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON",
	}
	if o.Backend == model.BackendCUDA {
		args = append(args,
			"-DGGML_CUDA=ON",
			"-DCMAKE_CUDA_ARCHITECTURES="+joinArchs(o.CUDAArchs),
		)
	}
	if o.CCache != "" {
		args = append(args,
			"-DCMAKE_C_COMPILER_LAUNCHER=ccache",
			"-DCMAKE_CXX_COMPILER_LAUNCHER=ccache",
		)
	}
	return append(args, o.ExtraFlags...)
}

// BuildArgs renders the compile step: `cmake --build <dir> -j N` (section 6.5,
// D20).
func BuildArgs(buildDir string, jobs int) []string {
	if jobs < 1 {
		jobs = 1
	}
	return []string{"--build", buildDir, "-j", strconv.Itoa(jobs)}
}

// InstallArgs renders the install step.
//
// The destination is deliberately NOT passed here: `cmake --install --prefix`
// arrived in cmake 3.15 and section 6.5's preflight floor is 3.14, so the prefix
// is set once at configure time (CMAKE_INSTALL_PREFIX above) and read back out
// of the cache. That also means a D4 warm rerun installs where the original
// configure said, which is the staging directory the publish step expects.
func InstallArgs(buildDir string) []string {
	return []string{"--install", buildDir}
}

func joinArchs(archs []string) string {
	out := ""
	for i, a := range archs {
		if i > 0 {
			out += ";"
		}
		out += a
	}
	return out
}
