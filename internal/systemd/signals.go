package systemd

import (
	"context"
	"fmt"
	"strings"

	godbus "github.com/godbus/dbus/v5"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// The push half of the control channel: a dedicated signal connection that
// reports which unit objects changed.
//
// go-systemd ships a sub-state subscriber of its own and this package
// deliberately does not use it. Two reasons, and the first is disqualifying:
//
//  1. `SetSubStateSubscriber` writes the subscriber under a mutex that the
//     dispatch goroutine does not take when it reads — a data race the race
//     detector reports on any host with a live bus, on the very code path this
//     design depends on most.
//  2. It re-reads every changed unit's properties before it can tell whether the
//     unit is one the caller cares about. Resolving the unit NAME from the
//     object path locally, as this does, skips that round trip for every unit on
//     the host that is not ours — which on a busy system manager is nearly all
//     of them.
//
// systemd's own signal is only a trigger either way: PropertiesChanged on a unit
// does not reliably carry SubState's new value, so the value is read back from
// the unit object.

// unitObjectPrefix is where systemd exposes unit objects. A unit's name is
// escaped into the last path element.
const unitObjectPrefix = "/org/freedesktop/systemd1/unit/"

// signalSource delivers the object paths of units whose properties changed.
type signalSource interface {
	// Paths is closed when the source is done.
	Paths() <-chan godbus.ObjectPath
	Close() error
}

// busSignals is the live implementation.
type busSignals struct {
	conn  *godbus.Conn
	raw   chan *godbus.Signal
	paths chan godbus.ObjectPath
	done  chan struct{}
}

// dialSignals opens a signal connection to the manager for this scope and asks
// systemd to start emitting.
//
// It is a SECOND connection beside the method one, which is what go-systemd does
// internally too: a connection blocked delivering a signal is a connection that
// cannot answer a method call, and the daemon issues both constantly.
func dialSignals(ctx context.Context, scope model.SystemdScope) (signalSource, error) {
	conn, err := dialBus(ctx, scope)
	if err != nil {
		return nil, err
	}

	// Manager.Subscribe is what makes systemd emit PropertiesChanged for units
	// at all; without it the match below would never fire.
	manager := conn.Object("org.freedesktop.systemd1", godbus.ObjectPath("/org/freedesktop/systemd1"))
	if call := manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.Subscribe", 0); call.Err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: Manager.Subscribe: %w", call.Err)
	}

	if err := conn.AddMatchSignalContext(ctx,
		godbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		godbus.WithMatchMember("PropertiesChanged"),
		godbus.WithMatchPathNamespace(godbus.ObjectPath(strings.TrimSuffix(unitObjectPrefix, "/"))),
	); err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: add signal match: %w", err)
	}

	s := &busSignals{
		conn: conn,
		// Buffered because a full signal channel makes godbus DROP signals
		// rather than block its reader, and a dropped transition is a stale
		// instance row until the next poll.
		raw:   make(chan *godbus.Signal, 256),
		paths: make(chan godbus.ObjectPath, 256),
		done:  make(chan struct{}),
	}
	conn.Signal(s.raw)
	go s.pump()
	return s, nil
}

// dialBus opens one authenticated connection to the right bus.
//
// System scope uses the SYSTEM bus, which is polkit-mediated, and never
// systemd's private socket — that one works only as root, which would make the
// whole design appear to work in development and fail as the service identity.
func dialBus(ctx context.Context, scope model.SystemdScope) (*godbus.Conn, error) {
	var conn *godbus.Conn
	var err error
	if scope == model.ScopeUser {
		conn, err = godbus.SessionBusPrivate(godbus.WithContext(ctx))
	} else {
		conn, err = godbus.SystemBusPrivate(godbus.WithContext(ctx))
	}
	if err != nil {
		return nil, fmt.Errorf("systemd: connect to the %s bus: %w", scope, err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: authenticate to the %s bus: %w", scope, err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("systemd: hello on the %s bus: %w", scope, err)
	}
	return conn, nil
}

func (s *busSignals) pump() {
	defer close(s.paths)
	for {
		select {
		case <-s.done:
			return
		case sig, ok := <-s.raw:
			if !ok {
				return
			}
			if sig == nil || sig.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" {
				continue
			}
			if len(sig.Body) == 0 {
				continue
			}
			iface, _ := sig.Body[0].(string)
			if iface != "org.freedesktop.systemd1.Unit" {
				continue
			}
			select {
			case s.paths <- sig.Path:
			case <-s.done:
				return
			default:
				// Dropping is survivable: every consumer of this stream also
				// re-reads properties on reconnect and on its own tick.
			}
		}
	}
}

func (s *busSignals) Paths() <-chan godbus.ObjectPath { return s.paths }

func (s *busSignals) Close() error {
	select {
	case <-s.done:
		return nil
	default:
		close(s.done)
	}
	s.conn.RemoveSignal(s.raw)
	return s.conn.Close()
}

// unitNameFromPath decodes the unit name systemd escaped into an object path.
//
// The escaping is byte-wise: everything outside [A-Za-z0-9] becomes `_` plus two
// lowercase hex digits, so `llamaman-instance@qwen.service` is
// `llamaman_2dinstance_40qwen_2eservice`. Decoding it locally is what lets a
// subscriber be matched BEFORE a properties round trip is spent on a unit that
// belongs to somebody else.
//
// The empty string means the path is not a unit object.
func unitNameFromPath(path godbus.ObjectPath) string {
	p := string(path)
	if !strings.HasPrefix(p, unitObjectPrefix) {
		return ""
	}
	escaped := p[len(unitObjectPrefix):]
	if escaped == "" || strings.Contains(escaped, "/") {
		return ""
	}

	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(escaped) {
			return ""
		}
		hi, ok1 := unhex(escaped[i+1])
		lo, ok2 := unhex(escaped[i+2])
		if !ok1 || !ok2 {
			return ""
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
