package systemd

import (
	"context"
	"fmt"

	godbus "github.com/godbus/dbus/v5"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The two polkit actions this design touches, and the one detail that can be
// scoped (DESIGN section 5.2).
const (
	ActionManageUnits     = "org.freedesktop.systemd1.manage-units"
	ActionManageUnitFiles = "org.freedesktop.systemd1.manage-unit-files"
	ActionReloadDaemon    = "org.freedesktop.systemd1.reload-daemon"
)

// PolkitResult is what the boot self-test learned. Both booleans map onto a
// runtime_info column and onto a field of GET /system/capabilities.
type PolkitResult struct {
	// ManageUnits is runtime_info.polkit_ok: may this identity start, stop,
	// restart and reset-failed its own units? A negative answer raises a
	// blocking notification with the exact `sudo llamaman install-units
	// --repair-polkit` remediation and degrades the control plane to read-only
	// — rather than failing lazily on the user's first Start click (F9).
	ManageUnits bool

	// ManageUnitFiles is runtime_info.polkit_unit_files, surfaced as
	// `autostart_control`. A host installed with --no-autostart-grant shows
	// autostart as unavailable from the first page load instead of erroring on
	// the first toggle.
	ManageUnitFiles bool

	// Detail is runtime_info.polkit_detail: a short human-readable note about
	// how the answer was reached, including which polkit file format the host
	// is using when install-units had to guess.
	Detail string
}

// CheckPolkit is the boot self-test of DESIGN section 5.2: two
// CheckAuthorization calls for THIS process, made before anything a user clicks
// depends on the answer.
//
// applicable is false in the D2 user-scope topology, where NEITHER call is made:
// there is no polkit rule there at all, a user manager authorizes its owner
// unconditionally, and both runtime_info columns stay NULL meaning "not
// applicable" rather than "denied". GET /system/capabilities reports
// instance_control and autostart_control true from the scope alone.
func CheckPolkit(ctx context.Context, scope model.SystemdScope) (res PolkitResult, applicable bool, err error) {
	if scope == model.ScopeUser {
		return PolkitResult{}, false, nil
	}
	auth, err := dialAuthority(ctx)
	if err != nil {
		return PolkitResult{}, true, err
	}
	defer auth.Close()
	return checkPolkit(ctx, auth)
}

// authority is the slice of polkit's Authority interface this package uses,
// extracted so the branch logic can be tested without a bus.
type authority interface {
	// CheckAuthorization asks whether this process may perform actionID with
	// these details. It never requests interactive authentication: there is no
	// human at the other end of a system service.
	CheckAuthorization(ctx context.Context, actionID string, details map[string]string) (bool, error)
	Close() error
}

func checkPolkit(ctx context.Context, auth authority) (PolkitResult, bool, error) {
	var res PolkitResult

	// The manage-units probe carries the `unit` detail, because the rule is
	// name-scoped and fails closed on an undefined unit — probing without the
	// detail would exercise a branch that is deliberately denied and report a
	// working host as broken. llamaman-instances.target is the right subject:
	// it is in the granted set and starting it is harmless.
	units, err := auth.CheckAuthorization(ctx, ActionManageUnits,
		map[string]string{"unit": UnitInstancesTgt})
	if err != nil {
		return res, true, fmt.Errorf("systemd: polkit check for %s: %w", ActionManageUnits, err)
	}
	res.ManageUnits = units

	// manage-unit-files is authorized BUS-WIDE with no `unit` detail — systemd
	// gives polkit nothing to scope it by, which is why the rule states plainly
	// what it grants instead of pretending to narrow it.
	files, err := auth.CheckAuthorization(ctx, ActionManageUnitFiles, nil)
	if err != nil {
		return res, true, fmt.Errorf("systemd: polkit check for %s: %w", ActionManageUnitFiles, err)
	}
	res.ManageUnitFiles = files

	switch {
	case res.ManageUnits && res.ManageUnitFiles:
		res.Detail = "manage-units and manage-unit-files granted"
	case res.ManageUnits:
		res.Detail = "manage-units granted; manage-unit-files withheld (autostart is read-only)"
	default:
		res.Detail = "manage-units denied; the control plane is read-only"
	}
	return res, true, nil
}

// busAuthority is the live polkit client.
type busAuthority struct {
	conn *godbus.Conn
	obj  godbus.BusObject
	name string
}

func dialAuthority(ctx context.Context) (authority, error) {
	conn, err := godbus.SystemBusPrivate(godbus.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("systemd: connect to the system bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: authenticate to the system bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: hello on the system bus: %w", err)
	}
	names := conn.Names()
	if len(names) == 0 {
		conn.Close()
		return nil, fmt.Errorf("systemd: the system bus assigned no unique name")
	}
	return &busAuthority{
		conn: conn,
		obj: conn.Object("org.freedesktop.PolicyKit1",
			godbus.ObjectPath("/org/freedesktop/PolicyKit1/Authority")),
		name: names[0],
	}, nil
}

func (a *busAuthority) Close() error { return a.conn.Close() }

// polkitSubject is polkit's (kind, details) subject struct.
type polkitSubject struct {
	Kind    string
	Details map[string]godbus.Variant
}

func (a *busAuthority) CheckAuthorization(ctx context.Context, actionID string, details map[string]string) (bool, error) {
	// The subject is this connection's unique bus name rather than
	// ("unix-process", pid, start-time). Both are legal; the bus name is not
	// subject to pid reuse, and polkit resolves it back to this very process
	// through the bus daemon rather than through a /proc read that can race.
	subject := polkitSubject{
		Kind:    "system-bus-name",
		Details: map[string]godbus.Variant{"name": godbus.MakeVariant(a.name)},
	}
	if details == nil {
		details = map[string]string{}
	}

	// Flags 0 means "do not allow user interaction". There is no human at a
	// service's terminal, and a challenge that could never be answered would
	// hang the boot sequence rather than report a denial.
	const noInteraction = uint32(0)

	// CheckAuthorization returns ONE value, the (bba{ss}) AuthorizationResult
	// struct, so it is stored into one destination rather than three.
	var out struct {
		IsAuthorized bool
		IsChallenge  bool
		Details      map[string]string
	}
	call := a.obj.CallWithContext(ctx,
		"org.freedesktop.PolicyKit1.Authority.CheckAuthorization", 0,
		subject, actionID, details, noInteraction, "")
	if call.Err != nil {
		return false, call.Err
	}
	if err := call.Store(&out); err != nil {
		return false, err
	}
	// A challenge is not an authorization: nothing in this design can answer
	// one, so treating it as a yes would report a host as working and then fail
	// on the user's first click, which is the exact failure this self-test
	// exists to prevent.
	return out.IsAuthorized && !out.IsChallenge, nil
}
