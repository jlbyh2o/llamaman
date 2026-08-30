package source

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// GiB is one gibibyte, the unit D20's formula is stated in.
const GiB = 1 << 30

// BuildJobs is D20's parallelism, exactly as the decision states it:
//
//	N = min(NumCPU, max(2, MemAvailableGiB/2))
//
// The memory term is what makes it a rule rather than a preference: `nvcc` and
// `cc1plus` on ggml-cuda translation units routinely peak above 2 GiB, so a
// 32-thread host with 8 GiB available that runs `-j32` does not build slowly —
// it gets a compiler OOM-killed, which is the most common CUDA build failure on
// a workstation that is also serving models.
//
// The floor of 2 is deliberate and is NOT clamped away by a small memory
// figure: a host with 1 GiB available still tries two compilers, and D20's
// automatic retry at -j1 is what catches it if that was too many. The only
// thing that lowers the answer below 2 is a host with one CPU.
func BuildJobs(numCPU int, memAvailable uint64) int {
	if numCPU < 1 {
		numCPU = 1
	}
	n := int(memAvailable / GiB / 2)
	if n < 2 {
		n = 2
	}
	if n > numCPU {
		n = numCPU
	}
	return n
}

// MemAvailableBytes reads MemAvailable from a /proc/meminfo-shaped file.
//
// MemAvailable rather than MemFree is the whole point: MemFree on a host that
// has been serving models for a week is nearly zero — the page cache holds the
// GGUFs — and sizing a build from it would pin every host to -j2 forever.
func MemAvailableBytes(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("source: read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		key, rest, ok := strings.Cut(line, ":")
		if !ok || key != "MemAvailable" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, fmt.Errorf("source: %s: MemAvailable has no value", path)
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("source: %s: MemAvailable %q: %w", path, fields[0], err)
		}
		// The kernel reports kB, meaning KiB.
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("source: read %s: %w", path, err)
	}
	return 0, fmt.Errorf("source: %s has no MemAvailable line", path)
}
