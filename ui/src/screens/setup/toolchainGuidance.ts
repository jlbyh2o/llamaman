/**
 * What to tell a user about a build tool they do not have.
 *
 * This is the client-side twin of `internal/toolchain`'s guidance table, and it exists because the
 * wire only carries the *names*: `GET /api/v1/llamacpp/plan` answers `missing_tools: ["nvcc"]`
 * (section 3.5, section 6.3), a closed vocabulary rather than incidental strings, and section 11.2
 * asks the wizard to render "per-tool found/version/needed and distro-doc links" around it.
 *
 * The package names below are copied from `internal/toolchain/guidance.go` and must move with it.
 * They are only ever *shown*: nothing here is executed, and — as section 6.5 requires — no line of
 * this file suggests a command to run as root. A user is told which package carries the tool and
 * where its documentation is; how their distribution installs a package is theirs to know.
 */

/** The distro families `internal/toolchain` has package names for, in its own order. */
export const DISTRO_FAMILIES = [
  { id: 'debian', label: 'Debian · Ubuntu · Mint' },
  { id: 'fedora', label: 'Fedora · RHEL · Rocky' },
  { id: 'arch', label: 'Arch · CachyOS · Manjaro' },
  { id: 'suse', label: 'openSUSE · SLES' },
  { id: 'alpine', label: 'Alpine' },
  { id: 'gentoo', label: 'Gentoo' },
] as const;

export type DistroFamily = (typeof DISTRO_FAMILIES)[number]['id'];

export interface ToolGuidance {
  /** The `missing_tools` string. */
  name: string;
  /** How the tool is named to a person. */
  label: string;
  /** Why a build needs it, in one clause. */
  purpose: string;
  /** Package name per family. A family absent from the map has no single package that carries it. */
  packages: Partial<Record<DistroFamily, string>>;
  docsUrl: string;
  /** Its absence never blocks a build, so it never appears in `missing_tools`. */
  optional?: boolean;
  /** A CPU build does not need it. */
  cudaOnly?: boolean;
}

/** The probe set of section 6.5, in the order the wizard reads best. */
export const TOOLCHAIN_TOOLS: readonly ToolGuidance[] = [
  {
    name: 'gcc',
    label: 'gcc',
    purpose: 'The C compiler every backend is built with.',
    packages: {
      debian: 'build-essential',
      fedora: 'gcc',
      arch: 'base-devel',
      suse: 'gcc',
      alpine: 'build-base',
      gentoo: 'sys-devel/gcc',
    },
    docsUrl: 'https://gcc.gnu.org/install/',
  },
  {
    name: 'g++',
    label: 'g++',
    purpose: 'llama.cpp is C++; a C compiler alone is not enough.',
    packages: {
      debian: 'build-essential',
      fedora: 'gcc-c++',
      arch: 'base-devel',
      suse: 'gcc-c++',
      alpine: 'build-base',
      gentoo: 'sys-devel/gcc',
    },
    docsUrl: 'https://gcc.gnu.org/install/',
  },
  {
    name: 'cmake',
    label: 'cmake',
    purpose: 'Configures the build. Version 3.14 or newer.',
    packages: {
      debian: 'cmake',
      fedora: 'cmake',
      arch: 'cmake',
      suse: 'cmake',
      alpine: 'cmake',
      gentoo: 'dev-build/cmake',
    },
    docsUrl: 'https://cmake.org/download/',
  },
  {
    name: 'make',
    label: 'make',
    purpose: 'The fallback build driver when ninja is not installed.',
    packages: {
      debian: 'build-essential',
      fedora: 'make',
      arch: 'base-devel',
      suse: 'make',
      alpine: 'make',
      gentoo: 'sys-devel/make',
    },
    docsUrl: 'https://www.gnu.org/software/make/',
  },
  {
    name: 'git',
    label: 'git',
    purpose: 'Fetches the llama.cpp sources for a source build.',
    packages: {
      debian: 'git',
      fedora: 'git',
      arch: 'git',
      suse: 'git',
      alpine: 'git',
      gentoo: 'dev-vcs/git',
    },
    docsUrl: 'https://git-scm.com/downloads',
  },
  {
    name: 'nvcc',
    label: 'CUDA toolkit (nvcc)',
    purpose: 'Needed only for a CUDA build; a CPU build does not use it.',
    packages: {
      debian: 'nvidia-cuda-toolkit',
      fedora: 'cuda-toolkit',
      arch: 'cuda',
      suse: 'cuda-toolkit',
    },
    docsUrl: 'https://docs.nvidia.com/cuda/cuda-installation-guide-linux/',
    cudaOnly: true,
  },
  {
    name: 'driver',
    label: 'NVIDIA driver',
    purpose: 'The kernel driver ships nvidia-smi; a CUDA build needs both it and a GPU.',
    packages: {},
    docsUrl: 'https://www.nvidia.com/download/index.aspx',
    cudaOnly: true,
  },
  {
    name: 'ninja',
    label: 'ninja',
    purpose: 'Optional: without it the build falls back to Unix Makefiles — slower, but correct.',
    packages: {
      debian: 'ninja-build',
      fedora: 'ninja-build',
      arch: 'ninja',
      suse: 'ninja',
      alpine: 'samurai',
      gentoo: 'dev-build/ninja',
    },
    docsUrl: 'https://ninja-build.org/',
    optional: true,
  },
  {
    name: 'ccache',
    label: 'ccache',
    purpose: 'Optional: it makes a rebuild of a nearby commit minutes rather than an hour.',
    packages: {
      debian: 'ccache',
      fedora: 'ccache',
      arch: 'ccache',
      suse: 'ccache',
      alpine: 'ccache',
      gentoo: 'dev-util/ccache',
    },
    docsUrl: 'https://ccache.dev/',
    optional: true,
  },
];

