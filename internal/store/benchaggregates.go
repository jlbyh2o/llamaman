package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// Benchmark comparison and history queries (DESIGN section 10, section 3.13).
//
// "Comparisons are plain SQL over `bench_points ⋈ bench_results` with the sweep
// axes as columns, so 'tg128 across ngl for three llama.cpp builds' is ONE
// grouped query rather than post-processing in Go." That sentence is the whole
// specification of this file, and it is why the grouping, the aggregation and
// the filtering all happen in SQLite rather than over a slice.
//
// The axis and metric names arrive from a request body, so they are not
// interpolated: BenchAxes and BenchMetrics are closed maps from the wire name to
// the SQL expression, and a name with no entry is refused before any string is
// built. That is the only safe way to have a caller choose a GROUP BY column.

// BenchAxis is a name a comparison may group or filter on — a sweep axis, a
// result shape, or a fact about the run.
type BenchAxis string

// The axes §3.13's compare body can name. Each maps to one expression over the
// `bench_runs ⋈ bench_points ⋈ bench_results` join below.
const (
	AxisNGpuLayers  BenchAxis = "n_gpu_layers"
	AxisNBatch      BenchAxis = "n_batch"
	AxisNUbatch     BenchAxis = "n_ubatch"
	AxisNThreads    BenchAxis = "n_threads"
	AxisFlashAttn   BenchAxis = "flash_attn"
	AxisTypeK       BenchAxis = "type_k"
	AxisTypeV       BenchAxis = "type_v"
	AxisSplitMode   BenchAxis = "split_mode"
	AxisTensorSplit BenchAxis = "tensor_split"
	AxisNDepth      BenchAxis = "n_depth"
	AxisNPrompt     BenchAxis = "n_prompt"
	AxisNGen        BenchAxis = "n_gen"
	AxisTestKind    BenchAxis = "test_kind"
	AxisRunID       BenchAxis = "run_id"
	AxisRunName     BenchAxis = "run_name"
	AxisModelLabel  BenchAxis = "model_label"
	AxisQuantLabel  BenchAxis = "quant_label"
	AxisLlamacppTag BenchAxis = "llamacpp_tag"
	AxisBackend     BenchAxis = "llamacpp_backend"
)

// benchAxisSQL is the closed map. `p` is `bench_points`, `res` is
// `bench_results`, `r` is `bench_runs`.
var benchAxisSQL = map[BenchAxis]string{
	AxisNGpuLayers:  "p.n_gpu_layers",
	AxisNBatch:      "p.n_batch",
	AxisNUbatch:     "p.n_ubatch",
	AxisNThreads:    "p.n_threads",
	AxisFlashAttn:   "p.flash_attn",
	AxisTypeK:       "p.type_k",
	AxisTypeV:       "p.type_v",
	AxisSplitMode:   "p.split_mode",
	AxisTensorSplit: "p.tensor_split",
	AxisNDepth:      "res.n_depth",
	AxisNPrompt:     "res.n_prompt",
	AxisNGen:        "res.n_gen",
	AxisTestKind:    "res.test_kind",
	AxisRunID:       "r.id",
	AxisRunName:     "r.name",
	AxisModelLabel:  "r.model_label",
	AxisQuantLabel:  "r.quant_label",
	AxisLlamacppTag: "r.llamacpp_tag",
	AxisBackend:     "r.llamacpp_backend",
}

// BenchAxes lists the axis names a request may use, sorted, for the 422 message
// and for the generated OpenAPI enum.
func BenchAxes() []BenchAxis {
	out := make([]BenchAxis, 0, len(benchAxisSQL))
	for a := range benchAxisSQL {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether a is an axis this package can group, filter or plot on.
func (a BenchAxis) Valid() bool { _, ok := benchAxisSQL[a]; return ok }

// BenchMetric is the measured value a comparison plots.
type BenchMetric string

// The four measured columns of `bench_results`. Throughput is the headline;
// the nanosecond pair is what a reader checks when two throughputs are close.
const (
	MetricAvgTS    BenchMetric = "avg_ts"
	MetricStddevTS BenchMetric = "stddev_ts"
	MetricAvgNS    BenchMetric = "avg_ns"
	MetricStddevNS BenchMetric = "stddev_ns"
)

var benchMetricSQL = map[BenchMetric]string{
	MetricAvgTS:    "res.avg_ts",
	MetricStddevTS: "res.stddev_ts",
	MetricAvgNS:    "res.avg_ns",
	MetricStddevNS: "res.stddev_ns",
}

// BenchMetrics lists the metric names a request may use, sorted.
func BenchMetrics() []BenchMetric {
	out := make([]BenchMetric, 0, len(benchMetricSQL))
	for m := range benchMetricSQL {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Valid reports whether m is one of the four measured columns.
func (m BenchMetric) Valid() bool { _, ok := benchMetricSQL[m]; return ok }

// benchJoin is the join §10 names, restricted to points that actually produced a
// result. A `failed` or `skipped` point has no `bench_results` row, so the inner
// join is also the filter that keeps a comparison honest — a point that did not
// run contributes no number rather than a zero.
const benchJoin = `FROM bench_results res
	JOIN bench_points p ON p.id = res.point_id
	JOIN bench_runs   r ON r.id = res.run_id`

// BenchCompareQuery is `POST /api/v1/bench/compare`'s body, already validated.
type BenchCompareQuery struct {
	// RunIDs restricts the comparison to these runs. Empty compares every run,
	// which is what the history view wants.
	RunIDs []string
	// X is the axis along the horizontal, Series splits the lines, Y is the
	// measured value. Series may be empty for a single line.
	X      BenchAxis
	Series BenchAxis
	Y      BenchMetric
	// Filters are equality constraints keyed by axis — `{"test_kind":"tg"}`
	// being the commonest, since a chart mixing pp and tg throughput is not a
	// chart. Values are compared as TEXT so a caller does not have to know
	// whether the underlying column is an INTEGER.
	Filters map[BenchAxis]string
}

// Validate refuses an axis or metric this package cannot express, before any SQL
// is built. It returns a model.Error so the API layer answers 422 without
// re-deciding.
func (q BenchCompareQuery) Validate() error {
	if !q.X.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("x %q is not a comparable axis", q.X)}
	}
	if q.Series != "" && !q.Series.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("series %q is not a comparable axis", q.Series)}
	}
	if !q.Y.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("y %q is not a measured metric", q.Y)}
	}
	for axis := range q.Filters {
		if !axis.Valid() {
			return model.Error{Code: model.CodeBadFlags,
				Message: fmt.Sprintf("filter %q is not a comparable axis", axis)}
		}
	}
	return nil
}

