package api

import (
	"net/http"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/sse"
)

// eventsRoute mounts the SSE stream of DESIGN section 3.14 at
// `GET /api/v1/events`.
//
// The division of labor is the one internal/sse's own doc comment states: that
// package owns the handshake, the `retry:` directive, the 20 s `:keepalive`
// comment, the `event:`/`id:`/`data:` framing and Last-Event-ID replay, and it
// "has no notion of who is asking". This route is the who: the session gate in
// front of it is what decides the request is authorized before ServeHTTP is
// ever called.
//
// It is a GET, so the CSRF layer is a pass-through and the stream is never
// blocked on a token a long-lived EventSource cannot refresh. The request-log
// wrapper survives it because middleware's recorder implements Unwrap, which is
// what http.NewResponseController follows to reach the real writer and flush
// each frame.
func (a *API) eventsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/events",
		Auth:        AuthSession,
		OperationID: "streamEvents",
		Summary: "Server-Sent Events. `?topics=` filters the stream; `Last-Event-ID` replays " +
			"the `events` topic from the durable log.",
		Tag:     "events",
		Handler: a.cfg.Events,
		Query: []QueryParam{{
			Name: "topics",
			Description: "Comma-separated topics to subscribe to. Empty means every topic. " +
				"One of: " + strings.Join(eventTopics(), ", ") + ".",
			Enum: eventTopics(),
		}},
		Success: Response{
			Status: http.StatusOK,
			Description: "An event stream of `event: <topic>` / `id: <ulid>` / `data: {…}` frames, " +
				"a `retry: 3000` directive and a 20 s `:keepalive` comment.",
			// No Body: the response is text/event-stream, not JSON, so there
			// is no DTO to reflect over, and the generator documents the media
			// type from the absence of one.
			MediaType: "text/event-stream",
		},
		Errors: []Response{{
			Status:      http.StatusBadRequest,
			Description: "An unrecognized entry in `?topics=`.",
			Codes:       []model.ErrorCode{sse.ErrCodeInvalidTopic},
		}},
	}
}

// eventTopics is the `?topics=` enum, read from internal/events so the document
// cannot grow a second, hand-maintained copy of the list that then drifts from
// the one Hub.Subscribe actually accepts.
func eventTopics() []string {
	ts := events.Topics()
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}
