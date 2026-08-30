package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The fit observation of §5.8 and §8.7 (D32, D33).
//
// On the FIRST `ready` transition of a run — and only when
// `runtime_info.journal_read = 'ok'` (D77) — the last 200 journal lines of the
// instance's unit are scanned for llama.cpp's own buffer report and its `--fit`
// projection. What is parsed goes two places: `instance_status.fit_report_json`,
// which the instance page shows beside the estimate as "reported by llama.cpp",
// and a `fit_observations` row BESIDE THE PREDICTION THAT WAS MADE, which is
// what the calculator's calibration learns from.
//
// The gate matters as much as the parse. Without journal access the scan returns
// nothing, and a row written from an empty scan would be a `0` actual that drags
// every median toward zero — a calculator "calibrated" from a host it cannot
// observe. So the observation is SKIPPED and reports stay `confidence:
// "modeled"`, which F23 lists as the honest degradation.

// FitBufferKind names the three allocations llama.cpp reports at load.
type FitBufferKind string

const (
	// FitBufferModel is `load_tensors: … model buffer size`.
	FitBufferModel FitBufferKind = "model"
	// FitBufferKV is `llama_kv_cache…: … KV buffer size` (and the older
	// `llama_kv_cache_unified` spelling).
	FitBufferKV FitBufferKind = "kv"
	// FitBufferCompute is `llama_context: … compute buffer size`.
	FitBufferCompute FitBufferKind = "compute"
)

// FitBuffer is one reported allocation.
type FitBuffer struct {
	Kind FitBufferKind `json:"kind"`
	// Device is the backend buffer's name as llama.cpp printed it: `CUDA0`,
	// `CPU`, `CPU_Mapped`, `CUDA_Host`. It is kept verbatim because the split
	// between device and host memory is the whole reason the totals below are
	// two numbers rather than one.
	Device string `json:"device"`
	Bytes  uint64 `json:"bytes"`
	// Host reports whether this buffer lives in system RAM rather than on a
	// device. `CPU`, `CPU_Mapped` and anything ending in `_Host` are host
	// buffers; a calculator that summed them into the VRAM figure would learn a
	// correction of about two.
	Host bool `json:"host"`
}

// FitReport is what one journal scan learned. It is the shape stored verbatim in
// `instance_status.fit_report_json`.
type FitReport struct {
	// The three device-side totals, in bytes.
	WeightsBytes uint64 `json:"weights_bytes"`
	KVBytes      uint64 `json:"kv_bytes"`
	ComputeBytes uint64 `json:"compute_bytes"`
	// The host-side totals, kept apart for the reason FitBuffer.Host gives.
	WeightsHostBytes uint64 `json:"weights_host_bytes"`
	KVHostBytes      uint64 `json:"kv_host_bytes"`
	ComputeHostBytes uint64 `json:"compute_host_bytes"`

	Buffers []FitBuffer `json:"buffers,omitempty"`

	// FitLayers is llama.cpp's OWN `--fit` projection: how many layers it chose
	// to offload. It is the ground truth of D33 and is nil when the build did not
	// print one — which upstream does not when `-ngl` or `--tensor-split` is
	// pinned, and which the UI renders as "unavailable" rather than as zero.
	FitLayers      *int `json:"fit_layers,omitempty"`
	FitLayersTotal *int `json:"fit_layers_total,omitempty"`

	// OOM marks a load that died allocating.
	OOM bool `json:"oom"`
	// Found reports whether the scan recognized anything at all. A scan that
	// found nothing writes no row: see the gate above.
	Found bool `json:"found"`
}

// TotalBytes is the device-side sum, which is what `actual_total_bytes` stores.
func (r FitReport) TotalBytes() uint64 {
	return r.WeightsBytes + r.KVBytes + r.ComputeBytes
}

// Journal reads an instance unit's recent output.
//
// It is an interface this package declares rather than a call into
// internal/systemd, and that is D49's second invariant in practice: only
// internal/systemd execs `journalctl`. internal/app satisfies this with a
// `systemd.Tail`; a test satisfies it with a slice of lines.
type Journal interface {
	// Tail returns at most n lines of the unit's output, oldest first.
	Tail(ctx context.Context, unit string, n int) ([]string, error)
}

