package hw

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jlbyh2o/llamaman/internal/procx"
)

// The one v1 Prober implementation (DESIGN section 8.6, D16).
//
// NVML through `purego`/`dlopen` is deliberately rejected: hand-bound versioned
// C structs are untestable ABI risk, a CGO_ENABLED=0 binary is statically linked
// with no dynamic loader to service `dlopen`, and the whole saving is one ~30 ms
// fork per sample. `nvidia-smi` is itself an NVML client, so the numbers SPEC
// section 3.2 asks for are the numbers this reports — which is why D16 records
// the substitution as an explicit SPEC amendment rather than leaving it an
// unstated departure.

// The two queries of section 8.6, spelled once.
var (
	queryGPU = []string{
		"--query-gpu=index,uuid,name,memory.total,memory.used,memory.free," +
			"utilization.gpu,temperature.gpu,power.draw,compute_cap,driver_version",
		"--format=csv,noheader,nounits",
	}
	queryComputeApps = []string{
		"--query-compute-apps=pid,gpu_uuid,used_gpu_memory",
		"--format=csv,noheader,nounits",
	}
	// queryComputeAppsNoUUID is the fallback for drivers whose nvidia-smi
	// rejects `gpu_uuid` as an unknown field. It is run once per GPU with `-i`,
	// and the identity comes from the loop variable.
	queryComputeAppsNoUUID = []string{
		"--query-compute-apps=pid,used_gpu_memory",
		"--format=csv,noheader,nounits",
	}
)

// MiB is the unit every memory field nvidia-smi emits, and the conversion to
// bytes happens EXACTLY ONCE — here, in this file's parser.
//
// `--format=…,nounits` strips the suffix but does not change the unit:
// `memory.total`, `memory.used`, `memory.free` and `used_gpu_memory` are all
// MiB. Wiring the parser straight into `gpus.vram_total_bytes`,
// `instance_status.vram_bytes` or section 8.4's `assigned(g, n)` would be a
// factor-of-2²⁰ error that does not crash: it silently reports a 24 GB card as
// having 24 KB free and turns every fit verdict into `wont_run`.
//
// The other columns are left alone and their units are named here too, so the
// same mistake cannot be made twice: `utilization.gpu` is a percentage,
// `temperature.gpu` is degrees Celsius, `power.draw` is watts (a float, and
// `[N/A]` on cards with no sensor), `compute_cap` is a `major.minor` string.
const MiB = 1 << 20

// CacheTTL is the ~2 s sample cache of section 8.6. Every consumer — the SSE
// GPU stream, the fit calculator, the bench exclusivity guard, the supervisor's
// per-instance attribution — reads through it, so a page with four live panels
// forks nvidia-smi once rather than four times a second.
const CacheTTL = 2 * time.Second

// ErrProbeFailed is returned when nvidia-smi could not be run or exited
// non-zero. It is deliberately distinct from "there are no GPUs": a host with no
// NVIDIA driver returns an empty inventory and no error, and a host whose driver
// is broken returns this.
var ErrProbeFailed = errors.New("hw: nvidia-smi probe failed")

// Runner executes nvidia-smi and returns its merged output and exit status. It
// is the seam every test in this package substitutes; the production
// implementation goes through internal/procx, so a hung binary is killed with
// its whole process group when the context ends.
//
// It returns an error only when the process could not be STARTED; a non-zero
// exit is `code`, because the field-rejection fallback below is detected from an
// exit status plus its message.
type Runner func(ctx context.Context, name string, args ...string) (output string, code int, err error)

// Options configures a prober. The zero value probes this host.
type Options struct {
	// Run executes nvidia-smi. Nil uses internal/procx.
	Run Runner
	// LookPath resolves the binary. Nil uses exec.LookPath.
	LookPath func(file string) (string, error)
	// Now supplies the cache clock. Nil uses time.Now.
	Now func() time.Time
	// TTL overrides the sample cache. Zero uses CacheTTL; a negative value
	// disables caching, which is what the tests that count invocations want.
	TTL time.Duration
	// Timeout bounds one invocation. Zero uses 5 s: nvidia-smi prints a table
	// and exits, and one that does not is a wedged driver, not a slow one.
	Timeout time.Duration
}

func (o Options) run() Runner {
	if o.Run != nil {
		return o.Run
	}
	return execRun
}

