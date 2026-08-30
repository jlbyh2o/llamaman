/**
 * The start ledger — every launch attempt, including the ones that never reached `execve`.
 *
 * `instance_starts` is opened *before* preflight (D54) and closed exactly once (D63), which is what
 * makes this table forensic rather than decorative: a row with no argv is a preflight refusal, a row
 * with `outcome=null` is the run happening right now, and an `inhibited` row is a refusal to start
 * rather than a run — which is why it carries an `error_code` naming the reason and no exit code.
 *
 * A safe start shows its override inline, "so 'it only works with -ngl 0' is a fact in the history
 * rather than something a user has to remember" (section 3.10b).
 */

import { Badge, DataTable, Mono } from '../../../components';
import type { Column } from '../../../components';
import { durationBetween, formatElapsed, formatTimestamp } from '../../../format';
import type { InstanceStart } from '../../../api/types';

const TRIGGER_LABEL: Record<string, string> = {
  user: 'You',
  autostart: 'Boot',
  supervisor_restart: 'Supervisor',
  rolling: 'Version roll',
  bench_restore: 'Bench restore',
  safe_start: 'Safe start',
  external: 'systemctl',
};

function outcomeBadge(row: InstanceStart) {
  if (!row.outcome)
    return (
      <Badge tone="info" dot pulse>
        In flight
      </Badge>
    );
  if (row.outcome === 'failed') return <Badge tone="danger">Failed</Badge>;
  if (row.outcome === 'inhibited') return <Badge tone="warn">Refused</Badge>;
  return <Badge tone="neutral">Stopped</Badge>;
}

function duration(row: InstanceStart): string {
  const ms = durationBetween(row.at, row.ended_at);
  return ms === null ? '—' : formatElapsed(ms);
}

export function StartsTable({ starts }: { starts: readonly InstanceStart[] }) {
  const columns: Column<InstanceStart>[] = [
    {
      id: 'at',
      header: 'When',
      cell: (row) => formatTimestamp(row.at),
      sortValue: (row) => row.at,
      mono: true,
    },
    {
      id: 'trigger',
      header: 'Trigger',
      cell: (row) => (
        <span className="flex items-center gap-1.5">
          {TRIGGER_LABEL[row.trigger] ?? row.trigger}
          {row.trigger === 'safe_start' ? <Badge tone="info">-ngl 0</Badge> : null}
        </span>
      ),
      sortValue: (row) => row.trigger,
    },
    {
      id: 'outcome',
      header: 'Outcome',
      cell: (row) => outcomeBadge(row),
      sortValue: (row) => row.outcome ?? '',
    },
    {
      id: 'ready',
      header: 'Reached ready',
      cell: (row) => formatTimestamp(row.ready_at),
      secondary: true,
      mono: true,
    },
    {
      id: 'duration',
      header: 'Ran for',
      cell: duration,
      align: 'right',
      mono: true,
      secondary: true,
    },
    {
      id: 'detail',
      header: 'Detail',
      cell: (row) => {
        if (row.error_code) {
          return (
            <span className="flex flex-col gap-0.5">
              <Mono>{row.error_code}</Mono>
              {row.error_message ? (
                <span className="text-xs text-[var(--lm-text-muted)]">{row.error_message}</span>
              ) : null}
              {row.exit_code !== null && row.exit_code !== undefined ? (
                <span className="text-xs text-[var(--lm-text-faint)]">exit {row.exit_code}</span>
              ) : null}
            </span>
          );
        }
        if (row.argv.length === 0) {
          return (
            <span className="text-xs text-[var(--lm-text-faint)]">
              No command line was rendered — the refusal happened in preflight.
            </span>
          );
        }
        const override = Object.keys(row.override ?? {});
        return override.length > 0 ? (
          <Mono>{JSON.stringify(row.override)}</Mono>
        ) : (
          <span className="text-xs text-[var(--lm-text-faint)]">—</span>
        );
      },
    },
  ];

  return (
    <DataTable
      columns={columns}
      rows={starts}
      rowKey={(row) => row.id}
      caption="Every launch attempt, newest first"
      empty={
        <p className="px-3 py-6 text-center text-xs text-[var(--lm-text-muted)]">
          This instance has never been started.
        </p>
      }
    />
  );
}
