/**
 * Installed unit drift (DESIGN section 3.3 `GET /system/units`, section 5.4a).
 *
 * "`stale` — an older or absent stamp — is the ordinary state of a host that has self-updated across
 * a release which changed a template, and it blocks nothing" (D95). Only `missing` and a hash
 * mismatch at the current stamp are actionable (F16), which is why `stale` gets a neutral badge
 * rather than a warning one.
 */

import { DataTable, LoadingPanel, Mono, QueryError } from '../../components';
import type { Column } from '../../components';
import { CommandLine } from '../../features/system/CommandBlock';
import { useUnits } from '../../features/system/queries';
import type { UnitDrift, UnitStatus } from '../../features/system/types';
import { Badge } from '../../components';
import type { Tone } from '../../components';

const DRIFT_STYLE: Record<UnitDrift, { tone: Tone; label: string }> = {
  none: { tone: 'ok', label: 'Up to date' },
  stale: { tone: 'neutral', label: 'Stale stamp' },
  missing: { tone: 'danger', label: 'Missing' },
};

export function UnitsPanel() {
  const units = useUnits();

  const columns: Column<UnitStatus>[] = [
    {
      id: 'unit',
      header: 'Unit',
      cell: (row) => <Mono>{row.unit}</Mono>,
      sortValue: (row) => row.unit,
    },
    {
      id: 'drift',
      header: 'Drift',
      cell: (row) => {
        const style = DRIFT_STYLE[row.drift];
        return <Badge tone={style.tone}>{style.label}</Badge>;
      },
    },
    {
      id: 'stamp',
      header: 'Stamp',
      cell: (row) => (
        <Mono className="text-[var(--lm-text-faint)]">
          {row.installed_stamp ?? '—'} → {row.template_stamp}
        </Mono>
      ),
      secondary: true,
    },
  ];

  const actionable = (units.data ?? []).filter((row) => row.repair_command);

  return (
    <div className="space-y-3">
      {units.isPending ? (
        <LoadingPanel>Reading installed units…</LoadingPanel>
      ) : units.isError ? (
        // The daemon cannot write /etc, so this table is the only place a drifted unit shows up.
        // Rendering an empty one asserts the units are fine, which is the opposite of not knowing.
        <QueryError
          title="The installed units could not be read"
          error={units.error}
          onRetry={() => void units.refetch()}
        />
      ) : (
        <DataTable
          columns={columns}
          rows={units.data ?? []}
          rowKey={(row) => row.unit}
          caption="Installed systemd units"
        />
      )}
      {actionable.length > 0 ? (
        <div className="space-y-2">
          {actionable.map((row) => (
            <div key={row.unit} className="space-y-1.5">
              <p className="text-xs text-[var(--lm-text-muted)]">Repair {row.unit}:</p>
              <CommandLine command={row.repair_command as string} />
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}
