package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The job and event-log rows of DESIGN section 3.14, at the HTTP boundary.
//
// The row that matters most here is the cancel, and the reason is D96: a
// `self_update` is cancelable only BEFORE the `staged` commit, and at or after
// it `POST /jobs/{id}/cancel` answers `409 selfupdate_not_cancelable` "because
// the marker is on disk and the swap is a unit systemd owns, and nothing
// downstream reads `cancel_requested`".
//
// Both halves of that rule were implemented and NEITHER was reachable:
// selfupdate.Service.CheckCancel returned the refusal and was registered as a
// jobs.CancelGuard, model.SelfUpdateState.Cancelable() described the window, and
// no HTTP route existed to call either. A cut-off with no surface is a cut-off
// nothing can be wrong about, which is why these tests drive the route rather
// than the guard.

// stubJobs answers whatever a test needs and records the last id it was given.
type stubJobs struct {
	list       []model.Job
	job        model.Job
	err        error
	cancelErr  error
	lastFilter store.JobFilter
	lastID     string
}

func (s *stubJobs) List(_ context.Context, f store.JobFilter) ([]model.Job, error) {
	s.lastFilter = f
	return s.list, s.err
}

func (s *stubJobs) Job(_ context.Context, id string) (model.Job, error) {
	s.lastID = id
	return s.job, s.err
}

func (s *stubJobs) Cancel(_ context.Context, id string) (model.Job, error) {
	s.lastID = id
	if s.cancelErr != nil {
		return model.Job{}, s.cancelErr
	}
	return s.job, nil
}

// stubEvents is the event-log half.
type stubEvents struct {
	rows       []model.Event
	err        error
	lastFilter store.EventFilter
}

func (s *stubEvents) Events(_ context.Context, f store.EventFilter) ([]model.Event, error) {
	s.lastFilter = f
	return s.rows, s.err
}

// postCancel issues the cancel with the CSRF header a state-changing request
// carries. GETs skip it: the CSRF layer is a pass-through for safe methods.
func postCancel(t *testing.T, a *API, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, BasePath+"/jobs/"+id+"/cancel", nil)
	// The double-submit pair a real browser sends: the non-HttpOnly cookie and
	// the header. stubAuth verifies them; what matters here is that both are
	// present.
	r.AddCookie(&http.Cookie{Name: middleware.CookieCSRF, Value: "c"})
	r.Header.Set(middleware.HeaderCSRF, "c")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, r)
	return rec
}

func jobsAPI(t *testing.T, j JobService, e EventLogService) *API {
	t.Helper()
	return newTestAPI(t, Config{
		Auth:     stubAuth{complete: true, session: &middleware.Session{ID: "s"}, csrfOK: true},
		Jobs:     j,
		EventLog: e,
	})
}

// TestCancelSelfUpdatePastTheCutOff is D96, end to end through the route that
// was missing.
//
// The guard's refusal is a model.Error carrying `selfupdate_not_cancelable`, and
// it has to arrive as a 409 with that code — not as a 500, and not flattened
// into the queue's generic "already finished". The difference is what tells an
// operator that the swap is now systemd's business rather than that they
// mistyped an id.
func TestCancelSelfUpdatePastTheCutOff(t *testing.T) {
	t.Parallel()

	a := jobsAPI(t, &stubJobs{cancelErr: model.Error{
		Code: model.CodeSelfUpdateNotCancelable,
		Message: "this update is swapping: the marker is on disk and the swap belongs to " +
			"the service manager",
	}}, nil)

	rec := postCancel(t, a, "01JOB")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(model.CodeSelfUpdateNotCancelable) {
		t.Errorf("code = %q, want %q", got, model.CodeSelfUpdateNotCancelable)
	}
}

// TestCancelBeforeTheCutOffIsAccepted is the other half of D96: up to the
// `staged` commit the cancel is honored, and the route answers 202 with the job
// as the queue left it.
func TestCancelBeforeTheCutOffIsAccepted(t *testing.T) {
	t.Parallel()

	svc := &stubJobs{job: model.Job{
		ID: "01JOB", Kind: model.JobSelfUpdate,
		SubjectType: model.SubjectSelfUpdate, SubjectID: "01UPD",
		State: model.JobCanceled, CancelRequested: true,
	}}
	a := jobsAPI(t, svc, nil)

	rec := postCancel(t, a, "01JOB")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if svc.lastID != "01JOB" {
		t.Errorf("the handler canceled %q, want 01JOB", svc.lastID)
	}
	var got JobDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the receipt: %v", err)
	}
	if got.State != string(model.JobCanceled) || !got.CancelRequested {
		t.Errorf("receipt = %+v, want a canceled job with cancel_requested", got)
	}
	if got.Subject.Type != string(model.SubjectSelfUpdate) || got.Subject.ID != "01UPD" {
		t.Errorf("subject = %+v, want the self_updates row it names", got.Subject)
	}
}

