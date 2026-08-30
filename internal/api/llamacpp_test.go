package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/llamacpp"
	"github.com/jlbyh2o/llamaman/internal/llamacpp/github"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Handler tests for the llama.cpp lifecycle endpoints of DESIGN section 3.5.
//
// What this layer owns is the WIRE CONTRACT, and section 3's conventions are
// most of it: timestamps are RFC 3339 UTC strings, and anything that starts work
// answers `202 {"job_id":"…","subject":{…}}`. Both are things a service cannot
// get wrong on its own and a DTO can, which is why they are asserted here rather
// than inferred from the generated document.

// stubLlamacpp is a controllable LlamacppService.
type stubLlamacpp struct {
	view    llamacpp.View
	views   []llamacpp.View
	install llamacpp.InstallResult
	job     model.Job
	chunk   llamacpp.LogChunk
	tail    []string
	list    llamacpp.ReleaseList
	plan    llamacpp.Plan
	err     error

	gotID      string
	gotChannel model.LlamacppChannel
	gotPlan    llamacpp.PlanRequest
	gotOffset  int64
	gotLimit   int64
}

func (s *stubLlamacpp) List(context.Context) ([]llamacpp.View, error) { return s.views, s.err }

func (s *stubLlamacpp) Get(_ context.Context, id string) (llamacpp.View, error) {
	s.gotID = id
	return s.view, s.err
}

func (s *stubLlamacpp) Active(context.Context) (llamacpp.View, error) { return s.view, s.err }

func (s *stubLlamacpp) Install(context.Context, llamacpp.InstallRequest) (
	llamacpp.InstallResult, error) {
	return s.install, s.err
}

func (s *stubLlamacpp) Cancel(_ context.Context, id string) (model.Job, error) {
	s.gotID = id
	return s.job, s.err
}

func (s *stubLlamacpp) Retry(_ context.Context, id string) (model.Job, error) {
	s.gotID = id
	return s.job, s.err
}

func (s *stubLlamacpp) Activate(_ context.Context, id string, _ llamacpp.ActivateRequest) (
	model.Job, error) {
	s.gotID = id
	return s.job, s.err
}

func (s *stubLlamacpp) Rollback(context.Context, llamacpp.ActivateRequest) (model.Job, error) {
	return s.job, s.err
}

func (s *stubLlamacpp) Delete(_ context.Context, id string) (model.Job, error) {
	s.gotID = id
	return s.job, s.err
}

func (s *stubLlamacpp) Log(_ context.Context, id string, offset, limit int64) (
	llamacpp.LogChunk, error) {
	s.gotID, s.gotOffset, s.gotLimit = id, offset, limit
	return s.chunk, s.err
}

func (s *stubLlamacpp) FollowLog(string) (<-chan string, func(), bool) { return nil, nil, false }

func (s *stubLlamacpp) LogTail(string, int) []string { return s.tail }

func (s *stubLlamacpp) Releases(_ context.Context, ch model.LlamacppChannel) (
	llamacpp.ReleaseList, error) {
	s.gotChannel = ch
	return s.list, s.err
}

func (s *stubLlamacpp) PlanInstall(_ context.Context, req llamacpp.PlanRequest) (
	llamacpp.Plan, error) {
	s.gotPlan = req
	return s.plan, s.err
}

func llamacppAPI(t *testing.T, svc LlamacppService) *API {
	t.Helper()
	return newTestAPI(t, Config{
		Auth: stubAuth{
			complete: true,
			session:  &middleware.Session{ID: "s-1"},
			csrfOK:   true,
		},
		Llamacpp: svc,
	})
}

func sampleVersion() llamacpp.View {
	started := int64(1_700_000_100_000)
	size := int64(412_000_000)
	return llamacpp.View{Version: store.LlamacppVersion{
		ID:          "b10621-cuda-src",
		Channel:     model.ChannelNightly,
		Tag:         "b10621",
		Backend:     model.BackendCUDA,
		Acquisition: model.AcquisitionSource,
		GitURL:      "https://github.com/ggml-org/llama.cpp",
		State:       model.VersionReady,
		IsActive:    true,
		SizeBytes:   &size,
		SupportsFit: true,
		CreatedAt:   1_700_000_000_000,
		StartedAt:   &started,
	}}
}

