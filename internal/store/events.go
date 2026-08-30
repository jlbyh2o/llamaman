package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// events queries (DESIGN section 2.11).
//
// `events` is append-only, and its id is a ULID that doubles as the SSE
// Last-Event-ID cursor (§3.14). That is why there is no update method here and
// why both listings order by id: a client that reconnects with an id gets
// everything after it from a plain `id > ?`, with no second column and no
// timestamp ties to break.

const eventColumns = `id, at, level, category, subject_type, subject_id, action,
	from_state, to_state, actor, message, detail_json`

// AppendEvent writes one event. Every state transition in this design writes
// one, so the caller passes it inside the transaction that made the transition —
// an event that survived a rolled-back write would be a record of something that
// did not happen.
func (s *Store) AppendEvent(ctx context.Context, tx Tx, e model.Event) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (`+eventColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.At, string(e.Level), string(e.Category), e.SubjectType, e.SubjectID,
		e.Action, e.FromState, e.ToState, string(e.Actor), e.Message, e.DetailJSON)
	if err != nil {
		return fmt.Errorf("append event %s: %w", e.ID, err)
	}
	return nil
}

// EventFilter selects rows for GET /api/v1/events/log (§3.14).
type EventFilter struct {
	// Categories, when non-empty, restricts to these categories.
	Categories []model.EventCategory
	// Levels, when non-empty, restricts to these levels.
	Levels []model.EventLevel
	// SubjectType and SubjectID, when both set, read `idx_events_subject`.
	SubjectType string
	SubjectID   string
	// Before is the `?before=` cursor: return only rows with a smaller id, i.e.
	// older ones. Empty means start at the newest.
	Before string
	// Limit caps the result; zero means DefaultEventLimit.
	Limit int
}

// DefaultEventLimit bounds an event listing.
const DefaultEventLimit = 200

// Events lists rows NEWEST FIRST, which is the order the log view reads and the
// direction `?before=` pages in.
func (s *Store) Events(ctx context.Context, tx Tx, f EventFilter) ([]model.Event, error) {
	var (
		where []string
		args  []any
	)
	if len(f.Categories) > 0 {
		ph := make([]string, len(f.Categories))
		for i, c := range f.Categories {
			ph[i] = "?"
			args = append(args, string(c))
		}
		where = append(where, "category IN ("+strings.Join(ph, ",")+")")
	}
	if len(f.Levels) > 0 {
		ph := make([]string, len(f.Levels))
		for i, l := range f.Levels {
			ph[i] = "?"
			args = append(args, string(l))
		}
		where = append(where, "level IN ("+strings.Join(ph, ",")+")")
	}
	if f.SubjectType != "" && f.SubjectID != "" {
		where = append(where, "subject_type = ? AND subject_id = ?")
		args = append(args, f.SubjectType, f.SubjectID)
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}

	q := `SELECT ` + eventColumns + ` FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	return s.queryEvents(ctx, tx, q, args...)
}

// EventsAfter is the SSE replay of §3.14: everything after a client's
// Last-Event-ID, OLDEST FIRST, because a replay is delivered in the order the
// events happened. An empty cursor replays from the beginning of the retained
// window, which is what a client with no Last-Event-ID header gets.
func (s *Store) EventsAfter(ctx context.Context, tx Tx, afterID string, limit int) ([]model.Event, error) {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	return s.queryEvents(ctx, tx,
		`SELECT `+eventColumns+` FROM events WHERE id > ? ORDER BY id LIMIT ?`,
		afterID, limit)
}

func (s *Store) queryEvents(ctx context.Context, tx Tx, q string, args ...any) ([]model.Event, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select events: %w", err)
	}
	defer rows.Close()

	var out []model.Event
	for rows.Next() {
		var (
			e        model.Event
			level    string
			category string
			actor    string
		)
		if err := rows.Scan(&e.ID, &e.At, &level, &category, &e.SubjectType, &e.SubjectID,
			&e.Action, &e.FromState, &e.ToState, &actor, &e.Message, &e.DetailJSON); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Level = model.EventLevel(level)
		e.Category = model.EventCategory(category)
		e.Actor = model.EventActor(actor)
		out = append(out, e)
	}
	return out, rows.Err()
}
