# internal/supervisor/testdata

## journal/

llama-server's own startup output, as the journal carries it. These are the
input to `ParseFitReport` (DESIGN section 5.8's fit observation), and they are
files rather than string literals for the same reason internal/toolchain's
fixtures are: the risk in this parser is never the arithmetic, it is the shape
of a line another project prints and is free to change.

| file | what it is |
|---|---|
| `load-cuda.txt` | a full CUDA load: device and host model buffers, a KV buffer, device and host compute buffers, and llama.cpp's own `--fit` projection — D33's ground truth |
| `load-dual-gpu.txt` | the same across two devices, so the per-device sums have something to sum |
| `load-oom.txt` | a load that died in `cudaMalloc`. Section 8.7's golden rule is built on rows like this one |
| `load-cpu.txt` | a CPU-only load, where every buffer is a host buffer and the device totals must stay zero |
| `no-buffer-lines.txt` | a journal window that reached the ready line without any buffer report, which must produce no observation at all rather than a row of zeros |

Every byte figure is written to a whole MiB so the expected values in the test
are exact integers rather than rounded floats. The paths, ports and pids are
invented; nothing here identifies a host.
