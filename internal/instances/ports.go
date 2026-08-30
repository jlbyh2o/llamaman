package instances

import (
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
)

// Port rules and allocation (DESIGN section 2.8's port-rule table).
//
// `UNIQUE` and the 1024-65535 `CHECK` are the floor, not the contract. Both
// ports are validated here on `POST /instances` and on every `PATCH` that
// touches them, and a violation is `422 port_unavailable` with
// `{"port":N,"reason":…}` — an input error at save time rather than F6's
// runtime bind banner.
//
// The live bind probe is ADVISORY: another process can take the port between
// the probe and the listen, which is exactly why F6 still exists as a runtime
// fallback. It is here anyway because the alternative — trusting the table — is
// what lets a stray llama-server or an unrelated service turn a save into a
// start failure the user cannot explain.

// PortKind selects which half of section 2.8's table applies.
type PortKind string

const (
	// PortPublic is `instances.public_port`, the gateway listener the outside
	// world connects to.
	PortPublic PortKind = "public"
	// PortInternal is `instances.internal_port`, the loopback port
	// llama-server binds and the gateway proxies to.
	PortInternal PortKind = "internal"
)

// Valid reports whether k is one of the two kinds `?kind=` accepts.
func (k PortKind) Valid() bool { return k == PortPublic || k == PortInternal }

// DefaultPublicPortBase is where `GET /ports/suggest?kind=public` starts
// walking. The design's own examples use 8081 for the first instance's public
// port; starting one below it makes the first suggestion 8080 and every later
// one predictable.
const DefaultPublicPortBase = 8080

// PortScanLimit bounds a suggestion walk. A public walk could otherwise probe
// tens of thousands of ports, one bind syscall each, while an admin waits on a
// form. Exhausting it is reported honestly rather than retried forever.
const PortScanLimit = 2048

// PortPolicy is the environment section 2.8's rules are evaluated against: the
// two management-port facts, the internal pool and the gateway's bind address.
// It is a value rather than a settings lookup so the rules stay testable
// without a database.
type PortPolicy struct {
	// UIPortDesired is `ui.port_desired`.
	UIPortDesired int
	// UIPort is `runtime_info.ui_port` — the port the walk of section 11.1 step
	// 7 ACTUALLY landed on, which is not necessarily the desired one. Both are
	// excluded, because either could be where the management UI is reachable.
	UIPort int
	// InternalMin and InternalMax are `instances.internal_port_min`/`_max`.
	InternalMin int
	InternalMax int
	// GatewayBind is `gateway.bind`, the address a public port must bind on.
	GatewayBind string
}

// PortHolder is one instance's claim on both of its ports. The set of these is
// every NON-DELETED instance: soft deletion frees the name and both ports the
// instant `deleted_at` is stamped (D68), so a deleted instance holds nothing.
//
// It is an alias rather than a second struct because the store returns exactly
// this shape and the port rules consume exactly this shape; two identical types
// would only buy a conversion loop that could drift.
type PortHolder = model.InstancePorts

// Prober is the advisory live bind check. netutil.Free satisfies it; a test
// supplies its own so the rule table can be exercised without binding anything.
type Prober func(bind string, port int) bool

// ValidatePort applies section 2.8's table for one port.
//
// excludeID is the instance being edited, whose own current claim must not
// count as a conflict with itself — without it, a PATCH that changes nothing
// about the ports would fail its own validation.
func ValidatePort(
	kind PortKind,
	port int,
	policy PortPolicy,
	holders []PortHolder,
	excludeID string,
	probe Prober,
) error {
	bind := bindFor(kind, policy)

	if port < 1024 || port > 65535 {
		return portError(port, model.PortBindFailed,
			fmt.Sprintf("port %d is outside the 1024-65535 range an unprivileged "+
				"daemon can bind", port))
	}

	if kind == PortPublic {
		if port == policy.UIPortDesired || (policy.UIPort != 0 && port == policy.UIPort) {
			return portError(port, model.PortReservedManagement,
				fmt.Sprintf("port %d is the management UI's", port))
		}
		if inPool(port, policy) {
			return portError(port, model.PortReservedInternalPool,
				fmt.Sprintf("port %d is inside the internal pool [%d, %d] instances draw from",
					port, policy.InternalMin, policy.InternalMax))
		}
	} else if !inPool(port, policy) {
		return portError(port, model.PortOutsideInternalPool,
			fmt.Sprintf("an internal port must be inside the pool [%d, %d]",
				policy.InternalMin, policy.InternalMax))
	}

	if h, taken := holderOf(port, holders, excludeID); taken {
		return model.Error{
			Code:    model.CodePortUnavailable,
			Message: fmt.Sprintf("port %d is already held by the instance %q", port, h.Name),
			Details: map[string]any{
				"port":        port,
				"reason":      string(model.PortInUseByInstance),
				"instance_id": h.InstanceID,
				"instance":    h.Name,
			},
		}
	}

	if probe != nil && !probe(bind, port) {
		return portError(port, model.PortBindFailed,
			fmt.Sprintf("port %d could not be bound on %s right now", port, bind))
	}
	return nil
}

// SuggestPort is `GET /ports/suggest`, and it applies exactly the same rules —
// which is the point: a suggestion the save would refuse is worse than no
// suggestion.
func SuggestPort(kind PortKind, policy PortPolicy, holders []PortHolder, probe Prober) (int, error) {
	first, last := DefaultPublicPortBase, 65535
	if kind == PortInternal {
		first, last = policy.InternalMin, policy.InternalMax
	}

	for port, tried := first, 0; port <= last && tried < PortScanLimit; port, tried = port+1, tried+1 {
		if err := ValidatePort(kind, port, policy, holders, "", probe); err == nil {
			return port, nil
		}
	}
	return 0, model.Error{
		Code:    model.CodePortUnavailable,
		Message: fmt.Sprintf("no free %s port was found in [%d, %d]", kind, first, last),
		Details: map[string]any{"reason": string(model.PortBindFailed), "kind": string(kind)},
	}
}

// LiveProbe is the production Prober: a real bind-and-close through
// internal/netutil, which is the only thing that answers "may THIS process bind
// this port right now".
func LiveProbe(bind string, port int) bool { return netutil.Free(bind, port) }

// bindFor is which address a port of this kind has to bind: the gateway's for a
// public port, loopback for an internal one — llama-server never listens on a
// routable address (section 5.7).
func bindFor(kind PortKind, policy PortPolicy) string {
	if kind == PortPublic {
		return policy.GatewayBind
	}
	return LoopbackHost
}

func inPool(port int, policy PortPolicy) bool {
	return port >= policy.InternalMin && port <= policy.InternalMax
}

func holderOf(port int, holders []PortHolder, excludeID string) (PortHolder, bool) {
	for _, h := range holders {
		if h.InstanceID == excludeID {
			continue
		}
		if h.PublicPort == port || h.InternalPort == port {
			return h, true
		}
	}
	return PortHolder{}, false
}

func portError(port int, reason model.PortReason, message string) error {
	return model.Error{
		Code:    model.CodePortUnavailable,
		Message: message,
		Details: map[string]any{"port": port, "reason": string(reason)},
	}
}