/**
 * What the plan pair says about one tool.
 *
 * `missing_tools` is populated only for a *source* plan (`internal/llamacpp/plan.go`: "a prebuilt
 * needs no compiler, so its missing-tools list is empty by construction"), which is why the wizard
 * asks for both plans with `force_source=1`. Two lists then separate three cases:
 *
 *  - `blocking`  — missing from the CPU list too: nothing will build from source without it.
 *  - `cuda-only` — missing only from the CUDA list: a CPU build is unaffected.
 *  - `present`   — the probe found it, and found it good enough.
 *
 * An optional tool never appears in either list, so its answer is `unreported` rather than a claim
 * this endpoint cannot support.
 */
export type ToolVerdict = 'present' | 'blocking' | 'cuda-only' | 'unreported';

export function toolVerdict(
  tool: ToolGuidance,
  missingForCPU: readonly string[] | undefined,
  missingForCUDA: readonly string[] | undefined,
): ToolVerdict {
  if (tool.optional) return 'unreported';
  if (missingForCPU?.includes(tool.name)) return 'blocking';
  if (missingForCUDA?.includes(tool.name)) return 'cuda-only';
  if (missingForCPU === undefined && missingForCUDA === undefined) return 'unreported';
  // A CUDA-only tool is absent from the CPU list by construction, so "not missing" only means
  // something once the CUDA plan has answered.
  if (tool.cudaOnly && missingForCUDA === undefined) return 'unreported';
  return 'present';
}

/** The package name to show for a family, or every family's when none is chosen. */
export function packageHint(tool: ToolGuidance, family: DistroFamily | 'all'): string | null {
  if (Object.keys(tool.packages).length === 0) return null;
  if (family !== 'all') {
    const name = tool.packages[family];
    return name ? `Package: ${name}` : 'No single package carries this on that distribution.';
  }
  const parts = DISTRO_FAMILIES.flatMap((entry) => {
    const name = tool.packages[entry.id];
    return name ? [`${entry.id}: ${name}`] : [];
  });
  return parts.length ? `Package by distribution — ${parts.join(', ')}` : null;
}
