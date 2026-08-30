package instances

import (
	"errors"
	"net"
	"testing"

	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/netutil"
)

// Port-rule tests (DESIGN section 2.8's table).
//
// The probe is injected rather than real, which is the point of it being a
// parameter: the six rules are a table, and a table is testable without binding
// anything. The one test that does bind uses the production prober, so the seam
// itself is exercised too.

func testPolicy() PortPolicy {
	return PortPolicy{
		UIPortDesired: 5526,
		UIPort:        5527,
		InternalMin:   21000,
		InternalMax:   21999,
		GatewayBind:   "0.0.0.0",
	}
}

func freeProbe(string, int) bool { return true }
func busyProbe(string, int) bool { return false }

// reasonOf digs section 2.8's `reason` out of a port refusal.
func reasonOf(t *testing.T, err error) model.PortReason {
	t.Helper()
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a model.Error", err)
	}
	if me.Code != model.CodePortUnavailable {
		t.Fatalf("code = %q, want %q", me.Code, model.CodePortUnavailable)
	}
	reason, _ := me.Details["reason"].(string)
	return model.PortReason(reason)
}

func TestValidatePortRules(t *testing.T) {
	holders := []PortHolder{
		{InstanceID: "i-1", Name: "qwen", PublicPort: 8081, InternalPort: 21001},
		{InstanceID: "i-2", Name: "gemma", PublicPort: 8082, InternalPort: 21002},
	}

	tests := []struct {
		name    string
		kind    PortKind
		port    int
		exclude string
		probe   Prober
		want    model.PortReason
		ok      bool
	}{
		{name: "a free public port", kind: PortPublic, port: 8090, probe: freeProbe, ok: true},
		{name: "a free internal port", kind: PortInternal, port: 21050, probe: freeProbe, ok: true},
		{
			name: "the desired management port", kind: PortPublic, port: 5526,
			probe: freeProbe, want: model.PortReservedManagement,
		},
		{
			// The walk of §11.1 step 7 may have landed somewhere else, and that
			// port is just as much the management UI's.
			name: "the port the management walk actually landed on", kind: PortPublic, port: 5527,
			probe: freeProbe, want: model.PortReservedManagement,
		},
		{
			name: "a public port inside the internal pool", kind: PortPublic, port: 21500,
			probe: freeProbe, want: model.PortReservedInternalPool,
		},
		{
			name: "an internal port outside the pool", kind: PortInternal, port: 8090,
			probe: freeProbe, want: model.PortOutsideInternalPool,
		},
		{
			name: "a public port another instance holds", kind: PortPublic, port: 8082,
			probe: freeProbe, want: model.PortInUseByInstance,
		},
		{
			// Either column counts: the rule is "not held by another instance
			// (either column)", so a public port equal to someone's internal
			// port is refused even though the pools normally keep them apart.
			name: "a port another instance holds in the OTHER column", kind: PortInternal, port: 21001,
			probe: freeProbe, want: model.PortInUseByInstance,
		},
		{
			// A PATCH that does not move the ports must not fail its own
			// validation against the row it is editing.
			name: "the instance's own port, editing itself", kind: PortInternal, port: 21001,
			exclude: "i-1", probe: freeProbe, ok: true,
		},
		{
			name: "a port something else on the host holds", kind: PortPublic, port: 8090,
			probe: busyProbe, want: model.PortBindFailed,
		},
		{
			name: "a privileged port", kind: PortPublic, port: 80,
			probe: freeProbe, want: model.PortBindFailed,
		},
		{
			name: "a port beyond the range", kind: PortPublic, port: 70000,
			probe: freeProbe, want: model.PortBindFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePort(tt.kind, tt.port, testPolicy(), holders, tt.exclude, tt.probe)
			if tt.ok {
				if err != nil {
					t.Fatalf("ValidatePort = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("port %d was accepted, want reason %q", tt.port, tt.want)
			}
			if got := reasonOf(t, err); got != tt.want {
				t.Errorf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPortRefusalNamesTheHolder: `in_use_by_instance` says WHICH instance, so
// the form can offer to jump to it instead of only refusing.
func TestPortRefusalNamesTheHolder(t *testing.T) {
	holders := []PortHolder{{InstanceID: "i-1", Name: "qwen", PublicPort: 8081, InternalPort: 21001}}

	err := ValidatePort(PortPublic, 8081, testPolicy(), holders, "", freeProbe)
	var me model.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a model.Error", err)
	}
	if got := me.Details["instance"]; got != "qwen" {
		t.Errorf("details.instance = %v, want the holding instance's name", got)
	}
	if got := me.Details["instance_id"]; got != "i-1" {
		t.Errorf("details.instance_id = %v, want i-1", got)
	}
}

// TestSuggestPortAppliesTheSameRules: a suggestion the save would refuse is
// worse than no suggestion.
func TestSuggestPortAppliesTheSameRules(t *testing.T) {
	holders := []PortHolder{
		{InstanceID: "i-1", Name: "a", PublicPort: 8080, InternalPort: 21000},
		{InstanceID: "i-2", Name: "b", PublicPort: 8081, InternalPort: 21001},
	}

	pub, err := SuggestPort(PortPublic, testPolicy(), holders, freeProbe)
	if err != nil {
		t.Fatalf("SuggestPort(public): %v", err)
	}
	if pub != 8082 {
		t.Errorf("public suggestion = %d, want the first free port after the held ones", pub)
	}

	internal, err := SuggestPort(PortInternal, testPolicy(), holders, freeProbe)
	if err != nil {
		t.Fatalf("SuggestPort(internal): %v", err)
	}
	if internal != 21002 {
		t.Errorf("internal suggestion = %d, want the first free port in the pool", internal)
	}

	// And every suggestion must pass the very validation that produced it.
	for kind, port := range map[PortKind]int{PortPublic: pub, PortInternal: internal} {
		if err := ValidatePort(kind, port, testPolicy(), holders, "", freeProbe); err != nil {
			t.Errorf("the %s suggestion %d does not validate: %v", kind, port, err)
		}
	}
}

// TestSuggestPortExhausted: a pool with nothing free is reported rather than
// looped over.
func TestSuggestPortExhausted(t *testing.T) {
	policy := testPolicy()
	policy.InternalMin, policy.InternalMax = 21000, 21001
	holders := []PortHolder{
		{InstanceID: "i-1", Name: "a", PublicPort: 8080, InternalPort: 21000},
		{InstanceID: "i-2", Name: "b", PublicPort: 8081, InternalPort: 21001},
	}

	if _, err := SuggestPort(PortInternal, policy, holders, freeProbe); err == nil {
		t.Fatal("an exhausted pool produced a suggestion")
	} else if codeOf(err) != model.CodePortUnavailable {
		t.Errorf("code = %q, want %q", codeOf(err), model.CodePortUnavailable)
	}
}

// TestLiveProbeBinds exercises the real seam: the production prober is a
// bind-and-close, so a port this test holds must come back unavailable.
func TestLiveProbeBinds(t *testing.T) {
	if !LiveProbe(LoopbackHost, 0) {
		t.Skip("this host refuses even an ephemeral bind")
	}

	ln, port, err := ephemeral(t)
	if err != nil {
		t.Skipf("could not bind an ephemeral port: %v", err)
	}
	defer ln.Close()

	if LiveProbe(LoopbackHost, port) {
		t.Errorf("LiveProbe said port %d was free while this test held it", port)
	}
}

// ephemeral binds a kernel-chosen loopback port for TestLiveProbeBinds.
func ephemeral(t *testing.T) (net.Listener, int, error) {
	t.Helper()
	return netutil.Ephemeral(LoopbackHost)
}