// FitJournalLines is the scan depth §5.8 names.
const FitJournalLines = 200

// FitPrediction is the estimate that was made for a run, carried here so the
// observation can be written beside it.
//
// The supervisor does not compute this. It cannot: the calculator needs the
// model's GGUF shape and the live GPU inventory, and a reconcile loop that read
// both on every ready transition would be a reconcile loop that forks nvidia-smi
// and reparses a header to write a history row. The composition root supplies a
// FitPredictor instead, and a nil one simply means no `fit_observations` rows —
// `fit_report_json` is still stamped, so D33's "reported by llama.cpp" panel
// works either way.
type FitPrediction struct {
	// The calibration key of D32.
	Arch        string
	Backend     model.Backend
	LlamacppTag string
	GPUName     string

	// PredictedComputeBytes is the calculator's CB for this configuration — the
	// denominator of the ratio D32 learns.
	PredictedComputeBytes int64

	// The shape and flags the prediction was made from, stored so a row can be
	// read a month later without joining back to a reconfigured instance.
	NLayer     *int64
	NEmbd      *int64
	NHead      *int64
	NHeadKV    *int64
	NVocab     *int64
	NCtx       *int64
	NBatch     *int64
	NUbatch    *int64
	NParallel  *int64
	FlashAttn  *bool
	TypeK      *string
	TypeV      *string
	NGpuLayers *int64
}

// FitPredictor answers "what did the calculator say for this instance?".
type FitPredictor interface {
	// Predict returns the prediction for an instance's CURRENT configuration.
	// ok is false when there is nothing to compare against — an unparsed model,
	// no active build — and no observation is then written.
	Predict(ctx context.Context, instanceID string) (p FitPrediction, ok bool, err error)
}

// observeFit runs the scan for one run's first `ready`.
//
// It runs OUTSIDE the pass's own transaction, deliberately: the scan forks
// journalctl, and holding a write transaction open across a subprocess would
// block every other writer for as long as the journal takes to answer. A failure
// anywhere in here is logged and dropped — an instance that is serving requests
// must not be reported as unhealthy because a history row could not be written.
func (s *Supervisor) observeFit(ctx context.Context, inst model.Instance,
	status model.InstanceStatus, now int64) {

	if s.cfg.Journal == nil {
		return
	}

	// D77's gate. `journal_read` is a fact the boot sequence probed once; asking
	// per observation is one indexed read on a singleton row, and it is read
	// fresh because the answer changes when an operator applies F23's usermod
	// and restarts nothing but the daemon.
	var info model.RuntimeInfo
	if err := s.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		info, err = s.st.RuntimeInfo(ctx, tx)
		return err
	}); err != nil {
		s.log.Debug("supervisor: fit observation skipped, runtime info unreadable",
			slog.String("instance", inst.Name), slog.String("error", err.Error()))
		return
	}
	if info.JournalRead == nil || *info.JournalRead != model.JournalOK {
		// F23: every journal-derived feature degrades loudly rather than
		// returning empty. Here that means no row at all, so reports stay
		// `modeled` instead of being "calibrated" from a scan of nothing.
		return
	}

	lines, err := s.cfg.Journal.Tail(ctx, unitName(inst), FitJournalLines)
	if err != nil {
		s.log.Debug("supervisor: fit observation skipped, journal unreadable",
			slog.String("instance", inst.Name), slog.String("error", err.Error()))
		return
	}

	rep := ParseFitReport(lines)
	if !rep.Found {
		return
	}

	var (
		pred   FitPrediction
		hasPre bool
	)
	if s.cfg.Fit != nil {
		pred, hasPre, err = s.cfg.Fit.Predict(ctx, inst.ID)
		if err != nil {
			s.log.Debug("supervisor: fit prediction unavailable",
				slog.String("instance", inst.Name), slog.String("error", err.Error()))
			hasPre = false
		}
	}

	blob, err := json.Marshal(rep)
	if err != nil {
		s.log.Debug("supervisor: fit report is not serializable",
			slog.String("instance", inst.Name), slog.String("error", err.Error()))
		return
	}
	next := status
	next.FitReportJSON = ptr(string(blob))

	obs, writeObs := fitObservation(s.newID(s.now()), now, pred, hasPre, rep)

	if err := s.st.Write(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := s.st.UpdateInstanceStatus(ctx, tx, next); err != nil {
			return err
		}
		if !writeObs {
			return nil
		}
		return s.st.InsertFitObservation(ctx, tx, obs)
	}); err != nil {
		s.log.Warn("supervisor: fit observation not recorded",
			slog.String("instance", inst.Name), slog.String("error", err.Error()))
	}
}

