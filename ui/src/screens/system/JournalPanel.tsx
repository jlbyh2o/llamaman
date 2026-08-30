/**
 * The system journal viewer (DESIGN section 3.3 `GET /system/journal`, section 5.3, D77).
 *
 * "An empty stream and a denied one must not look alike": a `409 journal_unavailable` is caught
 * before the log pane ever opens, and renders the F23 remediation card in its place rather than a
 * pane that looks like nobody has logged anything this boot.
 */

import { useMemo, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import {
  Button,
  Input,
  LoadingPanel,
  LogViewer,
  PanelHeader,
  Select,
  Switch,
} from '../../components';
import { API_BASE } from '../../api/client';
import { ApiError } from '../../api/errors';
import { CommandLine } from '../../features/system/CommandBlock';
import { useJournalPage } from '../../features/system/queries';
import { useLogStream } from '../../events/useLogStream';
import { formatTime } from '../../format';
import type { JournalLine } from '../../features/system/types';

const UNIT_OPTIONS = [
  { value: 'llamaman', label: 'llamaman.service' },
  { value: 'llamaman-instances', label: 'llamaman-instances.target' },
];

function formatLine(line: JournalLine): string {
  const prefix = `${formatTime(line.at)} ${line.unit ? `${line.unit}: ` : ''}`;
  return `${prefix}${line.message}`;
}

export function JournalPanel() {
  const [unit, setUnit] = useState('llamaman');
  const [lines, setLines] = useState(500);
  const [live, setLive] = useState(false);

  const page = useJournalPage({ unit, lines }, !live);
  const denied = page.error instanceof ApiError && page.error.code === 'journal_unavailable';

  const formatted = useMemo(() => (page.data ?? []).map(formatLine), [page.data]);

  const liveUrl =
    live && !denied ? `${API_BASE}/system/journal?unit=${encodeURIComponent(unit)}&tail=200` : null;
  const stream = useLogStream({ url: liveUrl, initialLines: formatted });

  return (
    <div className="space-y-3">
      <PanelHeader
        title="Journal"
        description="journalctl -o json for the unit selected below."
        actions={
          <div className="flex items-center gap-3">
            <Select value={unit} onValueChange={setUnit} options={UNIT_OPTIONS} className="w-56" />
            <Input
              type="number"
              mono
              value={lines}
              onChange={(event) => setLines(Number(event.target.value) || 500)}
              min={50}
              max={5000}
              className="w-24"
              aria-label="Lines"
              disabled={live}
            />
            <label className="flex items-center gap-2 text-xs text-[var(--lm-text-muted)]">
              <Switch checked={live} onCheckedChange={setLive} />
              Live follow
            </label>
            {!live ? (
              <Button
                size="sm"
                variant="secondary"
                icon={<RefreshCw />}
                loading={page.isFetching}
                onClick={() => void page.refetch()}
              >
                Refresh
              </Button>
            ) : null}
          </div>
        }
      />

      {denied ? (
        <div className="space-y-2 rounded-[var(--lm-radius)] border border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)] p-3">
          <p className="text-sm font-medium text-[var(--lm-text)]">
            This identity cannot read the journal (F23)
          </p>
          <p className="text-xs text-[var(--lm-text-muted)]">
            An empty pane here would look the same as nobody having logged anything — this identity
            simply is not in the group that grants it. Run:
          </p>
          <CommandLine command="sudo usermod -aG systemd-journal <identity> && sudo systemctl restart llamaman.service" />
        </div>
      ) : page.isLoading && !live ? (
        <LoadingPanel>Reading the journal…</LoadingPanel>
      ) : (
        <div className="rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)]">
          <LogViewer
            lines={live ? stream.lines : formatted}
            rows={28}
            lineNumbers={false}
            aria-label="System journal"
            toolbar={
              live ? (
                <span>
                  {stream.status === 'live'
                    ? 'Following live'
                    : stream.status === 'degraded'
                      ? 'Reconnecting…'
                      : 'Connecting…'}
                </span>
              ) : (
                <span>{formatted.length} lines</span>
              )
            }
          />
        </div>
      )}
    </div>
  );
}
