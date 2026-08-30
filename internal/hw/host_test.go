package hw

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMeminfo pins the two facts DESIGN section 8.7 reads: MemAvailable is the
// free-RAM figure, and the kB→bytes conversion happens once.
func TestMeminfo(t *testing.T) {
	cases := []struct {
		name          string
		file          string
		wantTotal     uint64
		wantAvailable uint64
	}{
		{
			name: "MemAvailable is preferred over MemFree",
			file: "proc/meminfo.txt",
			// 65704448 kB × 1024 and 49807360 kB × 1024.
			wantTotal:     67281354752,
			wantAvailable: 51002736640,
		},
		{
			name: "a pre-3.14 kernel falls back to MemFree",
			file: "proc/meminfo-no-available.txt",
			// 4046336 kB × 1024 and 812032 kB × 1024.
			wantTotal:     4143448064,
			wantAvailable: 831520768,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Meminfo(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("Meminfo: %v", err)
			}
			if !m.Known {
				t.Error("a parsed meminfo must be Known")
			}
			if m.TotalBytes != tc.wantTotal {
				t.Errorf("total = %d, want %d", m.TotalBytes, tc.wantTotal)
			}
			if m.AvailableBytes != tc.wantAvailable {
				t.Errorf("available = %d, want %d", m.AvailableBytes, tc.wantAvailable)
			}
		})
	}
}

// TestMeminfoMissingFileIsAnError, and specifically not a zero Memory with
// Known set: a fit verdict built on a fabricated 0 bytes free is exactly the
// failure F14 exists to prevent.
func TestMeminfoMissingFileIsAnError(t *testing.T) {
	m, err := Meminfo(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("want an error for a missing meminfo")
	}
	if m.Known {
		t.Error("a failed read must not report Known")
	}
}

// TestCpuinfo: physical cores are the (physical id, core id) pairs, not the
// `cpu cores` line and not the processor count, which double-counts on SMT.
func TestCpuinfo(t *testing.T) {
	c, err := Cpuinfo(filepath.Join("testdata", "proc/cpuinfo.txt"))
	if err != nil {
		t.Fatalf("Cpuinfo: %v", err)
	}
	if c.Threads != 4 {
		t.Errorf("threads = %d, want 4", c.Threads)
	}
	if c.Cores != 2 {
		t.Errorf("cores = %d, want 2 distinct (physical id, core id) pairs", c.Cores)
	}
	if c.Model == "" {
		t.Error("model name should be carried through")
	}
	if !c.HasFlag("AVX2") {
		t.Error("HasFlag should be case-insensitive")
	}
	if c.HasFlag("amx_tile") {
		t.Error("HasFlag must not invent flags")
	}
}

// TestCpuinfoWithoutTopology: an architecture whose cpuinfo prints no
// `physical id` reports its logical count rather than zero cores.
func TestCpuinfoWithoutTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpuinfo")
	body := "processor\t: 0\nmodel name\t: Cortex-A76\n\nprocessor\t: 1\nmodel name\t: Cortex-A76\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Cpuinfo(path)
	if err != nil {
		t.Fatalf("Cpuinfo: %v", err)
	}
	if c.Cores != 2 || c.Threads != 2 {
		t.Errorf("cores/threads = %d/%d, want 2/2", c.Cores, c.Threads)
	}
}

// TestDiskUsage runs against a real directory, because Statfs is the one thing
// here with no fixture: what it must never do is report zero for a filesystem
// that plainly has space.
func TestDiskUsage(t *testing.T) {
	d, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	if d.TotalBytes == 0 {
		t.Fatal("total bytes is 0 for a mounted filesystem")
	}
	if d.FreeBytes > d.TotalBytes {
		t.Errorf("free %d exceeds total %d", d.FreeBytes, d.TotalBytes)
	}
	free, err := FreeDiskBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeDiskBytes: %v", err)
	}
	if free == 0 {
		t.Error("free bytes is 0 for a mounted filesystem")
	}
}