func (o Options) lookPath() func(string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath
	}
	return exec.LookPath
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) ttl() time.Duration {
	if o.TTL != 0 {
		return o.TTL
	}
	return CacheTTL
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 5 * time.Second
}

func execRun(ctx context.Context, name string, args ...string) (string, int, error) {
	out, res, err := procx.Capture(ctx, procx.Cmd{
		Path: name, Args: args, ExtraEnv: []string{"LC_ALL=C"},
	})
	var ee *procx.ExitError
	if err != nil && !errors.As(err, &ee) {
		return out, -1, err
	}
	return out, res.ExitCode, nil
}

// NvidiaSMIProber is the nvidia-smi provider.
type NvidiaSMIProber struct {
	opts Options

	mu sync.Mutex
	// gpus and apps are the ~2 s sample cache.
	gpus    []GPU
	gpusAt  time.Time
	gpusErr error
	apps    []ComputeApp
	appsAt  time.Time
	appsErr error
	// inventory is the last identity set successfully read: index, uuid, name,
	// compute capability, driver version. It is what a FAILED probe reports with
	// nil VRAM pointers, because D16's promise is that a failure marks GPUs
	// unknown and never zero — and reporting an empty list would be reporting
	// zero GPUs, which is the same lie in a different shape.
	inventory []GPU
	// noUUIDField remembers that this driver's nvidia-smi rejects `gpu_uuid`.
	// Section 8.6 says the detection happens once and is remembered for the
	// process lifetime; re-detecting per sample would fork twice a second
	// forever on the hosts that need the fallback.
	noUUIDField bool
	// path is the resolved binary, looked up once.
	path     string
	pathErr  error
	pathDone bool
}

// NewNvidiaSMIProber builds a prober.
func NewNvidiaSMIProber(opts Options) *NvidiaSMIProber { return &NvidiaSMIProber{opts: opts} }

var _ Prober = (*NvidiaSMIProber)(nil)

// Available reports whether nvidia-smi is on PATH at all. A host without it has
// no NVIDIA GPUs, which is an ordinary state and not an error.
func (p *NvidiaSMIProber) Available() bool {
	_, err := p.binary()
	return err == nil
}

func (p *NvidiaSMIProber) binary() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pathDone {
		p.path, p.pathErr = p.opts.lookPath()("nvidia-smi")
		p.pathDone = true
	}
	return p.path, p.pathErr
}

// Probe returns the current GPU inventory, cached for TTL.
//
// A non-zero exit or an unparsable line marks every GPU unknown and returns nil
// VRAM fields — NEVER zeros (F14) — because a fabricated 0 MiB free would make
// the fit calculator confidently wrong: every verdict would be `wont_run`, with
// nothing to say that no measurement was made.
func (p *NvidiaSMIProber) Probe(ctx context.Context) ([]GPU, error) {
	p.mu.Lock()
	if fresh(p.gpusAt, p.opts) {
		out, err := clone(p.gpus), p.gpusErr
		p.mu.Unlock()
		return out, err
	}
	p.mu.Unlock()

	gpus, err := p.probeOnce(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.gpusAt, p.gpusErr = p.opts.now(), err
	if err != nil {
		p.gpus = unknownFrom(p.inventory)
		return clone(p.gpus), err
	}
	p.gpus = gpus
	p.inventory = identityOf(gpus)
	return clone(gpus), nil
}

func (p *NvidiaSMIProber) probeOnce(ctx context.Context) ([]GPU, error) {
	path, err := p.binary()
	if err != nil {
		// No nvidia-smi at all: no NVIDIA GPUs. An empty list and no error is
		// the honest answer, and it is not the same as a failed probe.
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, p.opts.timeout())
	defer cancel()
	out, code, runErr := p.opts.run()(cctx, path, queryGPU...)
	if runErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrProbeFailed, runErr)
	}
	if code != 0 {
		return nil, fmt.Errorf("%w: exit %d: %s", ErrProbeFailed, code, firstLine(out))
	}
	gpus, perr := ParseGPUs(out)
	if perr != nil {
		return nil, perr
	}
	return gpus, nil
}

