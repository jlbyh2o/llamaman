/**
 * The diagnostics bundle (DESIGN section 4 screen 16, section 11.3, D50).
 *
 * Screen 16's list ends with "Download diagnostics bundle" and the control did not exist — while
 * `features/instances/remediation.ts` was already telling a user recovering from a failed start to
 * "collect a diagnostics bundle from the System screen before restarting", which sent them to a
 * screen with no such button. The artifact was defined (`llamaman diagnostics --out FILE`) and the
 * daemon could build it; only the UI half was missing.
 *
 * It is an anchor, not a fetch. The response is a `.tar.gz` with a `Content-Disposition`, so letting
 * the browser handle the navigation gets the user a file under the daemon's own name — which
 * encodes the version and the timestamp a support conversation needs — with no blob URL to build and
 * revoke, and no chance of holding a multi-megabyte archive in memory. The session cookie rides
 * along exactly as it does for every other request.
 */

import { Download, ShieldCheck } from 'lucide-react';
import { Button, Panel, PanelHeader } from '../../components';
import { systemApi } from '../../features/system/api';

export function DiagnosticsPanel() {
  return (
    <Panel>
      <PanelHeader
        title="Diagnostics bundle"
        description="Everything a bug report needs about this host, in one archive."
        actions={
          <a href={systemApi.diagnosticsUrl()} download>
            <Button size="sm" variant="secondary" icon={<Download />}>
              Download
            </Button>
          </a>
        }
      />
      <p className="mt-3 flex items-start gap-2 text-xs text-[var(--lm-text-muted)]">
        <ShieldCheck aria-hidden className="mt-0.5 size-3.5 shrink-0 text-[var(--lm-ok)]" />
        <span>
          It carries the toolchain report, the unit-file drift check, the schema version and row
          counts, job and instance summaries, and a bounded journal tail. It deliberately carries no
          copy of the database, no session id, and no plaintext secret — the Hugging Face and GitHub
          tokens appear only as their masked hint, and every stored secret value is scrubbed from
          every other file before the archive is written.
        </span>
      </p>
    </Panel>
  );
}
