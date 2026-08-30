package bench

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Export (DESIGN section 10, SPEC section 3.5): "`json` (runs + points +
// results, self-describing), `csv` (one row per result with all axes
// flattened), `md` (a table plus a provenance header naming model, quant,
// llama.cpp tag, driver and GPU) — ready to paste into an issue."
//
// The provenance header is the part that makes the markdown form worth having.
// A table of numbers pasted into an issue with no model, no quant, no llama.cpp
// tag and no driver version is a table nobody can act on, and the person who
// pasted it is invariably the one person who did not need it written down.

// Format is `?format=` on `GET /api/v1/bench/runs/{id}/export`.
type Format string

const (
	// FormatJSON is the self-describing archive: the run, its points and every
	// result including `raw_json`.
	FormatJSON Format = "json"
	// FormatCSV is one row per result with the axes flattened — a spreadsheet.
	FormatCSV Format = "csv"
	// FormatMarkdown is the provenance header plus a table, for an issue.
	FormatMarkdown Format = "md"
)

// FormatValues lists the three formats SPEC section 3.5 names, in order.
func FormatValues() []Format { return []Format{FormatJSON, FormatCSV, FormatMarkdown} }

// Valid reports whether f is one of the three.
func (f Format) Valid() bool {
	for _, v := range FormatValues() {
		if f == v {
			return true
		}
	}
	return false
}

// MediaType is the `Content-Type` this format is served as. Each is the
// registered type for the format, so a browser and a `curl -O` both do the right
// thing with the body.
func (f Format) MediaType() string {
	switch f {
	case FormatCSV:
		return "text/csv; charset=utf-8"
	case FormatMarkdown:
		return "text/markdown; charset=utf-8"
	default:
		return "application/json; charset=utf-8"
	}
}

// Extension is the filename suffix for `Content-Disposition`.
func (f Format) Extension() string {
	if f == FormatMarkdown {
		return ".md"
	}
	return "." + string(f)
}

// MediaTypes lists the three content types, for the route declaration.
func MediaTypes() []string {
	out := make([]string, 0, len(FormatValues()))
	for _, f := range FormatValues() {
		out = append(out, f.MediaType())
	}
	return out
}

// Export is a rendered export and the two headers it needs.
type Export struct {
	Format    Format
	MediaType string
	// Filename is the `Content-Disposition` name: the run's name, slugged, plus
	// the extension. A download called `export` is a download nobody can find
	// again after the third one.
	Filename string
	Body     []byte
}

// Export renders one run in the requested format.
func (s *Service) Export(ctx context.Context, runID string, format Format) (Export, error) {
	if format == "" {
		format = FormatJSON
	}
	if !format.Valid() {
		return Export{}, errorf(model.CodeBadFlags,
			"format %q is not one of json, csv, md", format)
	}

	var (
		run     store.BenchRun
		points  []store.BenchPoint
		results []store.BenchResult
	)
	if err := s.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if run, err = s.store.BenchRun(ctx, tx, runID); err != nil {
			return err
		}
		if points, err = s.store.BenchPoints(ctx, tx, runID); err != nil {
			return err
		}
		results, err = s.store.BenchResults(ctx, tx, runID)
		return err
	}); err != nil {
		return Export{}, err
	}

	var (
		body []byte
		err  error
	)
	switch format {
	case FormatCSV:
		body, err = exportCSV(run, points, results)
	case FormatMarkdown:
		body, err = exportMarkdown(run, points, results)
	default:
		body, err = exportJSON(run, points, results)
	}
	if err != nil {
		return Export{}, err
	}

	return Export{
		Format:    format,
		MediaType: format.MediaType(),
		Filename:  slug(run.Name) + format.Extension(),
		Body:      body,
	}, nil
}

// exportRun is the JSON form's run object. It is a DTO rather than the store row
// so the archive's field names are stable against a schema change — an export
// read back in two years must not need this year's column list.
type exportRun struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	ModelID     *string `json:"model_id"`
	ModelLabel  string  `json:"model_label"`
	ModelPath   string  `json:"model_path"`
	QuantLabel  *string `json:"quant_label"`
	LlamacppTag string  `json:"llamacpp_tag"`
	// LlamacppCommit and LlamacppBackend complete the "what was measured with"
	// half of §10's environment capture; GPU and Host are the "on what" half.
	LlamacppCommit  *string         `json:"llamacpp_commit"`
	LlamacppBackend string          `json:"llamacpp_backend"`
	GPU             json.RawMessage `json:"gpu"`
	Host            json.RawMessage `json:"host"`
	Sweep           json.RawMessage `json:"sweep"`
	Repetitions     int             `json:"repetitions"`
	PointsTotal     int             `json:"points_total"`
	PointsDone      int             `json:"points_done"`
	PointsFailed    int             `json:"points_failed"`
	Notes           *string         `json:"notes"`
	CreatedAt       string          `json:"created_at"`
	StartedAt       *string         `json:"started_at"`
	FinishedAt      *string         `json:"finished_at"`
}