// TestVersionTimestampsAreRFC3339 is section 3's conventions applied to the one
// DTO that had them wrong: "Timestamps are RFC 3339 UTC strings on the wire …
// the conversion lives in the DTO layer only".
//
// Raw Unix milliseconds on the wire are not a cosmetic difference. They are a
// different JSON TYPE, they are frozen into the generated `api/openapi.json` and
// therefore into `ui/src/api/schema.d.ts`, and a client that renders every other
// timestamp with one code path has to special-case this one forever.
func TestVersionTimestampsAreRFC3339(t *testing.T) {
	t.Parallel()

	svc := &stubLlamacpp{view: sampleVersion()}
	a := llamacppAPI(t, svc)

	w := do(t, a, http.MethodGet, "/api/v1/llamacpp/versions/b10621-cuda-src", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var got LlamacppVersionDetailDTO
	decode(t, w, &got)

	if _, err := time.Parse(time.RFC3339, got.Version.CreatedAt); err != nil {
		t.Errorf("created_at = %q, which is not RFC 3339: %v", got.Version.CreatedAt, err)
	}
	if got.Version.StartedAt == nil {
		t.Fatal("started_at is null on a row that has one")
	}
	if _, err := time.Parse(time.RFC3339, *got.Version.StartedAt); err != nil {
		t.Errorf("started_at = %q, which is not RFC 3339: %v", *got.Version.StartedAt, err)
	}
	// The nullable ones stay null rather than becoming the epoch (F14).
	if got.Version.FinishedAt != nil || got.Version.ActivatedAt != nil {
		t.Errorf("a NULL timestamp became %v / %v", got.Version.FinishedAt, got.Version.ActivatedAt)
	}
}

// TestGitURLIsRedactedOnTheWire: a row written before the git-URL validation
// landed, or one edited into the database by hand, must not publish a credential
// through `git_url` (DESIGN sections 2.2 and 7.1).
func TestGitURLIsRedactedOnTheWire(t *testing.T) {
	t.Parallel()

	const secret = "ghp_TOKENVALUE"
	v := sampleVersion()
	v.Version.GitURL = "https://user:" + secret + "@example.test/me/llama.cpp.git"
	a := llamacppAPI(t, &stubLlamacpp{view: v})

	w := do(t, a, http.MethodGet, "/api/v1/llamacpp/versions/b10621-cuda-src", "")
	if body := w.Body.String(); contains(body, secret) {
		t.Fatalf("the response carries the credential: %s", body)
	}
	var got LlamacppVersionDetailDTO
	decode(t, w, &got)
	if got.Version.GitURL != "https://user:redacted@example.test/me/llama.cpp.git" {
		t.Errorf("git_url = %q", got.Version.GitURL)
	}
}

// TestLongActionsCarryTheSubject is section 3's other convention: "anything that
// starts work returns 202 with {\"job_id\":\"…\",\"subject\":{…}}".
//
// The subject is what the SSE stream is keyed on. A client given only a job id
// has to guess which domain row the frames it receives belong to, which on a
// screen showing three builds is not a guess it can make.
func TestLongActionsCarryTheSubject(t *testing.T) {
	t.Parallel()

	job := model.Job{
		ID: "01JOB", Kind: model.JobLlamacppActivate,
		SubjectType: model.SubjectLlamacppVersion, SubjectID: "b10621-cuda-src",
		State: model.JobQueued,
	}

	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{
			name: "activate", method: http.MethodPost,
			target: "/api/v1/llamacpp/versions/b10621-cuda-src/activate",
			body:   `{"restart_instances":"none"}`,
		},
		{
			name: "rollback", method: http.MethodPost,
			target: "/api/v1/llamacpp/rollback", body: `{}`,
		},
		{
			name: "delete", method: http.MethodDelete,
			target: "/api/v1/llamacpp/versions/b10621-cuda-src",
		},
		{
			name: "cancel", method: http.MethodPost,
			target: "/api/v1/llamacpp/versions/b10621-cuda-src/cancel",
		},
		{
			name: "retry", method: http.MethodPost,
			target: "/api/v1/llamacpp/versions/b10621-cuda-src/retry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := llamacppAPI(t, &stubLlamacpp{job: job})

			w := do(t, a, tc.method, tc.target, tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			var got JobReceiptDTO
			decode(t, w, &got)
			if got.JobID == nil || *got.JobID != "01JOB" {
				t.Errorf("job_id = %v", got.JobID)
			}
			if got.Subject.Type != string(model.SubjectLlamacppVersion) {
				t.Errorf("subject.type = %q", got.Subject.Type)
			}
			if got.Subject.ID != "b10621-cuda-src" {
				t.Errorf("subject.id = %q", got.Subject.ID)
			}
		})
	}
}

