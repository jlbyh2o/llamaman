package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/bench"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The bench routes of DESIGN section 3.13, at the HTTP boundary: the export's
// three content types, the 503 a build without the subsystem answers with, and
// the two refusals whose status this layer chooses rather than the service.

// stubBench answers whatever a test needs and records nothing else.
type stubBench struct {
	export  bench.Export
	err     error
	created bench.CreateResult
}

func (s stubBench) List(context.Context, store.BenchRunFilter) ([]store.BenchRun, error) {
	return nil, s.err
}
func (s stubBench) Get(context.Context, string) (bench.View, error) { return bench.View{}, s.err }
func (s stubBench) Create(context.Context, bench.CreateRequest) (bench.CreateResult, error) {
	return s.created, s.err
}
func (s stubBench) Start(context.Context, string) (model.Job, error)  { return model.Job{}, s.err }
func (s stubBench) Cancel(context.Context, string) (model.Job, error) { return model.Job{}, s.err }
func (s stubBench) Annotate(context.Context, string, string, *string) (store.BenchRun, error) {
	return store.BenchRun{}, s.err
}
func (s stubBench) Delete(context.Context, string) error { return s.err }
func (s stubBench) Results(context.Context, string) ([]bench.ResultRow, error) {
	return nil, s.err
}
func (s stubBench) Export(context.Context, string, bench.Format) (bench.Export, error) {
	return s.export, s.err
}
func (s stubBench) Preflight(context.Context, bench.PreflightRequest) (bench.Preflight, error) {
	return bench.Preflight{}, s.err
}
func (s stubBench) Compare(context.Context, store.BenchCompareQuery) ([]store.BenchComparePoint, error) {
	return nil, s.err
}
func (s stubBench) Series(context.Context, store.BenchSeriesQuery) ([]store.BenchSeriesPoint, error) {
	return nil, s.err
}

func benchAPI(t *testing.T, svc BenchService) *API {
	t.Helper()
	return newTestAPI(t, Config{
		Auth:  stubAuth{complete: true, session: &middleware.Session{ID: "s"}, csrfOK: true},
		Bench: svc,
	})
}

// TestBenchExportContentTypes: each format is served as its own registered media
// type with a `Content-Disposition` filename, which is what makes a browser save
// the file with a name the user can find again.
func TestBenchExportContentTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format   bench.Format
		body     string
		wantType string
		wantName string
	}{
		{bench.FormatJSON, `{"format":"llamaman.bench"}`, "application/json", "ngl-sweep.json"},
		{bench.FormatCSV, "run_id,run_name\n", "text/csv", "ngl-sweep.csv"},
		{bench.FormatMarkdown, "# ngl sweep\n", "text/markdown", "ngl-sweep.md"},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			a := benchAPI(t, stubBench{export: bench.Export{
				Format:    tt.format,
				MediaType: tt.format.MediaType(),
				Filename:  tt.wantName,
				Body:      []byte(tt.body),
			}})

			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				BasePath+"/bench/runs/01RUN/export?format="+string(tt.format), nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.wantType) {
				t.Errorf("Content-Type = %q, want %s", got, tt.wantType)
			}
			if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, tt.wantName) {
				t.Errorf("Content-Disposition = %q, want the filename %s", got, tt.wantName)
			}
			if rec.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.body)
			}
		})
	}
}

// TestBenchRoutesWithoutTheSubsystem: a documented endpoint a build cannot serve
// answers 503 rather than disappearing. A document that promised an endpoint the
// binary does not serve would fail the D43 conformance suite.
func TestBenchRoutesWithoutTheSubsystem(t *testing.T) {
	t.Parallel()

	a := benchAPI(t, nil)
	for _, path := range []string{
		BasePath + "/bench/runs",
		BasePath + "/bench/preflight?model_id=01M",
		BasePath + "/bench/runs/01RUN",
		BasePath + "/bench/runs/01RUN/results",
		BasePath + "/bench/runs/01RUN/export",
		BasePath + "/bench/series",
	} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s answered %d, want 503", path, rec.Code)
		}
	}
}

// TestBenchGPUConflictIsA409 pins the status section 3.13 gives the guard's
// refusal, and that `details.instances` reaches the client — the whole point of
// naming them is that the user knows what to stop.
func TestBenchGPUConflictIsA409(t *testing.T) {
	t.Parallel()

	a := benchAPI(t, stubBench{err: model.Error{
		Code:    bench.CodeBenchGPUConflict,
		Message: "1 instance(s) are loaded on the GPUs this benchmark would use",
		Details: map[string]any{"instances": []map[string]any{{"name": "busy"}}},
	}})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, newRequest(http.MethodPost, BasePath+"/bench/runs", `{"model_id":"01M"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body: %v", err)
	}
	if env.Error.Code != string(bench.CodeBenchGPUConflict) {
		t.Errorf("code = %q, want %s", env.Error.Code, bench.CodeBenchGPUConflict)
	}
	if env.Error.Details["instances"] == nil {
		t.Error("the 409 does not name the conflicting instances")
	}
}

// TestBenchSweepTooLargeIsA422: the sweep parsed and what it named is the
// problem, which is what 422 means throughout this API.
func TestBenchSweepTooLargeIsA422(t *testing.T) {
	t.Parallel()

	a := benchAPI(t, stubBench{err: model.Error{
		Code: bench.CodeSweepTooLarge, Message: "this sweep expands to 4096 points",
	}})

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, newRequest(http.MethodPost, BasePath+"/bench/runs",
		`{"model_id":"01M","sweep":{"n_batch":"1,2,3"}}`))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
}

// TestBenchCreateStatuses: `201` for a draft, `202` for a queued sweep, `200`
// for an Idempotency-Key replay. The three are different answers and the UI
// branches on them.
func TestBenchCreateStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		res  bench.CreateResult
		want int
	}{
		{
			name: "queued",
			body: `{"model_id":"01M"}`,
			res: bench.CreateResult{
				Run: store.BenchRun{ID: "01RUN", State: model.BenchQueued},
				Job: model.Job{ID: "01JOB", SubjectType: model.SubjectBenchRun, SubjectID: "01RUN"},
			},
			want: http.StatusAccepted,
		},
		{
			name: "draft",
			body: `{"model_id":"01M","draft":true}`,
			res: bench.CreateResult{
				Run: store.BenchRun{ID: "01RUN", State: model.BenchDraft},
			},
			want: http.StatusCreated,
		},
		{
			name: "replay",
			body: `{"model_id":"01M"}`,
			res: bench.CreateResult{
				Run:      store.BenchRun{ID: "01RUN", State: model.BenchQueued},
				Job:      model.Job{ID: "01JOB", SubjectType: model.SubjectBenchRun, SubjectID: "01RUN"},
				Replayed: true,
			},
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := benchAPI(t, stubBench{created: tt.res})

			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, newRequest(http.MethodPost, BasePath+"/bench/runs", tt.body))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body.String())
			}
			var out CreateBenchRunResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body: %v", err)
			}
			// The subject is the RUN in every case, so a draft with no job still
			// names the row the client should watch.
			if out.Subject.ID != "01RUN" || out.Subject.Type != string(model.SubjectBenchRun) {
				t.Errorf("subject = %+v, want the bench run", out.Subject)
			}
			if tt.res.Job.ID == "" && out.JobID != nil {
				t.Errorf("job_id = %v for a draft, want null", *out.JobID)
			}
		})
	}
}