// BenchComparePoint is one aggregated cell: an x value, a series label, the mean
// of the metric over every matching result, and how many results that mean was
// taken over.
//
// Samples is on the wire because a mean over one sample and a mean over nine are
// different claims, and a chart that hides the difference invites the reader to
// believe the wrong one.
type BenchComparePoint struct {
	X       string
	Series  string
	Value   float64
	Samples int
}

// BenchCompare runs §10's grouped query.
func (s *Store) BenchCompare(ctx context.Context, tx Tx, q BenchCompareQuery) ([]BenchComparePoint, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}

	xExpr := benchAxisSQL[q.X]
	seriesExpr := "''"
	if q.Series != "" {
		seriesExpr = benchAxisSQL[q.Series]
	}
	yExpr := benchMetricSQL[q.Y]

	var (
		clauses []string
		args    []any
	)
	if len(q.RunIDs) > 0 {
		marks := make([]string, len(q.RunIDs))
		for i, id := range q.RunIDs {
			marks[i] = "?"
			args = append(args, id)
		}
		clauses = append(clauses, "res.run_id IN ("+strings.Join(marks, ",")+")")
	}
	// Filters are applied in a stable order so the statement text — and
	// therefore SQLite's statement cache entry — does not depend on Go's map
	// iteration order.
	for _, axis := range sortedAxes(q.Filters) {
		clauses = append(clauses, "CAST("+benchAxisSQL[axis]+" AS TEXT) = ?")
		args = append(args, q.Filters[axis])
	}

	sql := `SELECT CAST(` + xExpr + ` AS TEXT) AS x,
	               CAST(` + seriesExpr + ` AS TEXT) AS series,
	               AVG(` + yExpr + `) AS value,
	               COUNT(*) AS samples
	        ` + benchJoin
	if len(clauses) > 0 {
		sql += " WHERE " + strings.Join(clauses, " AND ")
	}
	// The numeric ORDER BY before the textual one is what puts `-ngl 2` before
	// `-ngl 10` on the x axis: SQLite's CAST of a non-numeric string yields 0,
	// so a textual axis falls through to the lexical comparison unchanged.
	sql += ` GROUP BY x, series ORDER BY series, CAST(x AS REAL), x`

	rows, err := tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("compare bench runs: %w", err)
	}
	defer rows.Close()

	var out []BenchComparePoint
	for rows.Next() {
		var (
			p      BenchComparePoint
			x      *string
			series *string
		)
		if err := rows.Scan(&x, &series, &p.Value, &p.Samples); err != nil {
			return nil, fmt.Errorf("scan a comparison row: %w", err)
		}
		// A NULL axis is a flag the point did not set. It is rendered as the
		// empty string rather than dropped: "threads unset" is a real series and
		// dropping it would silently remove points from a chart.
		if x != nil {
			p.X = *x
		}
		if series != nil {
			p.Series = *series
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BenchSeriesQuery is `GET /api/v1/bench/series` — history across llama.cpp
// versions, which is the same join grouped by run instead of by axis.
type BenchSeriesQuery struct {
	// ModelID restricts to one model. Empty spans every model, which is rarely
	// what a reader wants but is a legal question.
	ModelID string
	// Test is the `bench_results.test_kind` to plot; empty plots all of them.
	Test model.BenchTestKind
	// Metric is the measured value. Empty means avg_ts.
	Metric BenchMetric
	// Group is what each line is labeled by — `llamacpp_tag` for "did the new
	// build get faster", `quant_label` for "which quant is fastest here".
	Group BenchAxis
	// Limit caps the rows; zero means DefaultBenchRunLimit.
	Limit int
}

// Validate refuses an unknown metric, group or test kind.
func (q BenchSeriesQuery) Validate() error {
	if q.Metric != "" && !q.Metric.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("metric %q is not a measured metric", q.Metric)}
	}
	if q.Group != "" && !q.Group.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("group %q is not a comparable axis", q.Group)}
	}
	if q.Test != "" && !q.Test.Valid() {
		return model.Error{Code: model.CodeBadFlags,
			Message: fmt.Sprintf("test %q is not one of pp, tg, pp+tg", q.Test)}
	}
	return nil
}

