// Package systemd is the only package that speaks to systemd (DESIGN section 1,
// invariant 2). It defines the Controller interface and its two implementations
// — a native D-Bus client over go-systemd for push-based state and typed
// properties, and an exec fallback over systemctl — plus unit and polkit
// template rendering, the journal reader, and sdnotify readiness and status
// messages (DESIGN section 5).
package systemd

// Pinned here rather than in go.mod alone: this is the one package permitted to
// import go-systemd (DESIGN section 1, invariant 2), and the blank import keeps
// the section-14 module in the build graph until dbusController lands. Delete
// this block when the real import appears.
import (
	_ "github.com/coreos/go-systemd/v22/dbus"

	// Unit and polkit templates live in packaging/ (see its README for why),
	// embedded there and referenced here. Delete this blank import when the
	// real rendering code that reads packaging.Templates lands.
	_ "github.com/jlbyh2o/llamaman/packaging"
)
