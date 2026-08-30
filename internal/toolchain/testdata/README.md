# internal/toolchain/testdata

Captured output of the tools this package probes. Every parser here is tested
against what a real tool actually printed, because the whole risk in a version
probe is that the banner is not the shape someone remembered.

## probe/

| file | how it was obtained |
|---|---|
| `gcc-fedora.txt`, `gxx-fedora.txt` | `gcc --version` / `g++ --version`, Fedora, GCC 16.2.1 |
| `gcc-ubuntu-11.txt` | transcribed from an Ubuntu 22.04 host's `gcc --version` — the vendor-prefixed banner is the shape that breaks a naive parser |
| `cmake-4.3.txt` | `cmake --version`, CMake 4.3.0 |
| `cmake-3.10.txt` | transcribed from an Ubuntu 18.04 host — the case that must FAIL the 3.14 minimum DESIGN section 6.5 sets |
| `ninja-1.13.txt` | `ninja --version` (a bare version, no banner) |
| `make-4.4.txt` | `make --version` |
| `git-2.55.txt` | `git --version` |
| `ccache-4.10.txt` | transcribed from ccache 4.10.2 |
| `nvcc-12.6.txt` | transcribed from CUDA 12.6's `nvcc --version` |
| `nvcc-11.5-no-vfield.txt` | transcribed from CUDA 11.5, which prints `release 11.5` with no `V…` field — the fallback branch |
| `getconf-glibc-2.43.txt` | `getconf GNU_LIBC_VERSION` |
| `ldd-glibc-2.43.txt` | `ldd --version` on glibc |
| `ldd-musl-1.2.5.txt` | transcribed from Alpine's `ldd --version`, which exits 1 while printing what we need |
| `nvidia-smi-single.txt` | `nvidia-smi --query-gpu=driver_version,compute_cap --format=csv,noheader` on a single-GPU host |
| `nvidia-smi-dual.txt` | the same query with two cards of different generations, so D21's architecture list has two entries |
| `nvidia-smi-not-supported.txt` | a driver too old to report `compute_cap`, which prints `[Not Supported]` |

Files marked "transcribed" reproduce output this development host cannot
produce (no CUDA toolkit, no musl, no old cmake). They are copied from the
tools' published output formats, not invented.

## osrelease/

`/etc/os-release` files for the distro-family detection table: an `ID_LIKE`
resolution (`ubuntu.txt`), a direct `ID` hit (`cachyos.txt`, `alpine.txt`), and
a distro with no package name this project can name (`nixos.txt`), which must
resolve to `unknown` and produce the every-distro fallback note rather than a
guess.

Nothing here contains a hostname, a user name, or any other host-identifying
detail.