// fitObservation builds the row, and reports whether there is one worth writing.
//
// A row is written only when there is BOTH a prediction and a parsed actual: the
// table's whole purpose is the ratio between them, and half a ratio is not a
// weaker signal, it is no signal.
func fitObservation(id string, now int64, p FitPrediction, hasPrediction bool,
	rep FitReport) (model.FitObservation, bool) {

	if !hasPrediction || p.PredictedComputeBytes <= 0 {
		return model.FitObservation{}, false
	}
	o := model.FitObservation{
		ID: id, At: now,
		Arch: p.Arch, Backend: p.Backend, LlamacppTag: p.LlamacppTag,
		NLayer: p.NLayer, NEmbd: p.NEmbd, NHead: p.NHead, NHeadKV: p.NHeadKV,
		NVocab: p.NVocab, NCtx: p.NCtx, NBatch: p.NBatch, NUbatch: p.NUbatch,
		NParallel: p.NParallel, FlashAttn: p.FlashAttn,
		TypeK: p.TypeK, TypeV: p.TypeV, NGpuLayers: p.NGpuLayers,
		PredictedBytes: p.PredictedComputeBytes,
		OOM:            rep.OOM,
		Source:         model.FitFromInstanceStart,
	}
	if p.GPUName != "" {
		o.GPUName = ptr(p.GPUName)
	}
	// Each actual is written only when its line was actually found. A buffer
	// llama.cpp did not print is NULL, never 0 — a zero would enter the median
	// as a claim that the allocation was free.
	if rep.WeightsBytes > 0 {
		o.ActualWeightsBytes = ptr(int64(rep.WeightsBytes))
	}
	if rep.KVBytes > 0 {
		o.ActualKVBytes = ptr(int64(rep.KVBytes))
	}
	if rep.ComputeBytes > 0 {
		o.ActualComputeBytes = ptr(int64(rep.ComputeBytes))
	}
	if total := rep.TotalBytes(); total > 0 {
		o.ActualTotalBytes = ptr(int64(total))
	}
	return o, true
}

// ParseFitReport reads llama.cpp's own startup lines.
//
// The shapes it recognizes, as upstream prints them:
//
//	load_tensors:        CUDA0 model buffer size =  4155.99 MiB
//	load_tensors:   CPU_Mapped model buffer size =   281.81 MiB
//	llama_kv_cache_unified:  CUDA0 KV buffer size =   896.00 MiB
//	llama_context:      CUDA0 compute buffer size =   304.00 MiB
//	llama_context:  CUDA_Host compute buffer size =    24.01 MiB
//
// It is deliberately keyed on the phrase rather than on the log prefix, because
// the prefixes are the half of these lines upstream renames — `llama_kv_cache`,
// `llama_kv_cache_unified` and `llama_kv_cache_unified_iswa` have all shipped —
// while "KV buffer size" has been stable across every one of them.
//
// It is exported and pure so the recorded-log fixtures can be run against it
// directly, which is the only way to test a parser whose input is another
// project's output.
func ParseFitReport(lines []string) FitReport {
	var r FitReport
	for _, line := range lines {
		low := strings.ToLower(line)

		switch {
		case strings.Contains(low, "cudamalloc failed"),
			strings.Contains(low, "out of memory"),
			strings.Contains(low, "failed to allocate"):
			r.OOM, r.Found = true, true
			continue
		}

		if n, total, ok := parseFitProjection(line); ok {
			r.FitLayers, r.Found = &n, true
			if total > 0 {
				r.FitLayersTotal = &total
			}
			continue
		}

		var kind FitBufferKind
		switch {
		case strings.Contains(low, "model buffer size"):
			kind = FitBufferModel
		case strings.Contains(low, "kv buffer size"):
			kind = FitBufferKV
		case strings.Contains(low, "compute buffer size"):
			kind = FitBufferCompute
		default:
			continue
		}

		bytes, ok := parseSizeAfterEquals(line)
		if !ok {
			continue
		}
		buf := FitBuffer{
			Kind:   kind,
			Device: bufferDevice(line, string(kind)),
			Bytes:  bytes,
		}
		buf.Host = isHostBuffer(buf.Device)
		r.Buffers = append(r.Buffers, buf)
		r.Found = true

		switch {
		case kind == FitBufferModel && buf.Host:
			r.WeightsHostBytes += bytes
		case kind == FitBufferModel:
			r.WeightsBytes += bytes
		case kind == FitBufferKV && buf.Host:
			r.KVHostBytes += bytes
		case kind == FitBufferKV:
			r.KVBytes += bytes
		case kind == FitBufferCompute && buf.Host:
			r.ComputeHostBytes += bytes
		default:
			r.ComputeBytes += bytes
		}
	}
	return r
}

