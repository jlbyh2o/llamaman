package toolchain

import (
	"context"
	"strings"
)

// The CUDA half of the probe: `nvcc` (the compiler) and `driver` (the kernel
// driver plus the compute capabilities of the cards present).
//
// The two are separate report entries because they fail separately and the fix
// differs: nvcc missing is a package to install, a driver missing is a kernel
// module and usually a reboot, and a driver present with no CUDA-capable card
// is neither. D19 rejects a CUDA build that lists no CUDA device at verify
// time; this probe is what stops that build from being started at all.
//
// Compute capabilities are read here rather than in internal/hw because this is
// the BUILD question — what `-DCMAKE_CUDA_ARCHITECTURES` should say (D21) —
// answered from a query that returns nothing else. internal/hw owns the GPU
// INVENTORY (uuid, name, VRAM, utilization, per-process attribution) behind its
// Prober interface, and the two do not overlap: nothing here reports memory,
// caches a sample, or is called on a hot path.

// nvidiaSMIQuery is the narrowest query that answers both build questions at
// once: the driver version, and one compute capability per card.
var nvidiaSMIQuery = []string{"--query-gpu=driver_version,compute_cap", "--format=csv,noheader"}

// Driver is what the NVIDIA driver reports about itself and the cards present.
type Driver struct {
	// Present reports whether nvidia-smi was found AND ran.
	Present bool
	// Version is the driver version verbatim ("610.57.04"). It is a STRING and
	// not a parsed Version because NVIDIA zero-pads its components — parsing
	// and re-rendering 610.57.04 produces 610.57.4, a version that host does
	// not have and that no support thread will match. Nothing in this design
	// compares driver versions; it only shows them.
	Version string
	// ComputeCaps is one entry per card, as the driver prints it ("8.9").
	ComputeCaps []string
}

// Architectures renders ComputeCaps in the form CMAKE_CUDA_ARCHITECTURES wants
// — "8.9" becomes "89" — deduplicated and sorted, which is D21's detected list.
// A host with two identical cards therefore compiles one architecture, not two.
func (d Driver) Architectures() []string {
	var out []string
	for _, cc := range d.ComputeCaps {
		if a := ArchFromComputeCap(cc); a != "" {
			out = append(out, a)
		}
	}
	return sortedUnique(out)
}

// ArchFromComputeCap turns a driver-reported compute capability into a
// CMAKE_CUDA_ARCHITECTURES entry: "8.9" → "89", "12.0" → "120". It returns
// empty for anything it cannot read, because a malformed architecture passed to
// nvcc is the `nvcc fatal: Unsupported gpu architecture` failure section 6.5
// gives an actionable hint for — better to detect nothing and let the user set
// the list in Settings → Builds than to pass through garbage.
func ArchFromComputeCap(cc string) string {
	cc = strings.TrimSpace(cc)
	if cc == "" {
		return ""
	}
	v, ok := ParseVersion(cc)
	if !ok || len(v.Parts) < 2 {
		return ""
	}
	major, minor := v.Parts[0], v.Parts[1]
	if major <= 0 || minor < 0 || minor > 9 {
		return ""
	}
	return itoa(major) + itoa(minor)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func probeDriver(ctx context.Context, opts Options, fam Family) (Driver, Tool) {
	t := Tool{Name: ToolDriver, CUDAOnly: true}
	if g, okg := GuidanceFor(ToolDriver); okg {
		t.DocsURL = g.DocsURL
	}

	path, err := opts.lookPath()("nvidia-smi")
	if err != nil {
		t.Note = "nvidia-smi not found on PATH — " + noteFor(ToolDriver, fam, true)
		return Driver{}, t
	}
	t.Found = true
	t.Path = path

	cctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	out, code, runErr := opts.run()(cctx, path, nvidiaSMIQuery...)
	if runErr != nil {
		t.Note = "nvidia-smi found at " + path + " but could not be run: " + runErr.Error()
		return Driver{}, t
	}
	if code != 0 {
		t.Note = "nvidia-smi exited non-zero — " + firstLine(out) +
			"; the driver is installed but not usable, which usually means the kernel module " +
			"does not match the userspace driver (a reboot after an update)"
		return Driver{}, t
	}

	d := ParseNvidiaSMI(out)
	if !d.Present {
		t.Note = "nvidia-smi ran but reported no GPU; a CUDA build would fail its D19 acceptance check"
		return d, t
	}
	t.OK = true
	t.Version = d.Version
	if len(d.Architectures()) > 0 {
		t.Note = "compute capability " + strings.Join(d.ComputeCaps, ", ") +
			" → CMAKE_CUDA_ARCHITECTURES " + strings.Join(d.Architectures(), ";")
	} else {
		t.Note = "driver present but no compute capability reported; set the architecture list in Settings → Builds"
	}
	return d, t
}

// ParseNvidiaSMI reads `nvidia-smi --query-gpu=driver_version,compute_cap
// --format=csv,noheader` output: one `<driver>, <cc>` line per card.
func ParseNvidiaSMI(out string) Driver {
	var d Driver
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		drv := strings.TrimSpace(fields[0])
		cc := strings.TrimSpace(fields[1])
		// "[Not Supported]" and "[N/A]" are what the driver prints for a card
		// whose capability it will not report; they are not versions.
		if strings.HasPrefix(cc, "[") {
			cc = ""
		}
		if !d.Present {
			d.Version = drv
			d.Present = true
		}
		if cc != "" {
			d.ComputeCaps = append(d.ComputeCaps, cc)
		}
	}
	return d
}

// ParseNvccVersion reads `nvcc --version`, whose last two informative lines are
//
//	Cuda compilation tools, release 12.6, V12.6.85
//	Build cuda_12.6.r12.6/compiler.35059454_0
//
// The `V12.6.85` form is preferred because it carries the patch level; the
// `release 12.6` form is the fallback for older toolkits that print only it.
func ParseNvccVersion(out string) (Version, bool) {
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, ", V")
		if idx < 0 {
			continue
		}
		if v, ok := ParseVersion(line[idx+3:]); ok {
			return v, true
		}
	}
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "release ")
		if idx < 0 {
			continue
		}
		if v, ok := ParseVersion(line[idx+len("release "):]); ok {
			return v, true
		}
	}
	return Version{}, false
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
