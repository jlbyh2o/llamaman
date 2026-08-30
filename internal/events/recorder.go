package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Appender is the one store method this package depends on, declared here per
// DESIGN §1's "everything with side effects sits behind an interface owned by
// its consumer" rule. *store.Store satisfies it.
type Appender interface {
	AppendEvent(ctx context.Context, tx store.Tx, e model.Event) error
}

// Recorder is the seam DESIGN §1 describes: a service method "performs a
// guarded transition inside one transaction, emits an events row, and
// publishes an SSE frame." Append is the first half and Publish is the second,
// and they are deliberately two calls rather than one: Append belongs inside
// the caller's write transaction (an event that survived a rolled-back write
// would be a record of something that did not happen), while Publish must run
// only after that transaction has committed — a live subscriber that heard
// about a row which then rolled back would be told something happened that
// did not. A Recorder has no way to enforce that ordering itself; every caller
// follows it the same way store.Store.Write's own callers follow "commit,
// then act on it."
type Recorder struct {
	appender Appender
	hub      *Hub
}

// NewRecorder builds a Recorder over the given store and Hub.
func NewRecorder(appender Appender, hub *Hub) *Recorder {
	return &Recorder{appender: appender, hub: hub}
}

// Append writes ev to the events table inside tx. Call it from within the same
// transaction that performs the state transition it describes.
func (r *Recorder) Append(ctx context.Context, tx store.Tx, ev model.Event) error {
	return r.appender.AppendEvent(ctx, tx, ev)
}

// Publish fans ev out on the "events" topic — the generic firehose every
// appended row goes out on regardless of category (§3.14; see Topic's doc
// comment). Call it only after the transaction Append wrote ev in has
// committed.
func (r *Recorder) Publish(ev model.Event) {
	r.hub.Publish(EncodeEventFrame(ev))
}

// eventDTO is the wire shape of an `events` row on the SSE "events" topic. It
// exists so the JSON on the wire is stable and snake_case regardless of how
// internal/model names its Go fields, matching the shape §3.14 shows for a
// frame's `data`: `{"type":…, …}`.
type eventDTO struct {
	Type        string  `json:"type"`
	ID          string  `json:"id"`
	At          int64   `json:"at"`
	Level       string  `json:"level"`
	Category    string  `json:"category"`
	SubjectType *string `json:"subject_type,omitempty"`
	SubjectID   *string `json:"subject_id,omitempty"`
	Action      string  `json:"action"`
	FromState   *string `json:"from_state,omitempty"`
	ToState     *string `json:"to_state,omitempty"`
	Actor       string  `json:"actor"`
	Message     string  `json:"message"`
	DetailJSON  *string `json:"detail_json,omitempty"`
}

// EncodeEventFrame builds the Frame an `events` row is broadcast as. It is
// exported so internal/sse's Last-Event-ID replay (reading rows straight from
// the store rather than from a live Publish) produces byte-identical `data`
// for the same row — a client must not be able to tell a replayed frame from a
// live one.
func EncodeEventFrame(ev model.Event) Frame {
	data, err := json.Marshal(eventDTO{
		Type:        "event." + string(ev.Category),
		ID:          ev.ID,
		At:          ev.At,
		Level:       string(ev.Level),
		Category:    string(ev.Category),
		SubjectType: ev.SubjectType,
		SubjectID:   ev.SubjectID,
		Action:      ev.Action,
		FromState:   ev.FromState,
		ToState:     ev.ToState,
		Actor:       string(ev.Actor),
		Message:     ev.Message,
		DetailJSON:  ev.DetailJSON,
	})
	if err != nil {
		// eventDTO's fields are all strings, *string and int64: json.Marshal
		// cannot fail on it. Encode the failure itself rather than panic, so a
		// future field addition that breaks this invariant degrades instead of
		// taking the publisher down with it.
		data = []byte(fmt.Sprintf(`{"type":"event.encode_error","id":%q}`, ev.ID))
	}
	return Frame{Topic: TopicEvents, ID: ev.ID, Data: data}
}
