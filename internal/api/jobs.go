package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/jobs"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The job and event-log rows of DESIGN section 3.14.
//
//	GET  /api/v1/jobs                 ?state=active
//	GET  /api/v1/jobs/{id}            state, progress, error
//	POST /api/v1/jobs/{id}/cancel     202, with two documented cut-offs
//	GET  /api/v1/events/log           ?category=&subject_id=&before=&limit=
//
// The cancel is the row that carries protocol rather than plumbing, and it is
// the reason the other three cannot simply be deferred: **two kinds carry a
// cut-off rather than a blanket accept**, and both refuse through the same
// mechanism (jobs.CancelGuard).
//
//	llamacpp_activate   cancelable only before its step-3 transaction commits
//	self_update         cancelable only before the `staged` commit — at or after
//	                    it, `409 selfupdate_not_cancelable`, because from that
//	                    instant the marker is on disk and the swap is a unit
//	                    systemd owns, and nothing downstream reads
//	                    `cancel_requested` (D96, section 12.1 step 5)
//
// internal/selfupdate implements that refusal and registers it as a CancelGuard,
// and model.SelfUpdateState.Cancelable() implements the pre-`staged` window — but
// until this file existed no HTTP route could reach either, so neither the
// ACCEPTED cancel of section 12.1 step 5 nor its documented refusal was
// exercisable at all. D96's cut-off had no reachable surface.

// The two codes these routes own, declared beside the code path that returns
// them, which is the precedent internal/sse set with `invalid_topic`.
const (
	// CodeJobNotCancelable is the 409 for a cancel on a job that is already
	// terminal. It is DISTINCT from `selfupdate_not_cancelable`: that one means
	// "this kind has a cut-off and you are past it", which sends a reader to
	// section 12.1 step 5, while this one means "there is nothing left to
	// cancel".
	CodeJobNotCancelable model.ErrorCode = "job_not_cancelable"
	// CodeQueryInvalid is the 422 for a query parameter that names something
	// outside its enum. It is 422 rather than 400 for the reason section 3 gives
	// throughout: the request parsed, and what it named is the problem.
	CodeQueryInvalid model.ErrorCode = "query_invalid"
)

// JobService is everything this layer needs from the job queue. The consumer
// owns the interface (DESIGN section 1); *jobs.Queue satisfies it.
type JobService interface {
	// List returns rows newest first. `?state=active` is expressed by the
	// caller passing model.LiveJobStates().
	List(ctx context.Context, f store.JobFilter) ([]model.Job, error)
	// Job returns one row, or store.ErrNotFound.
	Job(ctx context.Context, id string) (model.Job, error)
	// Cancel requests cancellation. It is where the two cut-offs above are
	// enforced, so it may answer a domain error the handler passes straight
	// through.
	Cancel(ctx context.Context, id string) (model.Job, error)
}

// EventLogService reads the durable `events` log for `GET /api/v1/events/log`.
// It is separate from the SSE stream, which is a transport and replays from the
// same table through Last-Event-ID.
type EventLogService interface {
	Events(ctx context.Context, f store.EventFilter) ([]model.Event, error)
}

