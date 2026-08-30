// Package supervisor is DESIGN section 5.8: the single reconciler that drives
// observed instance state toward desired state.
//
// It computes `(desired, actual)` per instance and takes AT MOST ONE corrective
// action per pass — a start, a stop, a port reassignment, an enablement change,
// or a recorded refusal. That rule is the whole design rather than a
// throttle: a reconciler that took every action it could justify in one pass
// would, on the pass after a crash, reassign a port, start the unit, enable it
// and write three ledger rows for a single event, and the next pass would
// observe a state that none of the three describe. Taking one action and
// re-observing is what makes every intermediate state a real state the UI can
// render and a test can assert.
//
// Three responsibilities live here and nowhere else:
//
//   - The `instance_starts` ledger's CLOSING half. `instance-exec` opens a row
//     before preflight (D54) and closes it on every path it can; the supervisor
//     closes the rest — a run whose unit went away, a launcher that died before
//     it could write, a run abandoned by a previous daemon — and it is the only
//     writer permitted to. `outcome` is written exactly once (D63), which is
//     what makes "the previous start" unambiguous for the restart policy.
//   - The restart policy and the crash-loop cutoff (D7/D8/D64), evaluated from
//     that ledger rather than from systemd's own restart counters, so behavior
//     is observable data instead of guesswork. The decision itself is a pure
//     function — see EvaluateStart — because it is the part of this system a
//     user reasons about directly.
//   - `instance_status`, with the three synchronous recovery exceptions §2.8
//     names. `applied_config_hash` in particular is stamped here and only here,
//     at the first `/health` 200, so a launcher that reached execve and then
//     died during model load never clears `restart_required` for a
//     configuration that never ran.
//
// It also owns the boot decisions that must happen exactly once per daemon
// start: the D53 autostart coupling keyed to the host boot (§5.8 step 1, the
// only writer of `runtime_info.host_boot_id`), and D74's single relabel of
// `external` starts that happened in the window between the host coming up and
// this daemon doing so.
//
// The exit-code contract of §5.6 is declared in contract.go and imported by the
// launcher, because those codes are values of a column this package owns.
package supervisor
