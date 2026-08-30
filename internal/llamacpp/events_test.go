package llamacpp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The invariant internal/sse/handler.go states and depends on: "a replayed row
// and its live-published twin carry the same ULID". Its Last-Event-ID dedup is
// `f.ID <= lastReplayedID` against ids read out of the `events` table, so a
// live frame minted beside its row — a fresh ULID, a different actor — can
// never match, and a client that reconnects renders the same transition twice,
// the second time under an id no table holds.
//
// This test asks the question end to end rather than unit-testing publish: it
// runs a real activation and a real delete through the queue with a subscriber
// attached, then holds every `events`-topic frame up against the table.
func TestEveryPublishedEventFrameIsAStoredRow(t *testing.T) {
	t.Parallel()

	hub := events.NewHub(0)
	t.Cleanup(hub.Close)
	f := newFixture(t, func(c *Config) {
		c.Events = events.NewRecorder(c.Store.(*store.Store), hub)
	})
	sub := hub.Subscribe([]events.Topic{events.TopicEvents})
	t.Cleanup(sub.Close)

	f.registerActivate(&fakeRoller{failAt: -1})
	f.registerDelete()
	f.seedVersion("b10621-cpu-src", model.VersionReady)
	f.seedVersion("b10700-cpu-src", model.VersionReady)
	f.seedVersion("b10800-cpu-src", model.VersionReady)

	f.activate(t, "b10621-cpu-src", RestartNone)
	f.activate(t, "b10700-cpu-src", RestartNone)
	if _, err := f.svc.Delete(context.Background(), "b10800-cpu-src"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.runOne()

	// Every row the daemon appended, keyed by id.
	rows := map[string]model.Event{}
	if err := f.store.Read(context.Background(), func(ctx context.Context, tx store.Tx) error {
		list, err := f.store.Events(ctx, tx, store.EventFilter{
			Categories: []model.EventCategory{model.CategoryLlamacpp},
		})
		if err != nil {
			return err
		}
		for _, ev := range list {
			rows[ev.ID] = ev
		}
		return nil
	}); err != nil {
		t.Fatalf("read the events table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the lifecycle above appended no llamacpp events at all")
	}

	sub.Close()
	published := map[string]bool{}
	for frame := range sub.Frames() {
		var got struct {
			ID      string `json:"id"`
			Action  string `json:"action"`
			Actor   string `json:"actor"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(frame.Data, &got); err != nil {
			t.Fatalf("decode frame %s: %v", frame.ID, err)
		}
		published[got.ID] = true

		row, ok := rows[got.ID]
		if !ok {
			t.Fatalf("frame %s (%s) names an id that exists in no events row; "+
				"internal/sse's Last-Event-ID dedup can never match it",
				got.ID, got.Action)
		}
		if got.Actor != string(row.Actor) || got.Action != row.Action ||
			got.Message != row.Message {
			t.Fatalf("frame %s disagrees with its row: wire has (%s, %s, %q), "+
				"table has (%s, %s, %q)",
				got.ID, got.Action, got.Actor, got.Message,
				row.Action, row.Actor, row.Message)
		}
	}

	// The other half of the same defect: a row written and never published is a
	// transition the Events screen and the dashboard's recent events never hear
	// about until someone reloads.
	for id, row := range rows {
		if !published[id] {
			t.Errorf("the %s row (%s) was appended but never published", row.Action, id)
		}
	}
}
