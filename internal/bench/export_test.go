package bench

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Export, comparison and history (DESIGN section 10, SPEC section 3.5), against
// a run that actually executed: the three formats are rendered from the same
// rows the UI reads, so a change to the flattening is caught in all four places
// at once.

// finishedRun runs one sweep to completion and returns its id.
func finishedRun(t *testing.T, f *fixture) string {
	t.Helper()
	created := f.createRun(simpleSweep(), false)
	if _, err := f.queue.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := f.mustRun(created.Run.ID).State; got != model.BenchSucceeded {
		t.Fatalf("the fixture run is %s, want succeeded", got)
	}
	return created.Run.ID
}

func TestExportJSON(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	id := finishedRun(t, f)

	out, err := f.svc.Export(context.Background(), id, FormatJSON)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if out.MediaType != "application/json; charset=utf-8" {
		t.Errorf("media type = %q", out.MediaType)
	}
	if !strings.HasSuffix(out.Filename, ".json") {
		t.Errorf("filename = %q, want a .json suffix", out.Filename)
	}

	var doc struct {
		Format  string           `json:"format"`
		Version int              `json:"version"`
		Runs    []map[string]any `json:"runs"`
		Points  []map[string]any `json:"points"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatalf("the JSON export does not parse: %v", err)
	}

	if doc.Format != ExportFormatName || doc.Version != ExportVersion {
		t.Errorf("archive identity = %s v%d, want %s v%d",
			doc.Format, doc.Version, ExportFormatName, ExportVersion)
	}
	if len(doc.Runs) != 1 || len(doc.Points) != 2 || len(doc.Results) != 4 {
		t.Fatalf("archive has %d runs, %d points and %d results; want 1, 2 and 4",
			len(doc.Runs), len(doc.Points), len(doc.Results))
	}

	// The provenance half of section 10's environment capture: without it a
	// cross-version comparison of these numbers is meaningless.
	for _, field := range []string{
		"model_label", "model_path", "llamacpp_tag", "llamacpp_backend",
		"gpu", "host", "sweep", "repetitions",
	} {
		if _, ok := doc.Runs[0][field]; !ok {
			t.Errorf("the exported run omits %q", field)
		}
	}
	// The captured GPU array is an array, not a base64 blob or a string.
	gpus, ok := doc.Runs[0]["gpu"].([]any)
	if !ok || len(gpus) != 2 {
		t.Errorf("gpu = %#v, want the two captured cards", doc.Runs[0]["gpu"])
	}

	// `raw_json` survives into the archive, which is the whole reason it is a
	// column: a field this schema does not model today is one somebody wants
	// tomorrow.
	raw, ok := doc.Results[0]["raw"].(map[string]any)
	if !ok {
		t.Fatalf("raw = %#v, want llama-bench's own object", doc.Results[0]["raw"])
	}
	if _, ok := raw["build_commit"]; !ok {
		t.Error("the archive lost llama-bench's build_commit")
	}
	if _, ok := doc.Points[0]["argv"].([]any); !ok {
		t.Errorf("argv = %#v, want the command line the point ran", doc.Points[0]["argv"])
	}
}

func TestExportCSV(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	id := finishedRun(t, f)

	out, err := f.svc.Export(context.Background(), id, FormatCSV)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if out.MediaType != "text/csv; charset=utf-8" {
		t.Errorf("media type = %q", out.MediaType)
	}
	if !strings.HasSuffix(out.Filename, ".csv") {
		t.Errorf("filename = %q, want a .csv suffix", out.Filename)
	}

	records, err := csv.NewReader(strings.NewReader(string(out.Body))).ReadAll()
	if err != nil {
		t.Fatalf("the CSV export does not parse: %v", err)
	}
	if len(records) != 5 {
		t.Fatalf("got %d CSV rows (header included), want 5", len(records))
	}
	if diff := cmp.Diff(csvColumns, records[0]); diff != "" {
		t.Errorf("CSV header (-want +got):\n%s", diff)
	}

	// One row per RESULT with every axis flattened beside it, which is what
	// makes the file useful in a spreadsheet without a join.
	byName := map[string]string{}
	for i, name := range records[0] {
		byName[name] = records[1][i]
	}
	if byName["n_batch"] == "" || byName["point_label"] == "" {
		t.Errorf("the axes were not flattened onto the row: %v", byName)
	}
	if byName["llamacpp_tag"] == "" || byName["model_label"] == "" {
		t.Errorf("the row carries no provenance: %v", byName)
	}
	// The first row is the first point's pp result: BenchResults orders by point
	// ordinal then result id, and store.NewID mints in the order the fixture's
	// objects were parsed. Asserted rather than assumed, because the three
	// numbers below belong to that object.
	if byName["test_kind"] != string(model.TestPP) {
		t.Fatalf("the first CSV row is a %q result, want %q — the export must keep "+
			"llama-bench's own order", byName["test_kind"], model.TestPP)
	}
	if byName["avg_ts"] != "6058.31" {
		t.Errorf("avg_ts = %q, want 6058.31", byName["avg_ts"])
	}
	if byName["stddev_ts"] != "86.42" {
		t.Errorf("stddev_ts = %q, want 86.42", byName["stddev_ts"])
	}
	if byName["samples"] != "3" {
		t.Errorf("samples = %q, want 3", byName["samples"])
	}
}

func TestExportMarkdown(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	id := finishedRun(t, f)

	out, err := f.svc.Export(context.Background(), id, FormatMarkdown)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if out.MediaType != "text/markdown; charset=utf-8" {
		t.Errorf("media type = %q", out.MediaType)
	}
	if !strings.HasSuffix(out.Filename, ".md") {
		t.Errorf("filename = %q, want a .md suffix", out.Filename)
	}

	body := string(out.Body)
	// Section 10's provenance header, name by name. A table of numbers pasted
	// into an issue without these is a table nobody can act on.
	for _, want := range []string{
		"# test run",
		"| Model |",
		"| llama.cpp |",
		"| GPU |",
		"| Repetitions |",
		"| configuration | test | t/s | ± | avg ms |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the markdown export has no %q line:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "NVIDIA GeForce RTX 4090") {
		t.Error("the provenance header does not name the GPU")
	}
	if !strings.Contains(body, "driver 560.35.03") {
		t.Error("the provenance header does not name the driver version")
	}
	if !strings.Contains(body, "| pp512 |") || !strings.Contains(body, "| tg128 |") {
		t.Errorf("the results table is missing a test shape:\n%s", body)
	}
	if !strings.Contains(body, "6058.31") {
		t.Error("the results table does not carry the throughput")
	}
}

func TestExportRejectsAnUnknownFormat(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	id := finishedRun(t, f)

	_, err := f.svc.Export(context.Background(), id, Format("xlsx"))
	if err == nil {
		t.Fatal("Export accepted a format SPEC section 3.5 does not name")
	}
	var me model.Error
	if !asModelError(err, &me) || me.Code != model.CodeBadFlags {
		t.Fatalf("got %v, want %s", err, model.CodeBadFlags)
	}
}

func TestExportFilenameIsASlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"test run", "test-run"},
		{"Qwen3 8B — Q4_K_M / ngl sweep", "qwen3-8b-q4-k-m-ngl-sweep"},
		{"   ", "bench"},
		{"...", "bench"},
	}
	for _, tt := range tests {
		if got := slug(tt.in); got != tt.want {
			t.Errorf("slug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCompareGroupsInSQL is section 10's "one grouped query rather than
// post-processing in Go": the axes are columns and the aggregation happens in
// SQLite.
func TestCompareGroupsInSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	id := finishedRun(t, f)

	points, err := f.svc.Compare(ctx, store.BenchCompareQuery{
		RunIDs:  []string{id},
		X:       store.AxisNBatch,
		Series:  store.AxisTestKind,
		Y:       store.MetricAvgTS,
		Filters: map[store.BenchAxis]string{store.AxisTestKind: string(model.TestTG)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	// Two batch sizes, one series, filtered to tg.
	if len(points) != 2 {
		t.Fatalf("got %d comparison points, want 2: %+v", len(points), points)
	}
	for _, p := range points {
		if p.Series != string(model.TestTG) {
			t.Errorf("the filter let a %s series through", p.Series)
		}
		if p.Value != 116.74 {
			t.Errorf("value = %v, want the tg throughput 116.74", p.Value)
		}
		if p.Samples != 1 {
			t.Errorf("samples = %d, want 1", p.Samples)
		}
	}
	// The numeric ordering is what puts `512` before `2048` on an x axis.
	if points[0].X != "512" || points[1].X != "2048" {
		t.Errorf("x order = %q, %q; want 512 then 2048", points[0].X, points[1].X)
	}
}

func TestCompareRejectsUnknownAxes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)

	tests := []struct {
		name string
		q    store.BenchCompareQuery
	}{
		{"an x that is not an axis", store.BenchCompareQuery{
			X: "temperature", Y: store.MetricAvgTS}},
		{"a y that is not a metric", store.BenchCompareQuery{
			X: store.AxisNBatch, Y: "vibes"}},
		{"a series that is not an axis", store.BenchCompareQuery{
			X: store.AxisNBatch, Y: store.MetricAvgTS, Series: "phase_of_the_moon"}},
		{"a filter key that is not an axis", store.BenchCompareQuery{
			X: store.AxisNBatch, Y: store.MetricAvgTS,
			Filters: map[store.BenchAxis]string{"'; DROP TABLE bench_runs; --": "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Compare(ctx, tt.q)
			if err == nil {
				t.Fatal("Compare accepted an axis it cannot express")
			}
			var me model.Error
			if !asModelError(err, &me) || me.Code != model.CodeBadFlags {
				t.Fatalf("got %v, want %s", err, model.CodeBadFlags)
			}
		})
	}
}

func TestSeriesGroupsByLlamacppTag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)
	finishedRun(t, f)

	points, err := f.svc.Series(ctx, store.BenchSeriesQuery{
		ModelID: f.ModelID,
		Test:    model.TestTG,
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d history points, want 1: %+v", len(points), points)
	}
	if points[0].Group != testVersion {
		t.Errorf("group = %q, want the llama.cpp tag %q", points[0].Group, testVersion)
	}
	if points[0].Value != 116.74 {
		t.Errorf("value = %v, want the tg throughput", points[0].Value)
	}
	if points[0].Samples != 2 {
		t.Errorf("samples = %d, want 2 (one tg result per point)", points[0].Samples)
	}
}

// TestSecondsPerPointEstimate: the duration estimate comes from prior runs'
// MEDIAN seconds-per-point, and a model with no history reports zero samples so
// the preflight can say "estimated" rather than "measured".
func TestSecondsPerPointEstimate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, nil)

	var (
		secs    float64
		samples int
	)
	read := func() {
		t.Helper()
		if err := f.store.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			secs, samples, err = f.store.BenchSecondsPerPoint(ctx, tx, f.ModelID)
			return err
		}); err != nil {
			t.Fatalf("BenchSecondsPerPoint: %v", err)
		}
	}

	read()
	if samples != 0 {
		t.Fatalf("a fresh host reported %d samples", samples)
	}

	// Run a sweep, advancing the clock across each point so the durations are
	// real numbers rather than zeros.
	f.run.onPoint = func(_ context.Context, n int) ([]byte, error) {
		f.clock.Advance(30 * 1000 * 1000 * 1000) // 30 s
		return mustFixture(t, "llama-bench-pp-tg.json"), nil
	}
	finishedRun(t, f)

	read()
	if samples != 2 {
		t.Fatalf("got %d samples, want 2", samples)
	}
	if secs != 30 {
		t.Errorf("median seconds per point = %v, want 30", secs)
	}
}

// asModelError is errors.As for model.Error without pulling `errors` into a file
// whose subject is formatting.
func asModelError(err error, target *model.Error) bool {
	me, ok := err.(model.Error)
	if ok {
		*target = me
	}
	return ok
}
