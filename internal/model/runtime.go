package model

// Runtime facts (DESIGN section 2.1).
//
// `runtime_info` is the singleton row of facts about this daemon and this host
// that the CLI must read without an HTTP call: `llamaman status` and `doctor`
// open the database directly. Four of its columns are closed enums, and each one
// is decided in exactly one place at boot and read everywhere else.

// SystemdScope is `runtime_info.systemd_scope` (§2.1, §11.1 step 1).
//
// Decided by ONE rule: the `serve --scope` flag `install-units` rendered into
// the unit, else the bus the connection succeeded on. Never guessed per call
// site — §5.2a, §5.3, §11.1 step 1 and §12 all read this column.
type SystemdScope string

const (
	ScopeSystem SystemdScope = "system"
	ScopeUser   SystemdScope = "user"
)

// SystemdScopeValues lists the members of the `runtime_info.systemd_scope` CHECK
// constraint, in order.
func SystemdScopeValues() []SystemdScope { return []SystemdScope{ScopeSystem, ScopeUser} }

// Valid reports whether s is a member of the CHECK constraint.
func (s SystemdScope) Valid() bool { return valid(s, SystemdScopeValues()) }

// SystemdControl is `runtime_info.systemd_control` (§2.1, §5.3): which control
// channel the daemon settled on. `unavailable` is not fatal — the daemon starts
// anyway, into the degraded mode of §11.1a — and it never means the daemon
// spawns llama-server itself.
type SystemdControl string

const (
	ControlDBus        SystemdControl = "dbus"
	ControlExec        SystemdControl = "exec"
	ControlUnavailable SystemdControl = "unavailable"
)

// SystemdControlValues lists the members of the `runtime_info.systemd_control`
// CHECK constraint, in order.
func SystemdControlValues() []SystemdControl {
	return []SystemdControl{ControlDBus, ControlExec, ControlUnavailable}
}

// Valid reports whether c is a member of the CHECK constraint.
func (c SystemdControl) Valid() bool { return valid(c, SystemdControlValues()) }

// JournalRead is `runtime_info.journal_read` (§2.1, D77): can this identity
// actually read the journal? Probed once at boot (§11.1 step 6). The two failure
// members are not interchangeable — `unavailable` means journalctl itself is
// absent, `denied` means it ran and returned nothing for a unit that has
// messages — and the UI says something different for each.
type JournalRead string

const (
	JournalOK          JournalRead = "ok"
	JournalDenied      JournalRead = "denied"
	JournalUnavailable JournalRead = "unavailable"
)

// JournalReadValues lists the members of the `runtime_info.journal_read` CHECK
// constraint, in order.
func JournalReadValues() []JournalRead {
	return []JournalRead{JournalOK, JournalDenied, JournalUnavailable}
}

// Valid reports whether j is a member of the CHECK constraint.
func (j JournalRead) Valid() bool { return valid(j, JournalReadValues()) }

// ListenerContinuity is `runtime_info.listener_continuity` (§2.1, D58/§9.4):
// whether the listening sockets survived the last restart through systemd's file
// descriptor store, or had to be rebound.
type ListenerContinuity string

const (
	ContinuityFDStore ListenerContinuity = "fdstore"
	ContinuityNone    ListenerContinuity = "none"
)

// ListenerContinuityValues lists the members of the
// `runtime_info.listener_continuity` CHECK constraint, in order.
func ListenerContinuityValues() []ListenerContinuity {
	return []ListenerContinuity{ContinuityFDStore, ContinuityNone}
}

// Valid reports whether c is a member of the CHECK constraint.
func (c ListenerContinuity) Valid() bool { return valid(c, ListenerContinuityValues()) }

// RuntimeInfo is the singleton `runtime_info` row (§2.1). Every column except
// the two version strings is nullable, because the row is written at boot before
// several of the facts have been probed, and a fact that has not been learned is
// NULL rather than a zero that reads as an answer (F14).
//
// Two columns carry rules that are not visible in their types:
//
//   - HostBootID and HostBootAt have EXACTLY ONE writer, supervisor boot
//     reconciliation step 1 (§5.8). §11.1 step 9 reads HostBootID and writes
//     nothing: persisting it there would destroy the input of the comparison the
//     supervisor makes a moment later, the D53 autostart coupling would never
//     fire, and autostart would be broken in both directions.
//   - HFHubDir and HFHome are a DERIVED CACHE, never an input. They are
//     rewritten from the primary `hf_cache_roots` row on every boot and on every
//     change to it, and exist only so `status` and `doctor` can print the cache
//     path without an HTTP call. The authority chain is `hf_cache_roots` ←
//     settings['hf.hub_dir'] ← these two (§7.2a). HFHome is NULL whenever the hub
//     directory is not literally `<something>/hub`.
type RuntimeInfo struct {
	DaemonVersion string
	DaemonCommit  string
	PID           *int64
	BootID        *string // ULID per daemon start; job lease owner
	BootAt        *int64
	HostBootID    *string // /proc/sys/kernel/random/boot_id, verbatim (D53)
	HostBootAt    *int64  // /proc/stat `btime` × 1000 (D74)
	UIBindAddr    *string
	UIPort        *int64 // ACTUAL port after the walk (§11.1 step 7)
	UIPortFlag    *int64 // `serve --port N` from the unit, NULL when absent
	UIURLHint     *string
	ServiceUser   *string
	ServiceUID    *int64
	ServiceGroup  *string
	ServiceGID    *int64

	SystemdScope       *SystemdScope
	SystemdControl     *SystemdControl
	JournalRead        *JournalRead
	PolkitOK           *bool // NULL in user scope: not asked, not denied (§5.2a)
	PolkitDetail       *string
	PolkitUnitFiles    *bool // manage-unit-files granted? NULL in user scope
	ListenerContinuity *ListenerContinuity

	BinaryPath      *string // filepath.EvalSymlinks(os.Executable()) — never hardcoded
	HFHubDir        *string
	HFHome          *string
	StateDir        *string
	SchemaVersion   *int64 // MAX(schema_migrations.version) after boot migration
	LastHeartbeatAt *int64
}