// BenchSeriesPoint is one point of a history line: when the run happened, what
// it was labeled by, and the mean of the metric over that run.
type BenchSeriesPoint struct {
	RunID   string
	RunName string
	Group   string
	At      int64
	Value   float64
	Samples int
}

// BenchSeries runs the history query, oldest first — the order a time axis
// reads in.
func (s *Store) BenchSeries(ctx context.Context, tx Tx, q BenchSeriesQuery) ([]BenchSeriesPoint, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	metric := q.Metric
	if metric == "" {
		metric = MetricAvgTS
	}
	group := q.Group
	if group == "" {
		group = AxisLlamacppTag
	}

	var (
		clauses []string
		args    []any
	)
	if q.ModelID != "" {
		clauses = append(clauses, "r.model_id = ?")
		args = append(args, q.ModelID)
	}
	if q.Test != "" {
		clauses = append(clauses, "res.test_kind = ?")
		args = append(args, string(q.Test))
	}

	sql := `SELECT r.id, r.name, CAST(` + benchAxisSQL[group] + ` AS TEXT) AS grp,
	               COALESCE(r.finished_at, r.started_at, r.created_at) AS at,
	               AVG(` + benchMetricSQL[metric] + `), COUNT(*)
	        ` + benchJoin
	if len(clauses) > 0 {
		sql += " WHERE " + strings.Join(clauses, " AND ")
	}
	sql += ` GROUP BY r.id, grp ORDER BY at, r.id, grp LIMIT ?`
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultBenchRunLimit
	}
	args = append(args, limit)

	rows, err := tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read the bench history: %w", err)
	}
	defer rows.Close()

	var out []BenchSeriesPoint
	for rows.Next() {
		var (
			p   BenchSeriesPoint
			grp *string
		)
		if err := rows.Scan(&p.RunID, &p.RunName, &grp, &p.At, &p.Value, &p.Samples); err != nil {
			return nil, fmt.Errorf("scan a history row: %w", err)
		}
		if grp != nil {
			p.Group = *grp
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BenchSecondsPerPoint is the duration estimate §10 asks for: "a duration
// estimate from prior runs' MEDIAN seconds-per-point for that model class".
//
// The median rather than the mean, and the design says so, because the
// distribution has a long right tail: one point that swapped, thermally
// throttled or waited behind a download would drag a mean far enough to make the
// estimate useless, and it is the estimate a user decides "do I start this now
// or after lunch" from.
//
// It returns the median and the number of points it was taken over. Zero
// samples means this model has no history and the caller should say so rather
// than present a default as a measurement.
func (s *Store) BenchSecondsPerPoint(ctx context.Context, tx Tx, modelID string) (float64, int, error) {
	var (
		clause string
		args   []any
	)
	if modelID != "" {
		clause = " AND r.model_id = ?"
		args = append(args, modelID)
	}

	// SQLite has no MEDIAN, so the middle is selected by ORDER BY plus a
	// parity-aware window: OFFSET (n-1)/2 with LIMIT 1 for an odd count lands on
	// the middle row, and LIMIT 2 for an even count lands on the two the mean is
	// taken over. `2 - n % 2` is that parity, expressed once.
	rows, err := tx.QueryContext(ctx,
		`WITH d AS (
		   SELECT (p.finished_at - p.started_at) / 1000.0 AS secs
		     FROM bench_points p JOIN bench_runs r ON r.id = p.run_id
		    WHERE p.state = 'succeeded'
		      AND p.started_at IS NOT NULL AND p.finished_at IS NOT NULL
		      AND p.finished_at > p.started_at`+clause+`
		 )
		 SELECT secs, (SELECT COUNT(*) FROM d)
		   FROM d ORDER BY secs
		  LIMIT  (SELECT 2 - COUNT(*) % 2 FROM d)
		 OFFSET (SELECT (COUNT(*) - 1) / 2 FROM d)`, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("read the median seconds per bench point: %w", err)
	}
	defer rows.Close()

	var (
		sum   float64
		taken int
		total int
	)
	for rows.Next() {
		var secs float64
		if err := rows.Scan(&secs, &total); err != nil {
			return 0, 0, fmt.Errorf("scan a bench duration: %w", err)
		}
		sum += secs
		taken++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if taken == 0 {
		return 0, 0, nil
	}
	return sum / float64(taken), total, nil
}

// sortedAxes orders a filter map's keys so the generated statement text is
// stable across calls.
func sortedAxes(m map[BenchAxis]string) []BenchAxis {
	out := make([]BenchAxis, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
