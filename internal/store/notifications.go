package store

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// `notifications` reads (DESIGN section 2.11, section 3.3).
//
// The write half — AppendNotification — lives in selfupdates.go beside the gate
// that was its first caller. The read half is here because it belongs to a
// different consumer: `GET /api/v1/system/notifications` and the dashboard feed
// behind it, which section 3.3 specifies as "undismissed notifications with
// remediation actions".
//
// Dismissal is a stamp rather than a delete, and that is what makes the
// retention sweep of section 2.11 ("`notifications` dismissed + 30 days")
// meaningful: a card the user cleared is still a record of something the daemon
// had to say, for the month a support question about it might arrive in.

// NotificationFilter narrows a Notifications read.
type NotificationFilter struct {
	// IncludeDismissed admits rows with a `dismissed_at`. The default is the
	// endpoint's default: section 3.3 asks for undismissed rows, because the
	// list is a work queue rather than a log.
	IncludeDismissed bool
	// Limit caps the rows returned. Zero means DefaultNotificationLimit.
	Limit int
}

// DefaultNotificationLimit is how many rows an unbounded read returns. The table
// is the small one beside `events` — things that need a human — so this is a
// guard against a pathological host rather than a paging boundary.
const DefaultNotificationLimit = 200

const notificationColumns = `id, at, severity, code, title, body,
	subject_type, subject_id, action_json, dismissed_at`

// Notifications returns rows newest first.
func (s *Store) Notifications(ctx context.Context, tx Tx, f NotificationFilter) ([]Notification, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultNotificationLimit
	}
	where := "WHERE dismissed_at IS NULL"
	if f.IncludeDismissed {
		where = ""
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+notificationColumns+` FROM notifications `+where+`
		 ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		var (
			n        Notification
			severity string
		)
		if err := rows.Scan(&n.ID, &n.At, &severity, &n.Code, &n.Title, &n.Body,
			&n.SubjectType, &n.SubjectID, &n.ActionJSON, &n.DismissedAt); err != nil {
			return nil, fmt.Errorf("list notifications: %w", err)
		}
		n.Severity = model.NotificationSeverity(severity)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	return out, nil
}

// Notification returns one row, or ErrNotFound.
func (s *Store) Notification(ctx context.Context, tx Tx, id string) (Notification, error) {
	var (
		n        Notification
		severity string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE id = ?`, id).
		Scan(&n.ID, &n.At, &severity, &n.Code, &n.Title, &n.Body,
			&n.SubjectType, &n.SubjectID, &n.ActionJSON, &n.DismissedAt)
	if err != nil {
		return Notification{}, notFound(err)
	}
	n.Severity = model.NotificationSeverity(severity)
	return n, nil
}

// DismissNotification stamps `dismissed_at`, reporting whether a row moved.
//
// A second dismissal of the same row reports false rather than failing: the
// endpoint is idempotent by design, because the button that calls it sits on a
// card that an SSE frame may have already removed from another tab.
func (s *Store) DismissNotification(ctx context.Context, tx Tx, id string, at int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE notifications SET dismissed_at = ? WHERE id = ? AND dismissed_at IS NULL`, at, id)
	if err != nil {
		return false, fmt.Errorf("dismiss notification %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("dismiss notification %s: %w", id, err)
	}
	return n > 0, nil
}