// ComputeApps returns the processes holding VRAM, cached for TTL.
//
// The `gpu_uuid` column is NOT optional (D17). Per-process attribution has two
// consumers and the second cannot be built without GPU identity:
// `instance_status.vram_bytes` needs only pid and used memory, but
// `instance_status.gpu_uuids_json` — which the section 10 bench exclusivity
// guard reads — has to know WHICH GPU each row belongs to, and a
// `pid,used_gpu_memory` query returns no such field.
func (p *NvidiaSMIProber) ComputeApps(ctx context.Context) ([]ComputeApp, error) {
	p.mu.Lock()
	if fresh(p.appsAt, p.opts) {
		out, err := cloneApps(p.apps), p.appsErr
		p.mu.Unlock()
		return out, err
	}
	p.mu.Unlock()

	apps, err := p.computeAppsOnce(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.appsAt, p.appsErr = p.opts.now(), err
	if err != nil {
		p.apps = nil
		return nil, err
	}
	p.apps = apps
	return cloneApps(apps), nil
}

func (p *NvidiaSMIProber) computeAppsOnce(ctx context.Context) ([]ComputeApp, error) {
	path, err := p.binary()
	if err != nil {
		return nil, nil
	}

	p.mu.Lock()
	fallback := p.noUUIDField
	p.mu.Unlock()

	if !fallback {
		cctx, cancel := context.WithTimeout(ctx, p.opts.timeout())
		out, code, runErr := p.opts.run()(cctx, path, queryComputeApps...)
		cancel()
		if runErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrProbeFailed, runErr)
		}
		if code == 0 {
			return ParseComputeApps(out, "")
		}
		if !rejectsField(out) {
			return nil, fmt.Errorf("%w: exit %d: %s", ErrProbeFailed, code, firstLine(out))
		}
		// Detected once, remembered for the process lifetime (section 8.6).
		p.mu.Lock()
		p.noUUIDField = true
		p.mu.Unlock()
	}

	return p.computeAppsPerGPU(ctx, path)
}

// computeAppsPerGPU is the section 8.6 fallback: loop `nvidia-smi -i <index>`
// once per GPU and take the identity from the loop variable.
func (p *NvidiaSMIProber) computeAppsPerGPU(ctx context.Context, path string) ([]ComputeApp, error) {
	gpus, err := p.Probe(ctx)
	if err != nil {
		return nil, err
	}
	var out []ComputeApp
	for _, g := range gpus {
		cctx, cancel := context.WithTimeout(ctx, p.opts.timeout())
		args := append([]string{"-i", strconv.Itoa(g.Index)}, queryComputeAppsNoUUID...)
		text, code, runErr := p.opts.run()(cctx, path, args...)
		cancel()
		if runErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrProbeFailed, runErr)
		}
		if code != 0 {
			return nil, fmt.Errorf("%w: exit %d: %s", ErrProbeFailed, code, firstLine(text))
		}
		rows, perr := ParseComputeApps(text, g.UUID)
		if perr != nil {
			return nil, perr
		}
		out = append(out, rows...)
	}
	return out, nil
}

// rejectsField recognizes the driver whose nvidia-smi does not know `gpu_uuid`.
// The message is "Field "gpu_uuid" is not a valid field to query."; matching the
// stable half of it is what keeps the detection from firing on an unrelated
// failure and silently degrading attribution on a healthy host.
func rejectsField(out string) bool {
	l := strings.ToLower(out)
	return strings.Contains(l, "not a valid field")
}

// ParseGPUs reads the `--query-gpu` CSV of section 8.6.
//
// It is exported because the fixtures in testdata/ are the test's whole point:
// every parser here is checked against what a real driver actually printed, on
// the principle that the risk in a probe is never the arithmetic but the shape
// of the line.
func ParseGPUs(out string) ([]GPU, error) {
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := splitCSV(line)
		if len(f) < 11 {
			return nil, fmt.Errorf("%w: unparsable --query-gpu line %q", ErrProbeFailed, line)
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("%w: unparsable GPU index in %q", ErrProbeFailed, line)
		}
		g := GPU{
			Index:          idx,
			UUID:           f[1],
			Name:           f[2],
			VRAMTotalBytes: mib(f[3]),
			VRAMUsedBytes:  mib(f[4]),
			VRAMFreeBytes:  mib(f[5]),
			UtilizationPct: intOr(f[6], 0),
			TemperatureC:   intOr(f[7], 0),
			PowerDrawWatts: floatOr(f[8], 0),
			ComputeCap:     bracketed(f[9]),
			DriverVersion:  bracketed(f[10]),
		}
		gpus = append(gpus, g)
	}
	return gpus, nil
}