type exportPoint struct {
	ID          string          `json:"id"`
	Ordinal     int             `json:"ordinal"`
	State       string          `json:"state"`
	Label       string          `json:"label"`
	Argv        json.RawMessage `json:"argv"`
	NGpuLayers  *int64          `json:"n_gpu_layers"`
	NBatch      *int64          `json:"n_batch"`
	NUbatch     *int64          `json:"n_ubatch"`
	NThreads    *int64          `json:"n_threads"`
	FlashAttn   *bool           `json:"flash_attn"`
	TypeK       *string         `json:"type_k"`
	TypeV       *string         `json:"type_v"`
	SplitMode   *string         `json:"split_mode"`
	TensorSplit *string         `json:"tensor_split"`
	NDepth      *int64          `json:"n_depth"`
	Error       *string         `json:"error_message"`
}

type exportResult struct {
	ID       string          `json:"id"`
	PointID  string          `json:"point_id"`
	TestKind string          `json:"test_kind"`
	NPrompt  int64           `json:"n_prompt"`
	NGen     int64           `json:"n_gen"`
	NDepth   int64           `json:"n_depth"`
	AvgTS    float64         `json:"avg_ts"`
	StddevTS float64         `json:"stddev_ts"`
	AvgNS    int64           `json:"avg_ns"`
	StddevNS int64           `json:"stddev_ns"`
	Samples  json.RawMessage `json:"samples_ns"`
	// Raw is llama-bench's own object, verbatim. It is in the archive for the
	// same reason it is in the column: a field this schema does not model today
	// is a field somebody wants tomorrow.
	Raw json.RawMessage `json:"raw"`
}

type exportDocument struct {
	// Format and Version identify the archive itself, so a reader knows what it
	// is holding without inferring it from the fields.
	Format  string `json:"format"`
	Version int    `json:"version"`
	// Runs is an array even for a single-run export: the shape does not change
	// when a multi-run export arrives, so a consumer written today keeps working.
	Runs    []exportRun    `json:"runs"`
	Points  []exportPoint  `json:"points"`
	Results []exportResult `json:"results"`
}

// ExportFormatName and ExportVersion identify the JSON archive.
const (
	ExportFormatName = "llamaman.bench"
	ExportVersion    = 1
)

func exportJSON(run store.BenchRun, points []store.BenchPoint, results []store.BenchResult) ([]byte, error) {
	doc := exportDocument{
		Format:  ExportFormatName,
		Version: ExportVersion,
		Runs: []exportRun{{
			ID:              run.ID,
			Name:            run.Name,
			State:           string(run.State),
			ModelID:         run.ModelID,
			ModelLabel:      run.ModelLabel,
			ModelPath:       run.ModelPath,
			QuantLabel:      run.QuantLabel,
			LlamacppTag:     run.LlamacppTag,
			LlamacppCommit:  run.LlamacppCommit,
			LlamacppBackend: run.LlamacppBackend,
			GPU:             json.RawMessage(run.GPUJSON),
			Host:            json.RawMessage(run.HostJSON),
			Sweep:           json.RawMessage(run.SweepJSON),
			Repetitions:     run.Repetitions,
			PointsTotal:     run.PointsTotal,
			PointsDone:      run.PointsDone,
			PointsFailed:    run.PointsFailed,
			Notes:           run.Notes,
			CreatedAt:       rfc3339(run.CreatedAt),
			StartedAt:       rfc3339Ptr(run.StartedAt),
			FinishedAt:      rfc3339Ptr(run.FinishedAt),
		}},
		Points:  make([]exportPoint, 0, len(points)),
		Results: make([]exportResult, 0, len(results)),
	}

	for _, p := range points {
		doc.Points = append(doc.Points, exportPoint{
			ID:          p.ID,
			Ordinal:     p.Ordinal,
			State:       string(p.State),
			Label:       PointLabel(p),
			Argv:        json.RawMessage(p.ArgsJSON),
			NGpuLayers:  p.NGpuLayers,
			NBatch:      p.NBatch,
			NUbatch:     p.NUbatch,
			NThreads:    p.NThreads,
			FlashAttn:   p.FlashAttn,
			TypeK:       p.TypeK,
			TypeV:       p.TypeV,
			SplitMode:   p.SplitMode,
			TensorSplit: p.TensorSplit,
			NDepth:      p.NDepth,
			Error:       p.ErrorMessage,
		})
	}
	for _, r := range results {
		item := exportResult{
			ID:       r.ID,
			PointID:  r.PointID,
			TestKind: string(r.TestKind),
			NPrompt:  r.NPrompt,
			NGen:     r.NGen,
			NDepth:   r.NDepth,
			AvgTS:    r.AvgTS,
			StddevTS: r.StddevTS,
			AvgNS:    r.AvgNS,
			StddevNS: r.StddevNS,
			Raw:      json.RawMessage(r.RawJSON),
		}
		if r.SamplesJSON != nil {
			item.Samples = json.RawMessage(*r.SamplesJSON)
		}
		doc.Results = append(doc.Results, item)
	}

	// Indented, because this file is read by people at least as often as by
	// programs — it is what gets attached to an issue.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("bench: render the JSON export: %w", err)
	}
	return buf.Bytes(), nil
}

