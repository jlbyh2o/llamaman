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

// Sink is how a service carries the rows it appended inside a transaction out
// to the Publish that follows the commit. It holds no side effects of its own —
// it is a slice with a name — so a service keeps owning its Events interface
// (Append/Publish) and simply uses this to remember what it wrote.
//
// It exists because the row and its frame must be ONE thing. Publishing a
// freshly minted model.Event that merely describes the same transition is not a
// cosmetic difference: internal/sse replays the `events` topic from the table on
// reconnect and then suppresses live frames with `f.ID <= lastReplayedID`, which
// can only work if a live frame carries the ULID its row was stored under. A
// re-minted frame wears an id no table holds, so the comparison never matches,
// the reconnecting client renders the transition twice, and the second copy
// names a row `GET /events/log` will never return. Actor, message, from_state
// and to_state drift the same way — the Events screen shows what the table says
// while the wire says something else.
//
// A Sink is used by one goroutine at a time: the one running the transaction,
// and then the same one publishing after it commits.
type Sink struct {
	evs []model.Event
}

// Add remembers ev. A nil Sink drops it, so a caller with nowhere to publish —
// a boot-time repair, a test — passes nil rather than inventing one.
func (s *Sink) Add(ev model.Event) {
	if s == nil {
		return
	}
	s.evs = append(s.evs, ev)
}

// Drain returns the collected rows in the order they were appended and empties
// the Sink, so a sink that is published twice cannot publish a row twice.
func (s *Sink) Drain() []model.Event {
	if s == nil {
		return nil
	}
	evs := s.evs
	s.evs = nil
	return evs
}

// Len reports how many rows are waiting to be published.
func (s *Sink) Len() int {
	if s == nil {
		return 0
	}
	return len(s.evs)
}

// Publish fans ev out on TWO topics: the generic "events" firehose every
// appended row goes out on regardless of category, and the TYPED topic its
// category corresponds to (§3.14; see Topic's doc comment). Call it only after
// the transaction Append wrote ev in has committed.
//
// The second half is not a convenience. §3.14 lists `instances`, `downloads`,
// `llamacpp` and `bench` as topics precisely so a client can subscribe to the
// part of the system it is looking at, and §4 builds every screen on that: the
// llama.cpp screen listens on `llamacpp`, the instances table on `instances`,
// the downloads queue on `downloads`. Publishing only on `events` left every one
// of those subscriptions permanently silent — a build could move
// `fetching → verifying → failed` with the wizard still reading "Working" until
// the user reloaded, which is indistinguishable from an install that hangs.
//
// A frame published this way carries no `patch`, so a client treats it as a
// signal and re-reads the family (§4). That is the correct default for a state
// transition: the row it names has changed in ways an `events` row does not
// fully describe. PublishPatch is for the cases where the publisher DOES know
// the whole delta.
func (r *Recorder) Publish(ev model.Event) {
	frame := EncodeEventFrame(ev)
	r.hub.Publish(frame)

	if topic, ok := TopicForCategory(ev.Category); ok {
		typed := frame
		typed.Topic = topic
		r.hub.Publish(typed)
	}
}

// PublishPatch fans out §3.14's richer per-entity shape:
// `{"type":"instance.status","id":"…","patch":{…}}` — "patches keyed by entity
// id, so the client merges into its query cache without refetching".
//
// It is separate from Publish because the two carry different promises. Publish
// says "this row changed"; PublishPatch says "this row changed and here is
// exactly how", and a client that merges a patch it was given is a client that
// did not issue a request. Only a publisher holding the complete new value of
// the fields it names may use it — a partial patch presented as complete is a
// cache that is now wrong with no read to correct it.
//
// The id is the ENTITY's, not the event's, because that is what the client keys
// its cache by. It is also what makes the frame safe to drop: unlike an `events`
// frame, a missed patch is recovered by the next full read rather than by
// Last-Event-ID replay, which only replays the `events` topic.
func (r *Recorder) PublishPatch(topic Topic, frameType, id string, patch any) {
	body, err := json.Marshal(struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Patch any    `json:"patch"`
	}{Type: frameType, ID: id, Patch: patch})
	if err != nil {
		// A patch that will not marshal must not become a frame claiming to be
		// one. Dropping it degrades to "the client re-reads eventually", which
		// is the same place a dropped frame lands.
		return
	}
	r.hub.Publish(Frame{Topic: topic, ID: id, Data: body})
}

// TopicForCategory maps an `events.category` onto the SSE topic §3.14 pairs it
// with, reporting false for a category that has no live channel of its own.
//
// Four categories map to a topic of the same name. `token`, `auth`, `update`,
// `system` and `gateway` do not, and that is the design rather than an omission:
// §3.14's topic list is what a SCREEN subscribes to, and those five have no
// screen that updates live from them — they are read from `events/log` when a
// user opens the audit view. Every one of them still travels on the generic
// `events` topic, so nothing is lost.
func TopicForCategory(c model.EventCategory) (Topic, bool) {
	switch c {
	case model.CategoryInstance:
		return TopicInstances, true
	case model.CategoryDownload:
		return TopicDownloads, true
	case model.CategoryLlamacpp:
		return TopicLlamacpp, true
	case model.CategoryBench:
		return TopicBench, true
	default:
		return "", false
	}
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

// PublishSignal fans out a frame that carries no patch: "something in this
// family changed; re-read it".
//
// It is the honest frame for a publisher that knows a row moved but does not
// hold the row's new fields — the job queue telling the llama.cpp screen that a
// build's job advanced, for instance, where the job's columns and the version's
// columns are different sets. §4's reducer treats a patch-less frame exactly
// this way, so the client does one read instead of rendering a merge of fields
// that were never sent.
func (r *Recorder) PublishSignal(topic Topic, frameType, id string) {
	body, err := json.Marshal(struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{Type: frameType, ID: id})
	if err != nil {
		return
	}
	r.hub.Publish(Frame{Topic: topic, ID: id, Data: body})
}