// ParseComputeApps reads the `--query-compute-apps` CSV. uuidFallback supplies
// the GPU identity for the two-column form the per-GPU loop uses; it is empty
// for the three-column query.
func ParseComputeApps(out, uuidFallback string) ([]ComputeApp, error) {
	var apps []ComputeApp
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A driver with no compute processes prints this rather than nothing.
		if strings.Contains(strings.ToLower(line), "no running") {
			continue
		}
		f := splitCSV(line)
		var pidStr, uuid, memStr string
		switch len(f) {
		case 3:
			pidStr, uuid, memStr = f[0], f[1], f[2]
		case 2:
			pidStr, uuid, memStr = f[0], uuidFallback, f[1]
		default:
			return nil, fmt.Errorf("%w: unparsable --query-compute-apps line %q",
				ErrProbeFailed, line)
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return nil, fmt.Errorf("%w: unparsable pid in %q", ErrProbeFailed, line)
		}
		used := mib(memStr)
		if used == nil {
			// `[N/A]` for a process whose memory the driver will not report. The
			// row still names a GPU the process is on, which is what the
			// exclusivity guard needs, so it is kept with a zero SIZE rather than
			// dropped — the confidence lives in gpu_attribution, not here.
			used = new(uint64)
		}
		apps = append(apps, ComputeApp{PID: pid, GPUUUID: uuid, UsedVRAMBytes: *used})
	}
	return apps, nil
}

// mib parses a MiB field and converts it to bytes — the one multiplication in
// the codebase. `[N/A]` and `[Not Supported]` become nil, never zero.
func mib(s string) *uint64 {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return nil
	}
	// Some drivers print a fractional MiB for used_gpu_memory.
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	b := uint64(v * MiB)
	return &b
}

func intOr(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func floatOr(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

// bracketed blanks the driver's `[N/A]` / `[Not Supported]` placeholders, which
// are not values and must never reach a column that says "compute capability".
func bracketed(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		return ""
	}
	return s
}

// splitCSV splits a `--format=csv` line. The fields nvidia-smi emits under this
// query never contain a comma or a quote — names are "NVIDIA GeForce RTX 4090",
// uuids are hex — so a full CSV reader would be machinery with nothing to do.
func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func fresh(at time.Time, o Options) bool {
	if o.ttl() <= 0 || at.IsZero() {
		return false
	}
	return o.now().Sub(at) < o.ttl()
}

// unknownFrom rebuilds the last known inventory with every memory field nil —
// D16's "failure marks GPUs unknown, never zero".
func unknownFrom(inv []GPU) []GPU {
	if len(inv) == 0 {
		return nil
	}
	out := make([]GPU, len(inv))
	for i, g := range inv {
		out[i] = GPU{
			Index: g.Index, UUID: g.UUID, Name: g.Name,
			ComputeCap: g.ComputeCap, DriverVersion: g.DriverVersion,
		}
	}
	return out
}

// identityOf keeps the stable half of an inventory for the failure path above.
func identityOf(gpus []GPU) []GPU {
	out := make([]GPU, len(gpus))
	for i, g := range gpus {
		out[i] = GPU{
			Index: g.Index, UUID: g.UUID, Name: g.Name,
			ComputeCap: g.ComputeCap, DriverVersion: g.DriverVersion,
		}
	}
	return out
}

// clone hands every caller its own slice: the cache is shared, and a consumer
// that sorted the result in place would reorder every other consumer's view.
func clone(gpus []GPU) []GPU {
	if gpus == nil {
		return nil
	}
	out := make([]GPU, len(gpus))
	copy(out, gpus)
	// The VRAM pointers are shared, which is safe only because nothing writes
	// through them; give each copy its own so that stays true by construction.
	for i := range out {
		out[i].VRAMTotalBytes = copyPtr(out[i].VRAMTotalBytes)
		out[i].VRAMUsedBytes = copyPtr(out[i].VRAMUsedBytes)
		out[i].VRAMFreeBytes = copyPtr(out[i].VRAMFreeBytes)
	}
	return out
}

func cloneApps(apps []ComputeApp) []ComputeApp {
	if apps == nil {
		return nil
	}
	out := make([]ComputeApp, len(apps))
	copy(out, apps)
	return out
}

func copyPtr(p *uint64) *uint64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
