package systemd

import (
	"errors"
	"os"
	"sync"

	"github.com/jlbyh2o/llamaman/internal/model"
)

// ErrSystemctlNotFound is returned when neither candidate path exists. It is a
// refusal to install rather than a fallback, because a `systemctl` this design
// cannot name is a `systemctl` the three callers below could disagree about.
var ErrSystemctlNotFound = errors.New("systemd: systemctl not found at /usr/bin/systemctl or /bin/systemctl")

// systemctlCandidates is the deterministic two-candidate probe of DESIGN
// section 12.2, in order. It is deliberately NOT a PATH search: section 5.4a's
// drift check re-renders every unit and compares hashes, so it has to agree with
// whatever install-units wrote on the same host, whatever `PATH` happened to
// hold at either moment. systemd also requires an absolute first token in
// ExecStopPost=, so a bare name would not work anyway.
var systemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}

var systemctlOverride struct {
	sync.RWMutex
	path string
	set  bool
}

// SystemctlPath is the ONLY producer of a systemctl path anywhere in this
// design (DESIGN section 12.2). Three callers share it and must not be able to
// disagree: install-units renders its result into the two self-update actors'
// ExecStopPost= lines, section 5.4a's drift render resolves the same token, and
// the exec fallback controller runs it.
func SystemctlPath() (string, error) {
	systemctlOverride.RLock()
	p, set := systemctlOverride.path, systemctlOverride.set
	systemctlOverride.RUnlock()
	if set {
		return p, nil
	}

	for _, c := range systemctlCandidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", ErrSystemctlNotFound
}

// setSystemctlPath overrides the probe for a test. It returns a restore func.
func setSystemctlPath(p string) func() {
	systemctlOverride.Lock()
	prev, prevSet := systemctlOverride.path, systemctlOverride.set
	systemctlOverride.path, systemctlOverride.set = p, true
	systemctlOverride.Unlock()
	return func() {
		systemctlOverride.Lock()
		systemctlOverride.path, systemctlOverride.set = prev, prevSet
		systemctlOverride.Unlock()
	}
}

// scopeArgs is the `--user` prefix every systemctl and journalctl invocation
// needs in the D2 topology, and nothing in the default one.
func scopeArgs(scope model.SystemdScope) []string {
	if scope == model.ScopeUser {
		return []string{"--user"}
	}
	return nil
}