// bufferDevice pulls the backend name out of a buffer line: it is the last word
// before the phrase, after the `prefix:` this line's subsystem printed.
func bufferDevice(line, kind string) string {
	low := strings.ToLower(line)
	var marker string
	switch FitBufferKind(kind) {
	case FitBufferModel:
		marker = "model buffer size"
	case FitBufferKV:
		marker = "kv buffer size"
	default:
		marker = "compute buffer size"
	}
	idx := strings.Index(low, marker)
	if idx < 0 {
		return ""
	}
	head := line[:idx]
	if c := strings.LastIndex(head, ":"); c >= 0 {
		head = head[c+1:]
	}
	fields := strings.Fields(head)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// isHostBuffer reports whether a backend buffer name names system RAM.
func isHostBuffer(device string) bool {
	d := strings.ToUpper(device)
	if d == "" {
		return false
	}
	return d == "CPU" || strings.HasPrefix(d, "CPU_") || strings.HasSuffix(d, "_HOST")
}

// parseSizeAfterEquals reads `= 1234.56 MiB` and returns bytes.
//
// The unit is part of the line and is honored rather than assumed: llama.cpp
// prints MiB for almost everything and GiB for a large mapped model, and a
// parser that assumed MiB would under-report the second by a factor of 1024.
func parseSizeAfterEquals(line string) (uint64, bool) {
	_, rest, ok := strings.Cut(line, "=")
	if !ok {
		return 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(fields[0], ","), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	unit := ""
	if len(fields) > 1 {
		unit = strings.ToUpper(strings.TrimSuffix(fields[1], ","))
	}
	var scale float64
	switch unit {
	case "B", "BYTES", "":
		scale = 1
	case "KIB", "KB":
		scale = 1 << 10
	case "MIB", "MB":
		scale = 1 << 20
	case "GIB", "GB":
		scale = 1 << 30
	default:
		return 0, false
	}
	return uint64(v * scale), true
}

// parseFitProjection reads llama.cpp's own `--fit` line, D33's ground truth.
//
// Upstream's wording has moved around, so the match is on the two facts that
// have not: the line mentions the fit, and it names a layer count. "33 of 33
// layers" yields (33, 33); a line with one number yields (n, 0).
func parseFitProjection(line string) (n, total int, ok bool) {
	low := strings.ToLower(line)
	if !strings.Contains(low, "fit") || !strings.Contains(low, "layer") {
		return 0, 0, false
	}
	// Only consider the text before the word "layer", so a trailing "on GPU 0"
	// or a byte count cannot be mistaken for the count.
	head := low[:strings.Index(low, "layer")]
	var nums []int
	for _, f := range strings.Fields(head) {
		f = strings.Trim(f, ":=,()")
		if f == "" {
			continue
		}
		if v, err := strconv.Atoi(f); err == nil && v >= 0 {
			nums = append(nums, v)
		}
	}
	switch len(nums) {
	case 0:
		return 0, 0, false
	case 1:
		return nums[0], 0, true
	default:
		return nums[len(nums)-2], nums[len(nums)-1], true
	}
}

// String renders a report for a log line.
func (r FitReport) String() string {
	return fmt.Sprintf("weights=%d kv=%d compute=%d oom=%v", r.WeightsBytes, r.KVBytes,
		r.ComputeBytes, r.OOM)
}
