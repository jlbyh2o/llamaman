package systemd

import (
	"errors"
	"fmt"
	"strings"

	godbus "github.com/godbus/dbus/v5"
)

// The three failures every caller has to tell apart, and the reason D-Bus is
// preferred over parsing systemctl in the first place: error IDENTITY. On the
// bus a missing unit is `org.freedesktop.systemd1.NoSuchUnit`; over systemctl it
// is exit code 5 plus English prose (DESIGN section 5.3).
var (
	// ErrNoSuchUnit means the manager does not know this unit.
	ErrNoSuchUnit = errors.New("systemd: no such unit")

	// ErrDenied means polkit refused. It is the F9 shape: the control plane
	// degrades to read-only with the `sudo llamaman install-units
	// --repair-polkit` remediation rather than failing lazily on a user's first
	// Start click.
	ErrDenied = errors.New("systemd: authorization denied")

	// ErrUnavailable means there is no control channel at all — no bus, no
	// systemctl. It is not fatal: the daemon starts anyway, into the degraded
	// mode of section 11.1a, and it never spawns llama-server itself.
	ErrUnavailable = errors.New("systemd: control channel unavailable")

	// ErrDisconnected means the bus connection dropped and the supervised
	// reconnect has not yet succeeded. Distinct from ErrUnavailable because it
	// is transient and the caller should retry rather than degrade.
	ErrDisconnected = errors.New("systemd: bus connection lost")
)

// JobResult values, as systemd's JobRemoved signal reports them.
const (
	JobDone       JobResult = "done"
	JobCanceled   JobResult = "canceled"
	JobTimeout    JobResult = "timeout"
	JobFailed     JobResult = "failed"
	JobDependency JobResult = "dependency"
	JobSkipped    JobResult = "skipped"
)

// OK reports whether a job left the queue having done what was asked.
func (r JobResult) OK() bool { return r == JobDone }

// D-Bus error names this package translates. Anything else is passed through
// with its name attached, because an opaque error a human can grep for beats a
// guess.
const (
	dbusNoSuchUnit   = "org.freedesktop.systemd1.NoSuchUnit"
	dbusLoadFailed   = "org.freedesktop.systemd1.LoadFailed"
	dbusAccessDenied = "org.freedesktop.DBus.Error.AccessDenied"
	dbusInteractive  = "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired"
	dbusNotSupported = "org.freedesktop.DBus.Error.NotSupported"
)

// translate maps a godbus error onto this package's vocabulary.
func translate(unit string, err error) error {
	if err == nil {
		return nil
	}
	var de godbus.Error
	if !errors.As(err, &de) {
		return err
	}
	switch de.Name {
	case dbusNoSuchUnit, dbusLoadFailed:
		return fmt.Errorf("%w: %s", ErrNoSuchUnit, unit)
	case dbusAccessDenied, dbusInteractive:
		// InteractiveAuthorizationRequired is polkit saying "a human could
		// approve this at a prompt". There is no human: the daemon calls with
		// no interaction flag, so it is a denial like any other.
		return fmt.Errorf("%w: %s (%s)", ErrDenied, unit, de.Name)
	case dbusNotSupported:
		return fmt.Errorf("%w: %s", ErrUnavailable, de.Name)
	}
	return fmt.Errorf("systemd: %s: %s: %w", unit, de.Name, err)
}

// translateExit maps a systemctl exit status and its stderr onto the same
// vocabulary. systemctl's exit codes are coarse — 5 covers "no such unit" but
// also several other loads — so the prose is consulted as well. Both are
// checked because neither alone is reliable across systemd versions, and the
// alternative (guessing from exit status only) is what makes an exec fallback
// report a denial as a missing unit.
func translateExit(unit string, code int, stderr string, err error) error {
	if err != nil && code == 0 {
		return err
	}
	if code == 0 {
		return nil
	}
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "access denied"),
		strings.Contains(lower, "interactive authentication required"),
		strings.Contains(lower, "authentication required"),
		strings.Contains(lower, "permission denied"):
		return fmt.Errorf("%w: %s: %s", ErrDenied, unit, trimProse(stderr))
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "no such file or directory"),
		code == 5:
		return fmt.Errorf("%w: %s", ErrNoSuchUnit, unit)
	}
	return fmt.Errorf("systemd: %s: systemctl exited %d: %s", unit, code, trimProse(stderr))
}

// trimProse reduces systemctl's multi-line complaint to its first line, which is
// the part worth putting in a notification.
func trimProse(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
