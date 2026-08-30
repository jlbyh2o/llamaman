package sse

import (
	"context"
	"io"
	"net/http"

	"github.com/jlbyh2o/llamaman/internal/events"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// Replayer is the persistence Last-Event-ID resume needs (DESIGN §1,
// invariant 1: only internal/store contains SQL, so this package declares the
// repository interface it needs). *store.Store satisfies it structurally —
// Read and EventsAfter are both already exported with exactly this shape
// (internal/store/events.go) — so the composition root wires Config.Replay to
// a live Store with no adapter.
type Replayer interface {
	Read(ctx context.Context, fn func(context.Context, store.Tx) error) error
	EventsAfter(ctx context.Context, tx store.Tx, afterID string, limit int) ([]model.Event, error)
}

// replay reads every `events` row after afterID and writes each as a frame,
// paging through Config.Replay in ReplayPageSize batches until it catches up
// to "now" — a client that was disconnected long enough to miss more than one
// page's worth still gets every row, not just the first page. It returns the
// highest id actually written (== afterID, unchanged, if nothing was missed or
// Replay is nil) and false if the connection should close: a write failure
// (the client went away) or a Replay error (logged; there is nothing left to
// answer with once the 200 and the retry directive are already on the wire).
func (h *Handler) replay(ctx context.Context, w io.Writer, rc *http.ResponseController, afterID string) (lastID string, ok bool) {
	if h.cfg.Replay == nil {
		return afterID, true
	}
	page := h.replayPageSize()
	last := afterID
	for {
		var rows []model.Event
		err := h.cfg.Replay.Read(ctx, func(ctx context.Context, tx store.Tx) error {
			var err error
			rows, err = h.cfg.Replay.EventsAfter(ctx, tx, last, page)
			return err
		})
		if err != nil {
			h.cfg.Logger.Warn("sse: replay read failed", "after", last, "error", err)
			return last, false
		}
		for _, e := range rows {
			if !writeAndFlush(w, rc, encodeFrame(events.EncodeEventFrame(e))) {
				return last, false
			}
			last = e.ID
		}
		if len(rows) < page {
			return last, true
		}
		select {
		case <-ctx.Done():
			return last, false
		default:
		}
	}
}