// TestInstallResponseCarriesTheSubject: the install POST answers with the same
// receipt, embedded beside the version row.
func TestInstallResponseCarriesTheSubject(t *testing.T) {
	t.Parallel()

	svc := &stubLlamacpp{install: llamacpp.InstallResult{
		Version: sampleVersion().Version,
		Job: model.Job{
			ID: "01JOB", Kind: model.JobLlamacppInstall,
			SubjectType: model.SubjectLlamacppVersion, SubjectID: "b10621-cuda-src",
		},
	}}
	a := llamacppAPI(t, svc)

	w := do(t, a, http.MethodPost, "/api/v1/llamacpp/versions",
		`{"channel":"nightly","tag":"b10621","backend":"cuda"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var got InstallLlamacppResponse
	decode(t, w, &got)
	if got.Subject.ID != "b10621-cuda-src" || got.Subject.Type != "llamacpp_version" {
		t.Errorf("subject = %+v", got.Subject)
	}
	if got.JobID == nil || *got.JobID != "01JOB" {
		t.Errorf("job_id = %v", got.JobID)
	}
}

// TestInstallReuseHasNoJob is D71's third branch on the wire: the id is already
// installed, nothing was queued, and `job_id` is null rather than an empty
// string a client could mistake for something to poll.
func TestInstallReuseHasNoJob(t *testing.T) {
	t.Parallel()

	a := llamacppAPI(t, &stubLlamacpp{install: llamacpp.InstallResult{
		Version: sampleVersion().Version, Reused: true,
	}})

	w := do(t, a, http.MethodPost, "/api/v1/llamacpp/versions", `{"channel":"stable"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a reuse; body %s", w.Code, w.Body)
	}
	var got InstallLlamacppResponse
	decode(t, w, &got)
	if got.JobID != nil {
		t.Errorf("job_id = %v, want null", *got.JobID)
	}
	if !got.Reused {
		t.Error("reused = false")
	}
}

// TestBuildLogServesTextAndJSON is section 3.5's "`?offset=&limit=` plain text".
func TestBuildLogServesTextAndJSON(t *testing.T) {
	t.Parallel()

	svc := &stubLlamacpp{chunk: llamacpp.LogChunk{
		Text:   "[compile] [812/1930] Building CXX object\n",
		Offset: 4096, NextOffset: 4137, Size: 90_000, Live: true,
	}}
	a := llamacppAPI(t, svc)

	t.Run("plain text by default, with the cursor in headers", func(t *testing.T) {
		w := do(t, a, http.MethodGet,
			"/api/v1/llamacpp/versions/b10621-cuda-src/log?offset=4096&limit=8192", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); !contains(ct, "text/plain") {
			t.Errorf("Content-Type = %q", ct)
		}
		if w.Body.String() != svc.chunk.Text {
			t.Errorf("body = %q", w.Body.String())
		}
		if got := w.Header().Get("X-Log-Next-Offset"); got != "4137" {
			t.Errorf("X-Log-Next-Offset = %q", got)
		}
		if svc.gotOffset != 4096 || svc.gotLimit != 8192 {
			t.Errorf("the handler passed offset=%d limit=%d", svc.gotOffset, svc.gotLimit)
		}
	})

	t.Run("JSON when asked for", func(t *testing.T) {
		r := newRequest(http.MethodGet, "/api/v1/llamacpp/versions/b10621-cuda-src/log", "")
		r.Header.Set("Accept", "application/json")
		w := serve(a, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", w.Code, w.Body)
		}
		var got LogPageDTO
		decode(t, w, &got)
		if got.Text != svc.chunk.Text || !got.Live || got.NextOffset != 4137 {
			t.Errorf("body = %+v", got)
		}
	})

	t.Run("an SSE tail replays the ring and closes when nothing is running", func(t *testing.T) {
		svc := &stubLlamacpp{tail: []string{"[fetch] cloning", "[compile] 1/2"}}
		a := llamacppAPI(t, svc)
		r := newRequest(http.MethodGet, "/api/v1/llamacpp/versions/b10621-cuda-src/log", "")
		r.Header.Set("Accept", "text/event-stream")
		w := serve(a, r)

		if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		body := w.Body.String()
		for _, want := range []string{
			"data: [fetch] cloning", "data: [compile] 1/2", "event: end",
		} {
			if !contains(body, want) {
				t.Errorf("the stream is missing %q:\n%s", want, body)
			}
		}
	})
}

// TestReleasesRendersTheChangelogServerSide is D35 on this endpoint: a release
// body is markdown from a public repository, and what reaches the browser
// holding the admin session must carry no script.
func TestReleasesRendersTheChangelogServerSide(t *testing.T) {
	t.Parallel()

	svc := &stubLlamacpp{list: llamacpp.ReleaseList{
		Channel: model.ChannelNightly,
		Releases: []llamacpp.ReleaseView{{
			Tag:          "b10621",
			Name:         "b10621",
			Prerelease:   true,
			PublishedAt:  time.Unix(1_700_000_000, 0).UTC(),
			BodyMarkdown: "# Fixes\n\n<script>alert(1)</script>\n",
			BodyHTML:     "<h1>Fixes</h1>",
			AssetName:    "llama-b10621-bin-ubuntu-x64.tar.gz",
			AssetSize:    41_000_000,
		}},
		RateLimit: github.RateLimit{Remaining: 57, Limit: 60, Known: true},
	}}
	a := llamacppAPI(t, svc)

	w := do(t, a, http.MethodGet, "/api/v1/llamacpp/releases?channel=nightly", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if svc.gotChannel != model.ChannelNightly {
		t.Errorf("the handler asked for channel %q", svc.gotChannel)
	}
	var got ReleaseListDTO
	decode(t, w, &got)
	if len(got.Releases) != 1 {
		t.Fatalf("got %d releases", len(got.Releases))
	}
	r := got.Releases[0]
	if contains(r.BodyHTML, "<script") {
		t.Errorf("the rendered changelog carries a script: %q", r.BodyHTML)
	}
	if r.BodyMarkdown == "" {
		t.Error("the raw markdown is missing; the \"view source\" toggle has nothing to show")
	}
	if _, err := time.Parse(time.RFC3339, r.PublishedAt); err != nil {
		t.Errorf("published_at = %q, which is not RFC 3339", r.PublishedAt)
	}
	if r.AssetName == nil || *r.AssetName == "" {
		t.Error("asset_name is null on a release that publishes one")
	}
	// Section 6.2: "why is the nightly list stale" has an answer on screen.
	if !got.RateLimit.Known || got.RateLimit.Remaining != 57 {
		t.Errorf("rate_limit = %+v", got.RateLimit)
	}
}

// TestPlanReadsItsQuery is section 6.3's endpoint: the decision, with its
// reason, before the user commits.
func TestPlanReadsItsQuery(t *testing.T) {
	t.Parallel()

	svc := &stubLlamacpp{plan: llamacpp.Plan{
		VersionID: "b10621-cuda-src", Acquisition: model.AcquisitionSource,
		Backend: model.BackendCUDA, Channel: model.ChannelNightly, Tag: "b10621",
		Reason:           "no Linux CUDA prebuilt exists, so CUDA is always built from source",
		EstimatedMinutes: 12,
		MissingTools:     []string{"nvcc"},
		CUDAArch:         []string{"89"},
		FreeSpaceOK:      true,
		FreeBytes:        60 << 30,
		RequiredBytes:    12 << 30,
	}}
	a := llamacppAPI(t, svc)

	w := do(t, a, http.MethodGet,
		"/api/v1/llamacpp/plan?channel=nightly&tag=b10621&backend=cuda&force_source=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if svc.gotPlan.Channel != model.ChannelNightly || svc.gotPlan.Tag != "b10621" ||
		svc.gotPlan.Backend != model.BackendCUDA || !svc.gotPlan.ForceSource {
		t.Errorf("the handler passed %+v", svc.gotPlan)
	}
	var got PlanDTO
	decode(t, w, &got)
	if got.Acquisition != "source" || got.Reason == "" {
		t.Errorf("plan = %+v", got)
	}
	if len(got.MissingTools) != 1 || got.MissingTools[0] != "nvcc" {
		t.Errorf("missing_tools = %v", got.MissingTools)
	}
	// The plan says it cannot proceed: nvcc is missing, which is the whole
	// point of answering before a four-minute build.
	if got.CanProceed {
		t.Error("can_proceed = true with a missing tool")
	}
}

// TestLlamacppRefusalsCarryTheirCodes: the SPA branches on these codes and the
// generated TypeScript closes the enum around them, so the code has to survive
// onto the wire with the status section 3.5 pairs it with.
func TestLlamacppRefusalsCarryTheirCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		target string
		body   string
		err    error
		status int
		code   model.ErrorCode
	}{
		{
			name: "cancel with nothing running", method: http.MethodPost,
			target: "/api/v1/llamacpp/versions/b1-cpu-src/cancel",
			err:    model.Error{Code: llamacpp.CodeBuildNotCancelable, Message: "nothing to cancel"},
			status: http.StatusConflict, code: llamacpp.CodeBuildNotCancelable,
		},
		{
			name: "retry of a build that succeeded", method: http.MethodPost,
			target: "/api/v1/llamacpp/versions/b1-cpu-src/retry",
			err:    model.Error{Code: llamacpp.CodeBuildNotRetryable, Message: "nothing to retry"},
			status: http.StatusConflict, code: llamacpp.CodeBuildNotRetryable,
		},
		{
			name: "a plan for a git URL this daemon will not clone", method: http.MethodGet,
			target: "/api/v1/llamacpp/plan?channel=custom&git_url=ext%3A%3Ash",
			err:    model.Error{Code: model.CodeBadFlags, Message: "transport escape"},
			status: http.StatusUnprocessableEntity, code: model.CodeBadFlags,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := llamacppAPI(t, &stubLlamacpp{err: tc.err})
			w := do(t, a, tc.method, tc.target, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", w.Code, tc.status, w.Body)
			}
			if got := errorCode(t, w); got != string(tc.code) {
				t.Errorf("error.code = %q, want %q", got, tc.code)
			}
		})
	}
}
