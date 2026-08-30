# internal/hw/testdata

Captured and transcribed output for the two parsers in this package. Every
parser here is tested against what a real tool actually printed, because the
whole risk in a hardware probe is not the arithmetic — it is the shape of the
line, and the placeholder a driver prints when it will not answer.

## nvidia-smi/

All files are the output of the two queries DESIGN section 8.6 pins, under
`--format=csv,noheader,nounits`. **Every memory column in them is MiB**, which
is the fact the conversion test exists to protect.

| file | what it is |
|---|---|
| `query-gpu-single.txt` | one 24 GiB card. `memory.total` is 24576 MiB, and the parser must turn that into 25769803776 bytes — the exact assertion section 8.6 asks for |
| `query-gpu-dual.txt` | two cards of different generations, so the per-GPU verdict of section 8.7 has an asymmetric pair to work on and `tensor_split` has two entries to index |
| `query-gpu-not-supported.txt` | a driver too old to report `compute_cap`, which prints `[Not Supported]`, on a card with no power sensor, which prints `[N/A]`. Neither is a value and neither may become a zero |
| `compute-apps-dual.txt` | `pid,gpu_uuid,used_gpu_memory` with one process on two GPUs and a second process sharing the first — the join D17 needs |
| `compute-apps-none.txt` | what the driver prints when nothing holds VRAM |
| `compute-apps-nouuid.txt` | the two-column form the per-GPU `-i` fallback loop reads, whose identity comes from the loop variable |
| `field-not-valid.txt` | the message from a driver whose `nvidia-smi` rejects `gpu_uuid`, which is what arms that fallback |
| `driver-mismatch.txt` | the most common total failure: a kernel module that does not match the userspace driver. Every GPU must go `unknown`, never zero |

## proc/

| file | what it is |
|---|---|
| `meminfo.txt` | `/proc/meminfo` from a 64 GiB host, abridged to the lines this package reads |
| `meminfo-no-available.txt` | the same without `MemAvailable`, as kernels before 3.14 print it — the branch that falls back to `MemFree` |
| `cpuinfo.txt` | a four-thread, two-core `/proc/cpuinfo`, so the `(physical id, core id)` de-duplication has something to de-duplicate |

The GPU UUIDs, the pids and the model name are invented. Nothing here contains
a hostname, a user name, or any other host-identifying detail.
