// Package systemd is the only package that speaks to systemd (DESIGN section 1,
// invariant 2). Nothing outside it may import go-systemd, dial the bus, or exec
// systemctl or journalctl.
//
// What lives here, and where DESIGN specifies each part:
//
//   - Controller (section 5.3) and its two implementations — DBusController,
//     the primary, and ExecController, the degraded systemctl fallback. Connect
//     chooses between them at boot and reports which won, so the UI can say
//     whether unit state is pushed or polled. The interface deliberately does
//     NOT expose StartTransientUnit (D3): polkit sees a unit name, not its
//     properties, so any transient-unit grant would let a compromised daemon
//     start a unit with User=root, and removing the capability is the only real
//     mitigation.
//   - Unit and polkit rendering (sections 5.2, 5.4, 5.5, 12.2), from the
//     templates embedded in packaging/. One renderer produces both what
//     install-units writes and what section 5.4a's drift check compares against,
//     so the two cannot disagree about what a host should have. Every file it
//     writes is stamped `# llamaman-units: <N>` on its first line (D95), which
//     is what makes a content mismatch decidable: same stamp and a different
//     hash is a hand-edit (F16), an older stamp is `drift: stale` and blocks
//     nothing.
//   - Install (sections 11.3 and 13 step 7), the body of `llamaman
//     install-units`: write the files, add the identity to systemd-journal
//     (D77), reload the manager. It creates nothing under the state directory.
//   - CheckPolkit (section 5.2), the boot self-test: two CheckAuthorization
//     calls made BEFORE anything a user clicks depends on the answer, so a
//     misconfigured host degrades to read-only with a remediation instead of
//     failing on the first Start click (F9).
//   - The journal reader (D6), a `journalctl -o json [--follow]` subprocess.
//     sdjournal is not used because it requires cgo, which forfeits the single
//     static binary.
//   - sd_notify (D9): READY, STATUS, WATCHDOG, EXTEND_TIMEOUT_USEC, STOPPING,
//     plus the FDSTORE half of D58's listener continuity.
//   - ScopeProbe (section 11.1 step 1), the user-bus question internal/app takes
//     as a callback because only this package may ask one.
package systemd
