/** Disk usage per cache root and the state directory (DESIGN section 3.3 `GET /system/disk`). */

import { HardDrive } from 'lucide-react';
import {
  Badge,
  EmptyState,
  LoadingPanel,
  Meter,
  Panel,
  PanelHeader,
  QueryError,
} from '../../components';
import { formatBytes } from '../../format';
import { useDisk } from '../../features/system/queries';

export function DiskPanel() {
  const disk = useDisk();

  return (
    <div className="space-y-3">
      <PanelHeader
        title="Disk"
        description="Every cache root and the state directory, refreshed every 30 s."
      />
      {disk.isPending ? (
        <LoadingPanel>Reading disk usage…</LoadingPanel>
      ) : disk.isError ? (
        <QueryError
          title="Disk usage could not be read"
          error={disk.error}
          onRetry={() => void disk.refetch()}
        />
      ) : (disk.data?.length ?? 0) === 0 ? (
        <EmptyState icon={<HardDrive />} title="No disk data yet" />
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {disk.data.map((entry) => (
            <Panel key={entry.path}>
              <div className="flex items-center justify-between gap-2">
                <p className="lm-numeric truncate text-sm text-[var(--lm-text)]">{entry.path}</p>
                <div className="flex shrink-0 items-center gap-1.5">
                  {entry.primary ? <Badge tone="accent">Primary</Badge> : null}
                  <Badge tone="neutral">
                    {entry.kind === 'cache_root' ? 'Cache root' : 'State dir'}
                  </Badge>
                </div>
              </div>
              <Meter
                className="mt-3"
                used={entry.used_bytes}
                total={entry.total_bytes}
                label="Used"
                detail={`${formatBytes(entry.free_bytes)} free of ${formatBytes(entry.total_bytes)}`}
              />
              {entry.model_bytes != null || entry.version_bytes != null ? (
                <dl className="mt-3 grid grid-cols-2 gap-x-3 text-xs">
                  {entry.model_bytes != null ? (
                    <div>
                      <dt className="text-[var(--lm-text-faint)]">Models</dt>
                      <dd className="lm-numeric text-[var(--lm-text)]">
                        {formatBytes(entry.model_bytes)}
                      </dd>
                    </div>
                  ) : null}
                  {entry.version_bytes != null ? (
                    <div>
                      <dt className="text-[var(--lm-text-faint)]">llama.cpp versions</dt>
                      <dd className="lm-numeric text-[var(--lm-text)]">
                        {formatBytes(entry.version_bytes)}
                      </dd>
                    </div>
                  ) : null}
                </dl>
              ) : null}
            </Panel>
          ))}
        </div>
      )}
    </div>
  );
}
