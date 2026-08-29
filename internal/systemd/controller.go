package systemd

import (
	"context"
	"time"
)

// JobPath is the D-Bus object path of an enqueued job, returned by the no-wait
// variants so a caller can correlate a job it deliberately did not wait for.
type JobPath string

// JobResult is what systemd reported when a job left the queue — "done",
// "failed", "canceled", "timeout", "dependency", "skipped".
type JobResult string

// UnitProps is the typed snapshot of the unit properties this project reads
// (DESIGN section 5.3).
type UnitProps struct {
	ActiveState           string
	SubState              string
	MainPID               uint32
	ExecMainStatus        int32
	Result                string
	NRestarts             uint32
	MemoryCurrent         uint64
	ExecMainExitTimestamp time.Time
}

// SubStateEvent is one push notification that a unit changed sub-state.
type SubStateEvent struct {
	Unit     string
	SubState string
}

// Controller is the whole of this project's systemd vocabulary (DESIGN section
// 5.3). Two implementations satisfy it: a D-Bus client and an exec fallback over
// systemctl. No package outside internal/systemd may implement or bypass it.
type Controller interface {
	// Start, Stop and Restart block until systemd removes the job, which is
	// the correct behavior for every instance unit.
	Start(ctx context.Context, unit string) (JobResult, error)
	Stop(ctx context.Context, unit string) (JobResult, error)
	Restart(ctx context.Context, unit string) (JobResult, error)

	// StartNoWait and RestartNoWait enqueue the job, return its object path
	// and never wait for JobRemoved. They are mandatory for the two calls
	// whose completion requires this process to die: starting the self-update
	// oneshot, and restarting llamaman.service itself.
	StartNoWait(ctx context.Context, unit string) (JobPath, error)
	RestartNoWait(ctx context.Context, unit string) (JobPath, error)

	// Enable and Disable change unit enablement and reload the manager.
	Enable(ctx context.Context, units []string) error
	Disable(ctx context.Context, units []string) error

	// ResetFailed clears a unit's failed state and its start-limit counter
	// (D93).
	ResetFailed(ctx context.Context, unit string) error

	// Props reads the unit properties above.
	Props(ctx context.Context, unit string) (UnitProps, error)

	// SubscribeSubState delivers sub-state changes for units matching pattern
	// until ctx is done.
	SubscribeSubState(ctx context.Context, pattern string) (<-chan SubStateEvent, error)
}
