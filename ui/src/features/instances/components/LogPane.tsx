/**
 * The live journald pane (DESIGN section 4, screen 6).
 *
 * A paged read seeds it and an SSE tail keeps it going — the same split `useLogStream` was written
 * for. Two failure modes are named rather than hidden, because both are designed states:
 *
 *  - **F23**: the service identity cannot read the journal, and `GET /instances/{id}/logs` answers
 *    `409 journal_unavailable`. The remedy is a root-only group change, so the card prints it.
 *  - The route is not served by this daemon yet (section 3.10's staged rollout), which is a
 *    different sentence from "there are no logs".
 */

import { AlertTriangle } from 'lucide-react';
import { LogViewer, StatusBadge } from '../../../components';
import { ApiError } from '../../../api/errors';

export interface LogPaneProps {
  lines: readonly string[];
  status: 'idle' | 'connecting' | 'live' | 'degraded';
  error?: unknown;
  rows?: number;
}

const STREAM_LABEL: Record<LogPaneProps['status'], { state: string; label: string }> = {
  idle: { state: 'stopped', label: 'Not following' },
  connecting: { state: 'starting', label: 'Connecting' },
  live: { state: 'ready', label: 'Following' },
  degraded: { state: 'degraded', label: 'Reconnecting' },
};

export function LogPane({ lines, status, error, rows = 28 }: LogPaneProps) {
  if (error) {
    const journalDenied = error instanceof ApiError && error.code === 'journal_unavailable';
    return (
      <div className="flex items-start gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)] px-3 py-2 text-xs text-[var(--lm-text)]">
        <AlertTriangle aria-hidden className="mt-0.5 size-3.5 shrink-0 text-[var(--lm-warn)]" />
        <div className="space-y-1">
          {journalDenied ? (
            <>
              <p>
                The daemon’s identity cannot read this unit’s journal, so there is nothing to tail.
              </p>
              <p className="lm-numeric rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-sunken)] px-2 py-1 text-[12px] text-[var(--lm-text-muted)]">
                sudo usermod -aG systemd-journal &lt;identity&gt; &amp;&amp; sudo systemctl restart
                llamaman.service
              </p>
            </>
          ) : (
            <p>
              This daemon does not serve{' '}
              <span className="lm-numeric">/instances/&#123;id&#125;/logs</span> yet. The unit’s
              journal is reachable with{' '}
              <span className="lm-numeric">
                journalctl -u llamaman-instance@&lt;name&gt;.service -f
              </span>
              .
            </p>
          )}
        </div>
      </div>
    );
  }

  const stream = STREAM_LABEL[status];
  return (
    <LogViewer
      lines={lines}
      rows={rows}
      lineNumbers={false}
      aria-label="Instance journal"
      toolbar={<StatusBadge kind="instance" state={stream.state} label={stream.label} />}
      placeholder="Waiting for the unit to log something…"
      className="rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)]"
    />
  );
}