// csvColumns is the CSV header, and its order is the contract: a spreadsheet
// somebody built a chart on top of must not have its columns reordered by a
// refactor.
var csvColumns = []string{
	"run_id", "run_name", "model_label", "quant_label",
	"llamacpp_tag", "llamacpp_commit", "llamacpp_backend",
	"ordinal", "point_label", "n_gpu_layers", "n_batch", "n_ubatch", "n_threads",
	"flash_attn", "type_k", "type_v", "split_mode", "tensor_split",
	"test_kind", "n_prompt", "n_gen", "n_depth",
	"avg_ts", "stddev_ts", "avg_ns", "stddev_ns", "samples",
}

func exportCSV(run store.BenchRun, points []store.BenchPoint, results []store.BenchResult) ([]byte, error) {
	rows := flattenResults(run, points, results)

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(csvColumns); err != nil {
		return nil, fmt.Errorf("bench: render the CSV export: %w", err)
	}
	for _, r := range rows {
		record := []string{
			r.RunID, r.RunName, run.ModelLabel, str(run.QuantLabel),
			run.LlamacppTag, str(run.LlamacppCommit), run.LlamacppBackend,
			strconv.Itoa(r.Ordinal), r.Label,
			i64str(r.NGpuLayers), i64str(r.NBatch), i64str(r.NUbatch), i64str(r.NThreads),
			boolStr(r.FlashAttn), str(r.TypeK), str(r.TypeV), str(r.SplitMode), str(r.TensorSplit),
			string(r.TestKind),
			strconv.FormatInt(r.NPrompt, 10), strconv.FormatInt(r.NGen, 10),
			strconv.FormatInt(r.NDepth, 10),
			formatTS(r.AvgTS), formatTS(r.StddevTS),
			strconv.FormatInt(r.AvgNS, 10), strconv.FormatInt(r.StddevNS, 10),
			strconv.Itoa(r.Samples),
		}
		if err := w.Write(record); err != nil {
			return nil, fmt.Errorf("bench: render the CSV export: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("bench: render the CSV export: %w", err)
	}
	return buf.Bytes(), nil
}

func exportMarkdown(run store.BenchRun, points []store.BenchPoint, results []store.BenchResult) ([]byte, error) {
	rows := flattenResults(run, points, results)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", run.Name)

	// The provenance header. Every line of it is a thing the numbers below are
	// meaningless without, which is why §10 enumerates them rather than leaving
	// the choice to the renderer.
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Model | %s |\n", mdCell(run.ModelLabel))
	if run.QuantLabel != nil {
		fmt.Fprintf(&b, "| Quantization | %s |\n", mdCell(*run.QuantLabel))
	}
	fmt.Fprintf(&b, "| llama.cpp | %s (%s) |\n", mdCell(run.LlamacppTag), mdCell(run.LlamacppBackend))
	if run.LlamacppCommit != nil {
		fmt.Fprintf(&b, "| Commit | `%s` |\n", mdCell(*run.LlamacppCommit))
	}
	for _, line := range gpuLines(run.GPUJSON) {
		fmt.Fprintf(&b, "| GPU | %s |\n", mdCell(line))
	}
	for _, pair := range hostLines(run.HostJSON) {
		fmt.Fprintf(&b, "| %s | %s |\n", pair[0], mdCell(pair[1]))
	}
	fmt.Fprintf(&b, "| Repetitions | %d |\n", run.Repetitions)
	fmt.Fprintf(&b, "| Points | %d of %d succeeded, %d failed |\n",
		run.PointsDone, run.PointsTotal, run.PointsFailed)
	fmt.Fprintf(&b, "| Run at | %s |\n", rfc3339(run.CreatedAt))
	b.WriteString("\n")

	if run.Notes != nil && *run.Notes != "" {
		for _, line := range strings.Split(*run.Notes, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Fprintf(&b, "> %s\n", line)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("| configuration | test | t/s | ± | avg ms |\n")
	b.WriteString("|---|---|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			mdCell(r.Label), mdCell(testLabel(r)),
			formatTS(r.AvgTS), formatTS(r.StddevTS),
			formatTS(float64(r.AvgNS)/1e6))
	}
	if len(rows) == 0 {
		b.WriteString("| _no results_ | | | | |\n")
	}
	return []byte(b.String()), nil
}

// testLabel renders a result's shape the way llama-bench's own table does.
func testLabel(r ResultRow) string {
	var parts []string
	if r.NPrompt > 0 {
		parts = append(parts, "pp"+strconv.FormatInt(r.NPrompt, 10))
	}
	if r.NGen > 0 {
		parts = append(parts, "tg"+strconv.FormatInt(r.NGen, 10))
	}
	label := strings.Join(parts, "+")
	if label == "" {
		label = string(r.TestKind)
	}
	if r.NDepth > 0 {
		label += " @ d" + strconv.FormatInt(r.NDepth, 10)
	}
	return label
}

// gpuLines renders the captured GPU array into one line per card. A capture that
// will not parse yields nothing rather than an error: the export's job is to
// carry what is known, and a malformed provenance field must not cost a user
// their results table.
func gpuLines(raw string) []string {
	var gpus []struct {
		Name   string `json:"name"`
		UUID   string `json:"uuid"`
		Driver string `json:"driver"`
		VRAM   uint64 `json:"vram_total_bytes"`
	}
	if err := json.Unmarshal([]byte(raw), &gpus); err != nil {
		return nil
	}
	out := make([]string, 0, len(gpus))
	for _, g := range gpus {
		line := g.Name
		if g.VRAM > 0 {
			line += fmt.Sprintf(", %.1f GiB", float64(g.VRAM)/(1<<30))
		}
		if g.Driver != "" {
			line += ", driver " + g.Driver
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// hostLines renders the captured host facts as label/value pairs, in a fixed
// order so two exports diff cleanly.
func hostLines(raw string) [][2]string {
	var host struct {
		CPU      string `json:"cpu"`
		Cores    int    `json:"cores"`
		RAMBytes int64  `json:"ram_bytes"`
		Kernel   string `json:"kernel"`
	}
	if err := json.Unmarshal([]byte(raw), &host); err != nil {
		return nil
	}
	var out [][2]string
	if host.CPU != "" {
		cpu := host.CPU
		if host.Cores > 0 {
			cpu += fmt.Sprintf(" (%d cores)", host.Cores)
		}
		out = append(out, [2]string{"CPU", cpu})
	}
	if host.RAMBytes > 0 {
		out = append(out, [2]string{"RAM", fmt.Sprintf("%.1f GiB", float64(host.RAMBytes)/(1<<30))})
	}
	if host.Kernel != "" {
		out = append(out, [2]string{"Kernel", host.Kernel})
	}
	return out
}

// mdCell escapes the one character that would break a markdown table row.
func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// formatTS renders a throughput with two decimals, which is llama-bench's own
// precision and enough to tell two builds apart without implying more.
func formatTS(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func i64str(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func boolStr(p *bool) string {
	if p == nil {
		return ""
	}
	if *p {
		return "1"
	}
	return "0"
}

func rfc3339(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func rfc3339Ptr(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := rfc3339(*ms)
	return &s
}

// slug makes a filename out of a run name: lowercase, with every run of
// non-alphanumerics collapsed to a single dash. A `Content-Disposition` filename
// travels through shells, file managers and archive tools, and a name with a
// quote or a slash in it breaks at least one of them.
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "bench"
	}
	if len(out) > 80 {
		out = strings.Trim(out[:80], "-")
	}
	return out
}
