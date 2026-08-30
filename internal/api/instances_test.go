package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/instances"
	"github.com/jlbyh2o/llamaman/internal/model"
)

// Handler tests for the instance endpoints of DESIGN section 3.10.
//
// The service is stubbed: what this layer owns is the wire contract — which
// query parameters are read, which status each outcome maps to, and that a
// domain error's CODE survives onto the wire, because the SPA branches on those
// codes and the generated TypeScript closes the enum around them.

// stubInstances is a controllable InstanceService.
type stubInstances struct {
	view   instances.View
	views  []instances.View
	result instances.DeleteResult
	err    error

	// The calls the handler made, for the parameter assertions.
	gotIncludeDeleted bool
	gotID             string
	gotCreate         instances.CreateParams
	gotPatch          instances.PatchParams
	gotDelete         instances.DeleteParams
}

func (s *stubInstances) List(_ context.Context, includeDeleted bool) ([]instances.View, error) {
	s.gotIncludeDeleted = includeDeleted
	return s.views, s.err
}

func (s *stubInstances) Get(_ context.Context, id string) (instances.View, error) {
	s.gotID = id
	return s.view, s.err
}

func (s *stubInstances) Create(_ context.Context, p instances.CreateParams) (instances.View, error) {
	s.gotCreate = p
	return s.view, s.err
}

func (s *stubInstances) Patch(_ context.Context, id string, p instances.PatchParams) (instances.View, error) {
	s.gotID, s.gotPatch = id, p
	return s.view, s.err
}

func (s *stubInstances) Delete(_ context.Context, id string, p instances.DeleteParams) (instances.DeleteResult, error) {
	s.gotID, s.gotDelete = id, p
	return s.result, s.err
}

// sampleView is a serving instance carrying two of the four derived flags, so
// the DTO mapping is exercised on a row where they are not all false.
func sampleView() instances.View {
	inst := model.Instance{
		ID:               "01JQWEN",
		Name:             "qwen",
		DisplayName:      ptrTo("Qwen 3 8B"),
		ModelID:          ptrTo("m-qwen"),
		PublicPort:       8081,
		InternalPort:     21001,
		AuthMode:         model.AuthToken,
		RestartPolicy:    model.RestartOnFailure,
		RestartMax:       5,
		RestartWindowSec: 600,
		FlagsJSON:        `{"ctx_size":8192}`,
		ConfigHash:       "hash-new",
		DesiredState:     model.DesiredRunning,
		DraftValidation:  model.DraftDeferred,
		UnitName:         "llamaman-instance@qwen.service",
		Generation:       3,
		CreatedAt:        1_700_000_000_000,
		UpdatedAt:        1_700_000_001_000,
	}
	return instances.View{
		InstanceView: model.InstanceView{
			Instance: inst,
			Status: model.InstanceStatus{
				InstanceID:        inst.ID,
				State:             model.InstanceReady,
				AppliedConfigHash: ptrTo("hash-old"),
				LastChangeAt:      1_700_000_002_000,
				GPUAttribution:    model.AttributionMeasured,
				MainPID:           ptrTo(int64(4242)),
			},
		},
		Flags:           model.FlagSet{CtxSize: ptrTo(8192)},
		Derived:         model.DerivedFlags{RestartRequired: true, DraftUnverified: true},
		ActiveVersionID: "b10621-cuda-src",
	}
}

// sessionAPI builds an API with an authenticated session, so the session and
// CSRF gates let a request through to the handler under test.
func sessionAPI(t *testing.T, svc InstanceService) *API {
	t.Helper()
	return newTestAPI(t, Config{
		Auth: stubAuth{
			complete: true,
			session:  &middleware.Session{ID: "s-1"},
			csrfOK:   true,
		},
		Instances: svc,
	})
}

func do(t *testing.T, a *API, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	// The double-submit pair: the non-HttpOnly cookie and the header. stubAuth
	// verifies them for us; what matters here is that both are present, which
	// is what a real browser sends.
	r.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: "c"})
	r.Header.Set(middleware.HeaderCSRF, "c")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

// errorCode reads the code out of section 3's error envelope.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env model.ErrorEnvelope
	decode(t, w, &env)
	return string(env.Error.Code)
}

