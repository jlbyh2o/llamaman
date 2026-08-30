package hw

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The non-GPU half of DESIGN section 8.6: "RAM comes from /proc/meminfo
// (MemTotal, MemAvailable), CPU from /proc/cpuinfo, disk from unix.Statfs — all
// pure Go."
//
// Each reader takes its path so a test can point it at a fixture; the empty
// string is the real one.

// ProcMeminfo and ProcCPUinfo are the default sources.
const (
	ProcMeminfo = "/proc/meminfo"
	ProcCPUinfo = "/proc/cpuinfo"
)

// Memory is what /proc/meminfo says.
//
// AvailableBytes is `MemAvailable`, not `MemFree`, and the difference is the
// whole point: MemFree excludes reclaimable page cache, so on a host that has
// read a 40 GB model once it is a small number that would make every partial
// offload look impossible. MemAvailable is the kernel's own estimate of what a
// new allocation can actually have, which is the question section 8.7 asks.
type Memory struct {
	TotalBytes     uint64
	AvailableBytes uint64
	FreeBytes      uint64
	SwapTotalBytes uint64
	SwapFreeBytes  uint64
	// Known distinguishes "nothing available" from "not measured", for the same
	// reason GPU.VRAMKnown does.
	Known bool
}

// Meminfo reads memory facts. path empty reads ProcMeminfo.
func Meminfo(path string) (Memory, error) {
	if path == "" {
		path = ProcMeminfo
	}
	f, err := os.Open(path)
	if err != nil {
		return Memory{}, fmt.Errorf("hw: open %s: %w", path, err)
	}
	defer f.Close()

	var m Memory
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		// Every value in this file is in kB, whatever the field. The suffix is
		// dropped and the multiplication happens once, here.
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.TotalBytes = kb * 1024
		case "MemAvailable":
			m.AvailableBytes = kb * 1024
		case "MemFree":
			m.FreeBytes = kb * 1024
		case "SwapTotal":
			m.SwapTotalBytes = kb * 1024
		case "SwapFree":
			m.SwapFreeBytes = kb * 1024
		}
	}
	if err := sc.Err(); err != nil {
		return Memory{}, fmt.Errorf("hw: read %s: %w", path, err)
	}
	if m.TotalBytes == 0 {
		return Memory{}, fmt.Errorf("hw: %s has no MemTotal line", path)
	}
	if m.AvailableBytes == 0 {
		// Kernels older than 3.14 have no MemAvailable. MemFree is the honest
		// fallback and it is conservative — it under-reports what is really
		// available, so a fit verdict built on it never over-promises.
		m.AvailableBytes = m.FreeBytes
	}
	m.Known = true
	return m, nil
}

// CPU is the processor summary the system page shows.
type CPU struct {
	Model string
	// Cores is distinct physical cores; Threads is logical processors. They
	// differ on every SMT host, and `-t` wants the first.
	Cores   int
	Threads int
	// Flags is the instruction-set list, which is what tells a user whether a
	// prebuilt binary's AVX-512 requirement can be met.
	Flags []string
}

// Cpuinfo reads /proc/cpuinfo. path empty reads ProcCPUinfo.
func Cpuinfo(path string) (CPU, error) {
	if path == "" {
		path = ProcCPUinfo
	}
	f, err := os.Open(path)
	if err != nil {
		return CPU{}, fmt.Errorf("hw: open %s: %w", path, err)
	}
	defer f.Close()

	var c CPU
	// A physical core is a (physical id, core id) pair; counting "cpu cores"
	// alone double-counts on a dual-socket host.
	cores := map[string]struct{}{}
	var physID, coreID string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if physID != "" || coreID != "" {
				cores[physID+"/"+coreID] = struct{}{}
			}
			physID, coreID = "", ""
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val := strings.TrimSpace(rest)
		switch key {
		case "processor":
			c.Threads++
		case "model name":
			if c.Model == "" {
				c.Model = val
			}
		case "physical id":
			physID = val
		case "core id":
			coreID = val
		case "flags", "Features":
			if len(c.Flags) == 0 {
				c.Flags = strings.Fields(val)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return CPU{}, fmt.Errorf("hw: read %s: %w", path, err)
	}
	if physID != "" || coreID != "" {
		cores[physID+"/"+coreID] = struct{}{}
	}
	c.Cores = len(cores)
	if c.Cores == 0 {
		// An architecture whose cpuinfo has no topology fields (arm64 prints
		// none) reports its logical count rather than zero.
		c.Cores = c.Threads
	}
	return c, nil
}

// HasFlag reports whether the CPU advertises an instruction-set flag.
func (c CPU) HasFlag(name string) bool {
	for _, f := range c.Flags {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

// Disk is one filesystem's capacity.
type Disk struct {
	TotalBytes uint64
	// FreeBytes is Bavail × Bsize — what an UNPRIVILEGED writer can have, not
	// Bfree, because the blocks reserved for root are not blocks this daemon can
	// use and counting them makes a download fail at 98%.
	FreeBytes uint64
	UsedBytes uint64
}

// DiskUsage stats the filesystem holding path.
func DiskUsage(path string) (Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Disk{}, fmt.Errorf("hw: statfs %s: %w", path, err)
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs
	free := st.Bavail * bs
	used := total - st.Bfree*bs
	return Disk{TotalBytes: total, FreeBytes: free, UsedBytes: used}, nil
}

// FreeDiskBytes is DiskUsage's free figure alone, which is what the download
// planner and the build preflight ask for.
func FreeDiskBytes(path string) (uint64, error) {
	d, err := DiskUsage(path)
	return d.FreeBytes, err
}
