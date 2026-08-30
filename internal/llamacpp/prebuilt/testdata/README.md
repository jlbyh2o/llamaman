# internal/llamacpp/prebuilt/testdata

## elf/

Checked-in ELF binaries for the D18 glibc diagnosis (DESIGN section 15:
"`.gnu.version_r` parsing against checked-in ELF fixtures, asserting the
'requires GLIBC_2.38, host has 2.36' message").

They are real linker output, not hand-assembled bytes. Each was produced by
linking a `_start`-only object against a **stub** `libc.so.6` built with a
version script, which is what lets a fixture require a specific `GLIBC_*`
version regardless of what the machine that built it actually has:

```sh
# the stub library: two symbols in two version nodes
cat > stub.c <<'EOF'
int lm_stub_old(void) { return 0; }
int lm_stub_new(void) { return 1; }
EOF
cat > libc.map <<'EOF'
GLIBC_2.2.5 { global: lm_stub_old; };
GLIBC_2.38  { global: lm_stub_new; } GLIBC_2.2.5;
EOF
gcc -shared -fPIC -nostdlib -Wl,--version-script=libc.map \
    -Wl,-soname,libc.so.6 -o libc.so.6 stub.c

# a binary that needs the 2.38 node
cat > new.c <<'EOF'
extern int lm_stub_new(void);
void _start(void) { lm_stub_new(); }
EOF
gcc -nostdlib -no-pie new.c -L. -l:libc.so.6 \
    -Wl,--dynamic-linker=/lib64/ld-linux-x86-64.so.2 -o needs-glibc-2.38
strip needs-glibc-2.38
```

| file | what it exercises |
|---|---|
| `needs-glibc-2.38` | the headline case: `.gnu.version_r` says `libc.so.6 GLIBC_2.38` |
| `needs-glibc-2.2.5` | a binary that runs anywhere — the diagnosis must NOT blame glibc |
| `needs-glibc-and-libstdcxx` | two libraries, and a `CXXABI_*` version that must not be parsed as a glibc requirement |
| `needs-musl-loader` | the same binary with `PT_INTERP` set to `/lib/ld-musl-x86_64.so.1`, the Alpine case where the loader itself is absent |
| `wrong-arch-aarch64` | a copy of `needs-glibc-2.2.5` with `e_machine` patched to `EM_AARCH64` (bytes 18–19 → 183), so an arm64 tarball on an amd64 host is detected |
| `not-an-elf` | plain text: a file that cannot be parsed must produce a diagnosis, never an error that hides the original failure |

## help/

`llama-server --help` captures for the flag parser (section 5.7's
`help_flags_json` and `supports_fit`). `llama-server-help.txt` reproduces
upstream's help format — aliases grouped on one line, value placeholders after
the last alias, two sections. `llama-server-help-no-fit.txt` is the same capture
with the `--fit` line removed, which is a build predating D51's flag and must
set `supports_fit=0`.

## devices/

`llama-server --list-devices` captures for D19: two CUDA devices, a CPU-only
host, and the silent failure the check exists for — a CUDA build whose runtime
init failed and which would therefore serve from the CPU.

Nothing here contains a hostname, a user name, or any other host-identifying
detail. The tar archives the extraction tests use are built in memory by
`extract_test.go`; an evil archive is not something to keep on disk.