func TestListInstances(t *testing.T) {
	svc := &stubInstances{views: []instances.View{sampleView()}}
	a := sessionAPI(t, svc)

	w := do(t, a, http.MethodGet, "/api/v1/instances", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if svc.gotIncludeDeleted {
		t.Error("include_deleted defaulted to true")
	}

	var list List[InstanceDTO]
	decode(t, w, &list)
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("list = %+v, want one item", list)
	}
	if list.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null", *list.NextCursor)
	}

	got := list.Items[0]
	if got.ID != "01JQWEN" || got.Name != "qwen" {
		t.Errorf("identity mismatch: %+v", got)
	}
	// The derived flags travel beside `state`, and the instance is BOTH ready
	// and flagged — which is the whole reason they are not states (§2.8).
	if got.Status.State != string(model.InstanceReady) {
		t.Errorf("state = %q, want ready", got.Status.State)
	}
	if !got.RestartRequired || !got.DraftUnverified {
		t.Errorf("derived flags were not mapped: %+v", got)
	}
	if got.StaleVersion || got.Inhibited || got.InhibitReason != nil {
		t.Errorf("flags that are false came back set: %+v", got)
	}
	// Timestamps are RFC 3339 on the wire, milliseconds in storage.
	if got.CreatedAt != Time(1_700_000_000_000) {
		t.Errorf("created_at = %q, want an RFC 3339 string", got.CreatedAt)
	}
	if got.Flags.CtxSize == nil || *got.Flags.CtxSize != 8192 {
		t.Errorf("flags were not carried: %+v", got.Flags)
	}
}

