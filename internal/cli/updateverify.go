package cli

import (
	"context"
	"fmt"

	"github.com/jlbyh2o/llamaman/internal/selfupdate"
)

// `llamaman update-verify --scope system|user` — the JUDGE of DESIGN section
// 12.2, started by `OnFailure=llamaman-update-verify.service` on
// `llamaman.service` (D88) and never by a timer, never by the daemon.
//
// **It runs out of `<prefix>/llamaman.prev`, the retained previous binary**
// (D13): a judge that is itself the binary under judgment cannot run when the
// verdict is "it does not start". Its unit's two `ConditionPathExists=` lines —
// `<prefix>/llamaman.prev` and `update/pending` — are the whole arming logic, so
// on a fresh install and on every host where no update is in flight this
// subcommand is never reached at all.
//
// Its whole body is three checks and a rename, and everything it deliberately
// does NOT do is as load-bearing as what it does:
//
//   - It reads NO FIELD of any file. The verdict turns on the marker's existence
//     and on the unit state systemd reports, so no format it does not understand
//     can disarm it.
//   - It opens no database, **not even read-only**: a root-created `-wal`/`-shm`
//     beside a 0600 database is a database the service identity can never write
//     again (section 11.3).
//   - It stops nothing, restores no database and writes no marker.
//
// A missing or unparsable `--scope` performs nothing, logs and exits non-zero;
// the unit's `ExecStopPost=` still runs `reset-failed` and `start` on
// `llamaman.service`.

// UpdateVerify is the judge.
func UpdateVerify(env Env, args []string) error {
	rest, err := unitOnly(env, "update-verify", args)
	if err != nil {
		return err
	}

	scope, err := parseScopeFlag("update-verify", env, rest)
	if err != nil {
		fmt.Fprintf(env.Stderr, "llamaman update-verify: %v\n", err)
		fmt.Fprintf(env.Stderr, "repair: %s\n", repairUnitsLine)
		return err
	}

	log := actorLogger(env)
	v, err := selfupdate.Verify(context.Background(), selfupdate.VerifyOptions{
		Scope: scope,
		Log:   log,
	})
	if err != nil {
		// A refusal changes nothing — the host is exactly as the judge found it —
		// and the journal line carries the one manual command that does the same
		// thing by hand (section 12.3 rows 12-14).
		log.Error("self-update revert refused",
			"error", err, "scope", string(scope), "touched", "nothing")
		fmt.Fprintf(env.Stderr, "llamaman update-verify: %v\n", err)
		return err
	}

	if !v.Reverted {
		// Exit 0. "The unit is `active`, so this is not my business" is a
		// successful run that did nothing, and exiting non-zero for it would make
		// OnFailure= retry a judge that correctly declined.
		return nil
	}
	return nil
}
