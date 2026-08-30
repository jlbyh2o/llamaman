// Package netutil handles port selection: walking a range, probing whether a
// port is actually free rather than merely unrecorded, and allocating from the
// reserved range instances draw from (DESIGN sections 1, 2.8 and 11.1 step 7).
//
// Everything here binds. That is the point: the database records which ports this
// daemon INTENDS to use, and only a real listen answers whether a port is
// available on this host right now — another daemon, a stray llama-server, an
// unrelated service. §2.8 calls the DB-side check advisory for exactly that
// reason, and F6 exists because the two answers can disagree between the probe
// and the listen.
package netutil

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// DefaultWindow is §2.8's candidate set for the management listener:
// `[ui.port_desired, +20]`. Twenty is wide enough that a host with a few
// conflicting services still lands somewhere predictable, and narrow enough that
// the port a user was told about is still recognizable.
const DefaultWindow = 20

// ErrExhausted is returned by Walk when every candidate in the window was
// excluded or occupied. §11.1 step 7 answers it by binding an ephemeral port
// rather than refusing to start, and raising `ui_port_exhausted` — a daemon that
// will not start because a port is busy is worse than one on an unexpected port
// that says so.
var ErrExhausted = errors.New("netutil: every candidate port is excluded or in use")

// Listen binds one TCP port on bind, which may be "" (all interfaces), a literal
// address, or "0.0.0.0"/"::".
func Listen(bind string, port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
}

// Free reports whether a port can be bound on bind right now.
//
// It is a real bind-and-close, not a connect probe: a connect that is refused
// says nothing about whether THIS process may bind — a socket held by another
// user, or one bound to a different address in the same wildcard family, refuses
// connections and still cannot be taken. The listener is closed immediately, so
// the answer is advisory the moment it is returned; §2.8 says so, and every
// caller that must be right binds and keeps the listener instead.
func Free(bind string, port int) bool {
	ln, err := Listen(bind, port)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// WalkOptions parameterizes the port walk of §11.1 step 7.
type WalkOptions struct {
	// Bind is the address to bind, `ui.bind` for the management listener.
	Bind string
	// Desired is the first candidate, `ui.port_desired`.
	Desired int
	// Window is how many ports past Desired to try. Zero uses DefaultWindow.
	// The walk tries Desired plus Window more, which is §2.8's `[desired, +20]`
	// read inclusively.
	Window int
	// Excluded reports that a candidate must not be taken even if it is free.
	// This is §11.1 step 7's exclusion set — every non-deleted instance's
	// `public_port` and `internal_port`, and the internal pool — and it is not
	// cosmetic: the gateway listeners do not open until later in the boot, so a
	// bare "next free port" walk could take a port an instance owns and only
	// discover the theft when that instance's listener failed to bind (F6).
	// Nil excludes nothing.
	Excluded func(port int) bool
}

// Walk binds the first candidate in `[Desired, Desired+Window]` that is neither
// excluded nor occupied, and returns the listener and the port it landed on.
//
// It returns ErrExhausted when every candidate is taken. It never returns a
// listener the caller did not ask for: the ephemeral fallback of §11.1 step 7 is
// Ephemeral below, called explicitly, so that "the walk failed" and "we are on a
// port nobody chose" are two facts the caller reports separately — the second one
// raises `ui_port_exhausted`.
func Walk(opts WalkOptions) (net.Listener, int, error) {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	if opts.Desired < 1 || opts.Desired > 65535 {
		return nil, 0, fmt.Errorf("netutil: desired port %d is out of range", opts.Desired)
	}

	var lastErr error
	for p := opts.Desired; p <= opts.Desired+window; p++ {
		if p > 65535 {
			break
		}
		if opts.Excluded != nil && opts.Excluded(p) {
			continue
		}
		ln, err := Listen(opts.Bind, p)
		if err != nil {
			lastErr = err
			continue
		}
		return ln, p, nil
	}
	if lastErr != nil {
		return nil, 0, fmt.Errorf("%w (last error: %v)", ErrExhausted, lastErr)
	}
	return nil, 0, ErrExhausted
}

// Ephemeral binds a kernel-chosen port — §11.1 step 7's "bind an ephemeral port
// rather than refusing to start".
func Ephemeral(bind string) (net.Listener, int, error) {
	ln, err := Listen(bind, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("netutil: bind an ephemeral port on %q: %w", bind, err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("netutil: bound a %T rather than a TCP address", ln.Addr())
	}
	return ln, addr.Port, nil
}

// PortSet is the exclusion set as a value, for the common case where the caller
// has a list of ports rather than a predicate.
type PortSet map[int]struct{}

// NewPortSet builds a set from ports.
func NewPortSet(ports ...int) PortSet {
	s := make(PortSet, len(ports))
	for _, p := range ports {
		s[p] = struct{}{}
	}
	return s
}

// Add adds ports to the set.
func (s PortSet) Add(ports ...int) {
	for _, p := range ports {
		s[p] = struct{}{}
	}
}

// AddRange adds the inclusive range [lo, hi] — how the internal instance pool
// (`instances.internal_port_min` … `_max`) is excluded from the management walk.
func (s PortSet) AddRange(lo, hi int) {
	for p := lo; p <= hi; p++ {
		s[p] = struct{}{}
	}
}

// Contains reports membership, and is the WalkOptions.Excluded predicate.
func (s PortSet) Contains(port int) bool {
	_, ok := s[port]
	return ok
}
