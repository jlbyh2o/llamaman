package app

import (
	"context"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// eventLog satisfies api.EventLogService: the durable read behind
// `GET /api/v1/events/log` (DESIGN section 3.14).
//
// It is a four-line adapter over the store rather than a service because there
// is no domain logic to have: the filter is the query string, the rows are the
// rows, and D49's first invariant puts the SQL in internal/store. The SSE
// endpoint beside it replays from the same table through `Last-Event-ID`, which
// is why the two must never grow separate notions of what an event is.
type eventLog struct{ st *store.Store }

func (e eventLog) Events(ctx context.Context, f store.EventFilter) ([]model.Event, error) {
	var out []model.Event
	err := e.st.Read(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = e.st.Events(ctx, tx, f)
		return err
	})
	return out, err
}