// JobDTO is one `jobs` row on the wire (section 2.3).
//
// The scheduling record and the domain record are deliberately separate in the
// schema, and they are separate here too: this carries the lease, the retry
// count and the outcome, and names the domain row through `subject` rather than
// embedding any of it.
type JobDTO struct {
	ID      string     `json:"id"`
	Kind    string     `json:"kind"`
	Subject SubjectDTO `json:"subject"`
	State   string     `json:"state"`

	Priority    int `json:"priority"`
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`

	// RunAfter is the backoff instant: a retried job is `queued` and not
	// runnable until this passes.
	RunAfter string `json:"run_after"`
	// LeaseExpiresAt is null for a row no boot holds.
	LeaseExpiresAt *string `json:"lease_expires_at"`
	// CancelRequested is the flag a worker polls. It is reported because a
	// cancel that has been ACCEPTED but not yet observed is a real state the UI
	// has to be able to render as "canceling".
	CancelRequested bool `json:"cancel_requested"`

	// Progress is the worker's own progress blob, passed through as it was
	// written. Its shape is per-kind and is documented by the kind, not here.
	Progress map[string]any `json:"progress"`
	// ErrorCode and ErrorMessage: section 2.3a puts the CODE on the job and the
	// message on the domain row, so a UI that wants prose reads both.
	ErrorCode    *string `json:"error_code"`
	ErrorMessage *string `json:"error_message"`

	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// EventDTO is one `events` row (section 2.11). Its id is a ULID and doubles as
// the SSE `Last-Event-ID` cursor, which is why the log pages on it.
type EventDTO struct {
	ID       string  `json:"id"`
	At       string  `json:"at"`
	Level    string  `json:"level"`
	Category string  `json:"category"`
	Action   string  `json:"action"`
	Actor    string  `json:"actor"`
	Message  string  `json:"message"`
	Subject  *string `json:"subject_id"`
	// SubjectType is null for an event about nothing in particular.
	SubjectType *string `json:"subject_type"`
	FromState   *string `json:"from_state"`
	ToState     *string `json:"to_state"`
	// Detail is the row's `detail_json`, passed through as written.
	Detail map[string]any `json:"detail"`
}

func (a *API) jobRoutes() []Route {
	return []Route{
		a.listJobsRoute(),
		a.getJobRoute(),
		a.cancelJobRoute(),
		a.eventLogRoute(),
	}
}

func (a *API) listJobsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/jobs",
		Auth:        AuthSession,
		OperationID: "listJobs",
		Summary:     "The job queue, newest first. `?state=active` is the live set.",
		Tag:         "jobs",
		Query: []QueryParam{
			{
				Name: "state",
				Description: "`active` restricts to the live states — queued, leased, running, " +
					"paused and interrupted — which is the set that holds a subject against " +
					"the one-live-job-per-subject index. Any other job state name restricts " +
					"to it exactly.",
			},
			{Name: "kind", Description: "Restrict to one job kind."},
			{Name: "limit", Description: "Maximum rows to return."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.jobService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			f, err := jobFilterFrom(r)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			rows, err := svc.List(r.Context(), f)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			items := make([]JobDTO, 0, len(rows))
			for _, j := range rows {
				items = append(items, jobDTO(j))
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The matching jobs.",
			Body:        List[JobDTO]{},
		},
		Errors: []Response{{
			Status:      http.StatusUnprocessableEntity,
			Description: "`?state=` or `?kind=` named something that is not a member of its enum.",
			Codes:       []model.ErrorCode{CodeQueryInvalid},
		}},
	}
}

// jobFilterFrom reads the query string.
//
// `?state=` and `?kind=` are validated and answer 422, rather than being
// silently ignored the way `?limit=` is: they CHANGE WHAT IS RETURNED, and a
// typo that quietly widens a filter to "everything" is worse than an error
// (section 3's rule, stated at queryInt64).
func jobFilterFrom(r *http.Request) (store.JobFilter, error) {
	f := store.JobFilter{Limit: int(queryInt64(r, "limit", 0))}

	switch state := strings.TrimSpace(r.URL.Query().Get("state")); state {
	case "":
	case "active":
		f.States = model.LiveJobStates()
	default:
		s := model.JobState(state)
		if !s.Valid() {
			return f, unprocessable("?state= must be `active` or a job state, got %s", state)
		}
		f.States = []model.JobState{s}
	}

	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		k := model.JobKind(kind)
		if !k.Valid() {
			return f, unprocessable("?kind= is not a job kind: %s", kind)
		}
		f.Kinds = []model.JobKind{k}
	}
	return f, nil
}

func (a *API) getJobRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/jobs/{id}",
		Auth:        AuthSession,
		OperationID: "getJob",
		Summary:     "One job: state, progress, error.",
		Tag:         "jobs",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.jobService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			job, err := svc.Job(r.Context(), r.PathValue("id"))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusOK, jobDTO(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The job.",
			Body:        JobDTO{},
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No job has this id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

func (a *API) cancelJobRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/jobs/{id}/cancel",
		Auth:        AuthSession,
		OperationID: "cancelJob",
		Summary: "Request cancellation. Two kinds carry a cut-off rather than a blanket " +
			"accept: `llamacpp_activate` before its step-3 commit, and `self_update` " +
			"before the `staged` commit.",
		Tag: "jobs",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.jobService()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			// The cut-offs are enforced by the queue's CancelGuard, not here:
			// only the worker that owns a kind knows where its point of no
			// return is, and duplicating "is this still cancelable?" in the
			// transport is how the two answers come to disagree.
			job, err := svc.Cancel(r.Context(), r.PathValue("id"))
			if err != nil {
				// A guard's own refusal is a model.Error and carries its code
				// through untouched; the queue's "already terminal" is a plain
				// sentinel and is given one here, so the two are distinguishable
				// on the wire instead of collapsing into a 500.
				if errors.Is(err, jobs.ErrNotCancelable) {
					err = Conflict(CodeJobNotCancelable,
						"this job has already finished, so there is nothing to cancel")
				}
				WriteError(w, r, a.log, err)
				return
			}
			if err := WriteJSON(w, http.StatusAccepted, jobDTO(job)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status: http.StatusAccepted,
			Description: "The cancel was requested. A job that is not running moves to " +
				"`canceled` immediately; a running one stops at its next checkpoint.",
			Body: JobDTO{},
		},
		Errors: []Response{
			{
				Status:      http.StatusNotFound,
				Description: "No job has this id.",
				Codes:       []model.ErrorCode{CodeNotFound},
			},
			{
				Status: http.StatusConflict,
				Description: "The job is past its cut-off. A `self_update` at or after the " +
					"`staged` commit answers `selfupdate_not_cancelable`: the marker is on " +
					"disk and the swap belongs to the service manager (D96).",
				Codes: []model.ErrorCode{
					model.CodeSelfUpdateNotCancelable,
					CodeJobNotCancelable,
				},
			},
		},
	}
}

func (a *API) eventLogRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/events/log",
		Auth:        AuthSession,
		OperationID: "listEvents",
		Summary:     "The durable event log, newest first, paged backwards on the ULID cursor.",
		Tag:         "events",
		Query: []QueryParam{
			{
				Name:        "category",
				Description: "Restrict to one category.",
				Enum:        eventCategories(),
			},
			{Name: "subject_id", Description: "Restrict to one subject's history."},
			{
				Name: "before",
				Description: "Return only rows older than this event id. It is the last " +
					"item's ULID from the previous page — ids sort by creation, which is " +
					"what makes keyset paging possible here at all.",
			},
			{Name: "limit", Description: "Maximum rows to return."},
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.eventLog()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			q := r.URL.Query()
			f := store.EventFilter{
				SubjectID: strings.TrimSpace(q.Get("subject_id")),
				Before:    strings.TrimSpace(q.Get("before")),
				Limit:     int(queryInt64(r, "limit", 0)),
			}
			if c := strings.TrimSpace(q.Get("category")); c != "" {
				cat := model.EventCategory(c)
				if !cat.Valid() {
					WriteError(w, r, a.log,
						unprocessable("?category= is not an event category: %s", c))
					return
				}
				f.Categories = []model.EventCategory{cat}
			}

			rows, err := svc.Events(r.Context(), f)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}

			items := make([]EventDTO, 0, len(rows))
			for _, e := range rows {
				items = append(items, eventDTO(e))
			}
			// The cursor is the OLDEST id on this page, because the log reads
			// newest first and `?before=` pages backwards from it.
			var next *string
			if n := len(items); n > 0 && (f.Limit == 0 || n >= f.Limit) {
				next = ptrOf(items[n-1].ID)
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), next)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The matching events, newest first.",
			Body:        List[EventDTO]{},
		},
		Errors: []Response{{
			Status:      http.StatusUnprocessableEntity,
			Description: "`?category=` named something that is not an event category.",
			Codes:       []model.ErrorCode{CodeQueryInvalid},
		}},
	}
}

// eventCategories is the `?category=` enum, read from internal/model so the
// document cannot grow a second, hand-maintained copy of the list.
func eventCategories() []string {
	cs := model.EventCategoryValues()
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// jobService and eventLog are the nil checks these routes share. A daemon built
// without a queue answers 503 rather than dropping the routes: they are
// documented endpoints, and a build gap is reported, never faked.
func (a *API) jobService() (JobService, error) {
	if a.cfg.Jobs == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without a job queue")
	}
	return a.cfg.Jobs, nil
}

func (a *API) eventLog() (EventLogService, error) {
	if a.cfg.EventLog == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without an event log")
	}
	return a.cfg.EventLog, nil
}

func jobDTO(j model.Job) JobDTO {
	return JobDTO{
		ID:              j.ID,
		Kind:            string(j.Kind),
		Subject:         SubjectDTO{Type: string(j.SubjectType), ID: j.SubjectID},
		State:           string(j.State),
		Priority:        j.Priority,
		Attempts:        j.Attempts,
		MaxAttempts:     j.MaxAttempts,
		RunAfter:        Time(j.RunAfter),
		LeaseExpiresAt:  TimePtr(j.LeaseExpiresAt),
		CancelRequested: j.CancelRequested,
		Progress:        decodeJSONObject(j.ProgressJSON),
		ErrorCode:       j.ErrorCode,
		ErrorMessage:    j.ErrorMessage,
		CreatedAt:       Time(j.CreatedAt),
		StartedAt:       TimePtr(j.StartedAt),
		FinishedAt:      TimePtr(j.FinishedAt),
	}
}

func eventDTO(e model.Event) EventDTO {
	return EventDTO{
		ID:          e.ID,
		At:          Time(e.At),
		Level:       string(e.Level),
		Category:    string(e.Category),
		Action:      e.Action,
		Actor:       string(e.Actor),
		Message:     e.Message,
		Subject:     e.SubjectID,
		SubjectType: e.SubjectType,
		FromState:   e.FromState,
		ToState:     e.ToState,
		Detail:      decodeJSONObject(e.DetailJSON),
	}
}

// unprocessable is the 422 these routes return for a query parameter outside
// its enum.
func unprocessable(format string, args ...any) *Error {
	return Errorf(http.StatusUnprocessableEntity, CodeQueryInvalid, format, args...)
}

// decodeJSONObject renders a nullable JSON column as a map, so a client reads an
// object rather than a string it has to parse a second time.
//
// A column that will not parse becomes null rather than an error: `progress_json`
// and `detail_json` are written by workers as free-form blobs, and refusing to
// serve a job listing because one row's progress is malformed would turn a
// worker's bug into a blank screen.
func decodeJSONObject(raw *string) map[string]any {
	if raw == nil || *raw == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}