func TestListInstancesIncludeDeleted(t *testing.T) {
	svc := &stubInstances{}
	a := sessionAPI(t, svc)

	if w := do(t, a, http.MethodGet, "/api/v1/instances?include_deleted=true", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !svc.gotIncludeDeleted {
		t.Error("?include_deleted=true was not read")
	}
}

func TestGetInstanceDetail(t *testing.T) {
	view := sampleView()
	view.Argv = []string{"/v/bin/llama-server", "-m", "/m.gguf", "--port", "21001"}
	view.UnknownFlags = []string{"--brand-new-flag"}
	view.Warnings = []model.Warning{{
		Code:    model.WarnUnknownFlags,
		Message: "this build's --help does not advertise every flag",
	}}
	view.Starts = []model.InstanceStart{{
		ID: "s-1", InstanceID: "01JQWEN", At: 1_700_000_003_000,
		Trigger: model.StartBySafeStart, ConfigHash: "hash-new",
		OverrideJSON: ptrTo(`{"ctx_size":2048}`),
		ArgvJSON:     ptrTo(`["/v/bin/llama-server","-c","2048"]`),
	}}

	svc := &stubInstances{view: view}
	a := sessionAPI(t, svc)

	w := do(t, a, http.MethodGet, "/api/v1/instances/01JQWEN", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if svc.gotID != "01JQWEN" {
		t.Errorf("the handler read the id %q", svc.gotID)
	}

	var detail InstanceDetailDTO
	decode(t, w, &detail)
	if len(detail.Argv) != 5 {
		t.Errorf("argv = %v", detail.Argv)
	}
	if detail.ActiveVersionID != "b10621-cuda-src" {
		t.Errorf("active_version_id = %q", detail.ActiveVersionID)
	}
	if len(detail.Warnings) != 1 || detail.Warnings[0].Code != string(model.WarnUnknownFlags) {
		t.Errorf("warnings = %+v", detail.Warnings)
	}
	if len(detail.Starts) != 1 {
		t.Fatalf("starts = %+v", detail.Starts)
	}
	start := detail.Starts[0]
	if start.Outcome != nil {
		t.Errorf("outcome = %v, want null while the run is in flight (D63)", *start.Outcome)
	}
	// The safe start's patch is shown inline, so "it only works with -ngl 0" is
	// a fact in the history (§3.10b).
	if start.Override == nil || start.Override["ctx_size"] != float64(2048) {
		t.Errorf("override = %v, want the consumed patch", start.Override)
	}
	if len(start.Argv) != 3 {
		t.Errorf("the recorded argv was not decoded: %v", start.Argv)
	}
}

func TestCreateInstance(t *testing.T) {
	view := sampleView()
	view.Warnings = []model.Warning{{
		Code:    model.WarnDraftVocabUnverified,
		Message: "the draft model's vocabulary will be checked later",
	}}
	svc := &stubInstances{view: view}
	a := sessionAPI(t, svc)

	w := do(t, a, http.MethodPost, "/api/v1/instances", `{
		"name":"qwen","model_id":"m-qwen","draft_model_id":"m-draft",
		"auth_mode":"none","restart_policy":"always",
		"flags":{"ctx_size":8192,"n_gpu_layers":{"mode":"auto"}},
		"extra_flags":"--log-colors"
	}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body)
	}

	var resp CreateInstanceResponse
	decode(t, w, &resp)
	if resp.Instance.ID != "01JQWEN" {
		t.Errorf("instance = %+v", resp.Instance)
	}
	// A deferred draft check is a SUCCESSFUL save that owes a check (§3.10a).
	if len(resp.Warnings) != 1 || resp.Warnings[0].Code != string(model.WarnDraftVocabUnverified) {
		t.Errorf("warnings = %+v", resp.Warnings)
	}

	p := svc.gotCreate
	if p.Name != "qwen" || p.ModelID != "m-qwen" {
		t.Errorf("params = %+v", p)
	}
	if p.DraftModelID == nil || *p.DraftModelID != "m-draft" {
		t.Errorf("draft_model_id = %v", p.DraftModelID)
	}
	if p.AuthMode == nil || *p.AuthMode != model.AuthNone {
		t.Errorf("auth_mode = %v", p.AuthMode)
	}
	if p.RestartPolicy == nil || *p.RestartPolicy != model.RestartAlways {
		t.Errorf("restart_policy = %v", p.RestartPolicy)
	}
	if p.Flags.NGpuLayers == nil || p.Flags.NGpuLayers.Mode != model.NGLAuto {
		t.Errorf("flags = %+v", p.Flags)
	}
	if p.ExtraFlags != "--log-colors" {
		t.Errorf("extra_flags = %q", p.ExtraFlags)
	}
	// Omitted ports stay nil, so the service allocates them.
	if p.PublicPort != nil || p.InternalPort != nil {
		t.Errorf("omitted ports came through as %v/%v", p.PublicPort, p.InternalPort)
	}
}

// TestCreateInstanceRefusalsKeepTheirCodes is what the SPA branches on: every
// domain refusal reaches the wire as its own code with the documented status.
func TestCreateInstanceRefusalsKeepTheirCodes(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   model.ErrorCode
	}{
		{"a bad name", model.Error{Code: model.CodeInstanceNameInvalid},
			http.StatusUnprocessableEntity, model.CodeInstanceNameInvalid},
		{"a taken name", model.Error{Code: model.CodeInstanceNameTaken},
			http.StatusConflict, model.CodeInstanceNameTaken},
		{"a port rule", model.Error{Code: model.CodePortUnavailable},
			http.StatusUnprocessableEntity, model.CodePortUnavailable},
		{"a draft mismatch", model.Error{Code: model.CodeDraftVocabMismatch},
			http.StatusUnprocessableEntity, model.CodeDraftVocabMismatch},
		{"ngl auto with a tensor split", model.Error{Code: model.CodeNGLAutoConflict},
			http.StatusUnprocessableEntity, model.CodeNGLAutoConflict},
		{"a forbidden extra flag", model.Error{Code: model.CodeExtraFlagForbidden},
			http.StatusUnprocessableEntity, model.CodeExtraFlagForbidden},
		{"a model that does not exist", model.Error{Code: model.CodeModelMissing},
			http.StatusUnprocessableEntity, model.CodeModelMissing},
		{"a flag value", model.Error{Code: model.CodeBadFlags},
			http.StatusUnprocessableEntity, model.CodeBadFlags},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := sessionAPI(t, &stubInstances{err: tt.err})
			w := do(t, a, http.MethodPost, "/api/v1/instances", `{"name":"qwen","model_id":"m"}`)
			if w.Code != tt.status {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			if got := errorCode(t, w); got != string(tt.code) {
				t.Errorf("code = %q, want %q", got, tt.code)
			}
		})
	}
}

// TestCreateInstanceRejectsUnknownFields is the request-side mirror of D43: a
// client that misspells a field is told so rather than having it ignored.
func TestCreateInstanceRejectsUnknownFields(t *testing.T) {
	a := sessionAPI(t, &stubInstances{})
	w := do(t, a, http.MethodPost, "/api/v1/instances", `{"name":"qwen","pubic_port":8080}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if got := errorCode(t, w); got != string(CodeBadRequest) {
		t.Errorf("code = %q", got)
	}
}

func TestPatchInstance(t *testing.T) {
	svc := &stubInstances{view: sampleView()}
	a := sessionAPI(t, svc)

	w := do(t, a, http.MethodPatch, "/api/v1/instances/01JQWEN",
		`{"generation":3,"display_name":"Qwen","draft_model_id":"","public_port":8090}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}

	p := svc.gotPatch
	if p.Generation != 3 {
		t.Errorf("generation = %d", p.Generation)
	}
	if p.DisplayName == nil || *p.DisplayName == nil || **p.DisplayName != "Qwen" {
		t.Errorf("display_name = %v", p.DisplayName)
	}
	// An empty string CLEARS a nullable reference; the outer pointer says the
	// field was mentioned, the inner nil says what to set it to.
	if p.DraftModelID == nil || *p.DraftModelID != nil {
		t.Errorf(`draft_model_id: "" should detach the draft model, got %v`, p.DraftModelID)
	}
	// An omitted field is left alone.
	if p.Description != nil {
		t.Errorf("an omitted description came through as %v", p.Description)
	}
	if p.PublicPort == nil || *p.PublicPort != 8090 {
		t.Errorf("public_port = %v", p.PublicPort)
	}

	var resp PatchInstanceResponse
	decode(t, w, &resp)
	if !resp.Instance.RestartRequired {
		t.Error("the response does not carry restart_required (§3.10)")
	}
}

// TestPatchInstanceGenerationConflict is the 409 the UI turns into "reload the
// form".
func TestPatchInstanceGenerationConflict(t *testing.T) {
	a := sessionAPI(t, &stubInstances{err: model.Error{
		Code:    model.CodeConflictGeneration,
		Message: "edited by someone else",
		Details: map[string]any{"generation": 4},
	}})

	w := do(t, a, http.MethodPatch, "/api/v1/instances/01JQWEN", `{"generation":3}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body)
	}
	if got := errorCode(t, w); got != string(model.CodeConflictGeneration) {
		t.Errorf("code = %q", got)
	}
}

func TestDeleteInstance(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   instances.DeleteParams
	}{
		{"soft by default", "/api/v1/instances/01JQWEN", instances.DeleteParams{}},
		{"purge", "/api/v1/instances/01JQWEN?purge=true", instances.DeleteParams{Purge: true}},
		{"keep tokens", "/api/v1/instances/01JQWEN?keep_tokens=true",
			instances.DeleteParams{KeepTokens: true}},
		{"an unparsable flag is simply false", "/api/v1/instances/01JQWEN?purge=maybe",
			instances.DeleteParams{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &stubInstances{result: instances.DeleteResult{Purged: tt.want.Purge}}
			a := sessionAPI(t, svc)

			w := do(t, a, http.MethodDelete, tt.target, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
			}
			if svc.gotDelete != tt.want {
				t.Errorf("params = %+v, want %+v", svc.gotDelete, tt.want)
			}
		})
	}
}

// TestDeleteInstanceCarriesTheManualCommand is §3.10c's best-effort rule on the
// wire: the delete succeeded, and the user is handed the one command the daemon
// could not run.
func TestDeleteInstanceCarriesTheManualCommand(t *testing.T) {
	a := sessionAPI(t, &stubInstances{result: instances.DeleteResult{
		Hints: []string{"sudo systemctl disable llamaman-instance@qwen.service"},
	}})

	w := do(t, a, http.MethodDelete, "/api/v1/instances/01JQWEN", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}

	var resp DeleteInstanceResponse
	decode(t, w, &resp)
	if resp.Purged {
		t.Error("a soft delete reported a purge")
	}
	if len(resp.Hints) != 1 || !strings.Contains(resp.Hints[0], "systemctl disable") {
		t.Errorf("hints = %v", resp.Hints)
	}
}

// TestInstanceRoutesNeedASession: every row of section 3.10 carries `session`
// in the Auth column, so an unauthenticated caller gets 401 rather than data.
func TestInstanceRoutesNeedASession(t *testing.T) {
	a := newTestAPI(t, Config{
		Auth:      stubAuth{complete: true},
		Instances: &stubInstances{},
	})

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/instances"},
		{http.MethodPost, "/api/v1/instances"},
		{http.MethodGet, "/api/v1/instances/01J"},
		{http.MethodPatch, "/api/v1/instances/01J"},
		{http.MethodDelete, "/api/v1/instances/01J"},
	} {
		body := ""
		if tc.method == http.MethodPost || tc.method == http.MethodPatch {
			body = "{}"
		}
		if w := do(t, a, tc.method, tc.target, body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.target, w.Code)
		}
	}
}

// TestInstanceRoutesWithoutAService: a documented endpoint whose subsystem is
// missing reports the gap rather than faking an answer.
func TestInstanceRoutesWithoutAService(t *testing.T) {
	a := newTestAPI(t, Config{Auth: stubAuth{
		complete: true,
		session:  &middleware.Session{ID: "s-1"},
		csrfOK:   true,
	}})

	if w := do(t, a, http.MethodGet, "/api/v1/instances", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: %s", w.Code, w.Body)
	}
}

func ptrTo[T any](v T) *T { return &v }