// TestCancelAFinishedJob: the queue's own sentinel is a plain error, so without
// a translation it would reach the client as a 500. It gets its own code so
// "there is nothing left to cancel" stays distinguishable from "this kind has a
// cut-off and you are past it".
func TestCancelAFinishedJob(t *testing.T) {
	t.Parallel()

	a := jobsAPI(t, &stubJobs{cancelErr: jobs.ErrNotCancelable}, nil)

	rec := postCancel(t, a, "01JOB")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec); got != string(CodeJobNotCancelable) {
		t.Errorf("code = %q, want %q", got, CodeJobNotCancelable)
	}
}

// TestCancelAMissingJob: store.ErrNotFound is a domain answer and 404 is what it
// means on the wire.
func TestCancelAMissingJob(t *testing.T) {
	t.Parallel()

	a := jobsAPI(t, &stubJobs{cancelErr: store.ErrNotFound}, nil)

	rec := postCancel(t, a, "01JOB")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

// TestListJobsStateFilter: `?state=active` is the live set, a named state is
// that state, and anything else is a 422 rather than a silently wider listing.
func TestListJobsStateFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantStates []model.JobState
	}{
		{"no filter", "", http.StatusOK, nil},
		{"active is the live set", "?state=active", http.StatusOK, model.LiveJobStates()},
		{"a named state", "?state=failed", http.StatusOK, []model.JobState{model.JobFailed}},
		{"not a state at all", "?state=sideways", http.StatusUnprocessableEntity, nil},
		{"not a kind at all", "?kind=knitting", http.StatusUnprocessableEntity, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := &stubJobs{}
			a := jobsAPI(t, svc, nil)

			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BasePath+"/jobs"+tt.query, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				if got := errorCode(t, rec); got != string(CodeQueryInvalid) {
					t.Errorf("code = %q, want %q", got, CodeQueryInvalid)
				}
				return
			}
			if len(svc.lastFilter.States) != len(tt.wantStates) {
				t.Fatalf("states = %v, want %v", svc.lastFilter.States, tt.wantStates)
			}
			for i, want := range tt.wantStates {
				if svc.lastFilter.States[i] != want {
					t.Errorf("states[%d] = %q, want %q", i, svc.lastFilter.States[i], want)
				}
			}
		})
	}
}

// TestEventLogFilter: the four parameters section 3.14 names reach the store,
// and a category outside the enum is a 422.
func TestEventLogFilter(t *testing.T) {
	t.Parallel()

	svc := &stubEvents{rows: []model.Event{{
		ID: "01EV2", At: 1788012345678, Level: model.LevelWarn,
		Category: model.CategoryUpdate, Action: "update.not_applied",
		Actor: model.ActorSystem, Message: "the update did not take",
	}}}
	a := jobsAPI(t, nil, svc)

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		BasePath+"/events/log?category=update&subject_id=01UPD&before=01EV9&limit=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	f := svc.lastFilter
	if len(f.Categories) != 1 || f.Categories[0] != model.CategoryUpdate {
		t.Errorf("categories = %v, want [update]", f.Categories)
	}
	if f.SubjectID != "01UPD" || f.Before != "01EV9" || f.Limit != 1 {
		t.Errorf("filter = %+v, want subject 01UPD before 01EV9 limit 1", f)
	}

	var page List[EventDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the page: %v", err)
	}
	// A full page carries the cursor for the next one, which is the OLDEST id on
	// it: the log reads newest first and `?before=` pages backwards.
	if page.NextCursor == nil || *page.NextCursor != "01EV2" {
		t.Errorf("next_cursor = %v, want 01EV2", page.NextCursor)
	}

	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		BasePath+"/events/log?category=sideways", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown category answered %d, want 422", rec.Code)
	}
}

// TestJobRoutesWithoutTheSubsystem: a documented endpoint a build cannot serve
// answers 503 rather than disappearing (D43).
func TestJobRoutesWithoutTheSubsystem(t *testing.T) {
	t.Parallel()

	a := jobsAPI(t, nil, nil)
	for _, path := range []string{
		BasePath + "/jobs",
		BasePath + "/jobs/01JOB",
		BasePath + "/events/log",
	} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s answered %d, want 503", path, rec.Code)
		}
	}

	rec := postCancel(t, a, "01JOB")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST cancel answered %d, want 503", rec.Code)
	}
}

// Compile-time proof that the two stubs satisfy the interfaces the composition
// root fills with *jobs.Queue and the store adapter.
var (
	_ JobService      = (*stubJobs)(nil)
	_ EventLogService = (*stubEvents)(nil)
)
