/**
 * The downgrade consequence (DESIGN section 12.4, D94).
 *
 * "`llamaman restore-db` on its own does not complete a downgrade — it is a destructive no-op." The
 * one-click apply below still runs — it self-corrects within about 235 seconds and leaves the host
 * back on the version it was running — so this is shown *before* the click as an informed choice,
 * and again after it (with the server's own `procedure` array) as the exact five commands that make
 * the downgrade actually stick.
 */

import { AlertTriangle } from 'lucide-react';
import { CommandProcedure } from '../../features/system/CommandBlock';

const GENERIC_PROCEDURE = [
  'sudo systemctl stop llamaman.service',
  'curl -fsSL https://raw.githubusercontent.com/jlbyh2o/llamaman/main/install.sh | sudo sh -s -- --version <older-version> --no-start',
  'sudo llamaman restore-db /var/lib/llamaman/db-backups/<snapshot>.db',
  'sudo systemctl reset-failed llamaman.service',
  'sudo systemctl start llamaman.service',
];

export function DowngradeNotice({ procedure }: { procedure?: readonly string[] }) {
  return (
    <div className="space-y-2 rounded-[var(--lm-radius)] border border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)] p-3">
      <p className="flex items-center gap-1.5 text-sm font-medium text-[var(--lm-text)]">
        <AlertTriangle className="size-4 shrink-0" aria-hidden />
        This is older than what's running
      </p>
      <p className="text-xs text-[var(--lm-text-muted)]">
        Migrations are forward-only. Clicking Update runs the same pipeline and, if the schema has
        moved past what the older binary understands, self-corrects within about 235 seconds — the
        host ends up back on the version it's running now. To make the downgrade actually stick,
        complete these five commands, in order, from the machine itself:
      </p>
      <CommandProcedure commands={procedure ?? GENERIC_PROCEDURE} />
      <p className="text-xs text-[var(--lm-text-muted)]">
        <code className="lm-numeric">restore-db</code> alone is a destructive no-op — steps 1 and 2
        are what make it mean something, and step 4 is what lets step 5 succeed.
      </p>
    </div>
  );
}
