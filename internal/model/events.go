package model

// Events and notifications (DESIGN section 2.11).
//
// Every state transition in this design writes an `events` row, and the row's id
// is a ULID that doubles as the SSE `Last-Event-ID` cursor — which is why events
// are appended rather than updated and why the id is sortable by creation.
// `notifications` is the much smaller table beside it: things that need a human.

// EventLevel is `events.level` (§2.11).
type EventLevel string

const (
	LevelDebug EventLevel = "debug"
	LevelInfo  EventLevel = "info"
	LevelWarn  EventLevel = "warn"
	LevelError EventLevel = "error"
)

// EventLevelValues lists the members of the `events.level` CHECK constraint, in order.
func EventLevelValues() []EventLevel {
	return []EventLevel{LevelDebug, LevelInfo, LevelWarn, LevelError}
}

// Valid reports whether l is a member of the CHECK constraint.
func (l EventLevel) Valid() bool { return valid(l, EventLevelValues()) }

// EventCategory is `events.category` (§2.11). The column is documented by a
// comment rather than closed by a CHECK — a new subsystem must be able to log
// before it can migrate — so this type is the application's own closed set and
// is deliberately absent from ClosedEnums. It is also the topic axis the SSE
// endpoint routes on (§3.14).
type EventCategory string

const (
	CategoryLlamacpp EventCategory = "llamacpp"
	CategoryModel    EventCategory = "model"
	CategoryDownload EventCategory = "download"
	CategoryInstance EventCategory = "instance"
	CategoryToken    EventCategory = "token"
	CategoryBench    EventCategory = "bench"
	CategoryAuth     EventCategory = "auth"
	CategoryUpdate   EventCategory = "update"
	CategorySystem   EventCategory = "system"
	CategoryGateway  EventCategory = "gateway"
)

// EventCategoryValues lists the categories the design names, in the order of the
// column's comment.
func EventCategoryValues() []EventCategory {
	return []EventCategory{
		CategoryLlamacpp, CategoryModel, CategoryDownload, CategoryInstance,
		CategoryToken, CategoryBench, CategoryAuth, CategoryUpdate,
		CategorySystem, CategoryGateway,
	}
}

// Valid reports whether c is one of the categories the design names.
func (c EventCategory) Valid() bool { return valid(c, EventCategoryValues()) }

// EventActor is `events.actor` (§2.11): who caused this. `systemd` is a real
// actor and not a synonym for `system` — an external `systemctl start` is a
// thing a human did through another interface, and the instance history says so.
type EventActor string

const (
	ActorAdmin   EventActor = "admin"
	ActorSystem  EventActor = "system"
	ActorSystemd EventActor = "systemd"
	ActorWizard  EventActor = "wizard"
	ActorCLI     EventActor = "cli"
)

// EventActorValues lists the members of the `events.actor` CHECK constraint, in order.
func EventActorValues() []EventActor {
	return []EventActor{ActorAdmin, ActorSystem, ActorSystemd, ActorWizard, ActorCLI}
}

// Valid reports whether a is a member of the CHECK constraint.
func (a EventActor) Valid() bool { return valid(a, EventActorValues()) }

// Event is one row of `events` (§2.11). ID is a ULID and is also the SSE cursor,
// so rows are never rewritten: an event is a fact about an instant.
type Event struct {
	ID          string
	At          int64
	Level       EventLevel
	Category    EventCategory
	SubjectType *string
	SubjectID   *string
	Action      string
	FromState   *string
	ToState     *string
	Actor       EventActor
	Message     string
	DetailJSON  *string
}

// NotificationSeverity is `notifications.severity` (§2.11).
type NotificationSeverity string

const (
	SeverityInfo  NotificationSeverity = "info"
	SeverityWarn  NotificationSeverity = "warn"
	SeverityError NotificationSeverity = "error"
)

// NotificationSeverityValues lists the members of the `notifications.severity`
// CHECK constraint, in order.
func NotificationSeverityValues() []NotificationSeverity {
	return []NotificationSeverity{SeverityInfo, SeverityWarn, SeverityError}
}

// Valid reports whether s is a member of the CHECK constraint.
func (s NotificationSeverity) Valid() bool { return valid(s, NotificationSeverityValues()) }
