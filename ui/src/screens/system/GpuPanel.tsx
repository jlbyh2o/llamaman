/** GPU inventory (DESIGN section 3.3 `GET /system/gpus`). */

import { Cpu } from 'lucide-react';
import { EmptyState, LoadingPanel, Meter, Panel, PanelHeader, QueryError } from '../../components';
import { formatBytes, formatPercent } from '../../format';
import { useGpus } from '../../features/system/queries';
import type { Gpu } from '../../features/system/types';

/**
 * One device.
 *
 * Every telemetry field is nullable and `state` says which reading this is, because section 3.3 and
 * D16 are explicit that a failed poll marks a device UNKNOWN rather than zero: a card rendering
 * "0 B free" reads as "full", which is the opposite of "we could not ask", and it is the reading
 * that turns every fit verdict into `wont_run` without ever saying why. So a device whose memory the
 * driver did not report shows a dash and a "Stale" marker, never a meter at zero.
 */
export function GpuCard({ gpu }: { gpu: Gpu }) {
  const vramKnown = gpu.vram_total !== null && gpu.vram_used !== null;

  return (
    <Panel>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-[var(--lm-text)]">{gpu.name}</p>
          <p className="lm-numeric mt-0.5 text-[11px] text-[var(--lm-text-faint)]">
            #{gpu.index} · {gpu.uuid}
          </p>
        </div>
        {gpu.state === 'unknown' ? (
          <span className="shrink-0 text-[11px] font-medium text-[var(--lm-warn)]">Stale</span>
        ) : null}
      </div>

      {vramKnown ? (
        <Meter
          className="mt-3"
          used={gpu.vram_used ?? 0}
          total={gpu.vram_total ?? 0}
          label="VRAM"
          detail={`${formatBytes(gpu.vram_used)} / ${formatBytes(gpu.vram_total)}`}
        />
      ) : (
        <p className="mt-3 text-xs text-[var(--lm-text-muted)]">
          VRAM is not known — the last driver poll did not answer for this device.
        </p>
      )}

      <dl className="mt-3 grid grid-cols-2 gap-x-3 gap-y-2 text-xs">
        <div>
          <dt className="text-[var(--lm-text-faint)]">Utilization</dt>
          <dd className="lm-numeric text-[var(--lm-text)]">
            {gpu.util_pct === null || gpu.util_pct === undefined
              ? '—'
              : formatPercent(gpu.util_pct, { alreadyPercent: true })}
          </dd>
        </div>
        <div>
          <dt className="text-[var(--lm-text-faint)]">Temperature</dt>
          <dd className="lm-numeric text-[var(--lm-text)]">
            {gpu.temp_c === null || gpu.temp_c === undefined ? '—' : `${gpu.temp_c}°C`}
          </dd>
        </div>
        <div>
          <dt className="text-[var(--lm-text-faint)]">Driver</dt>
          <dd className="lm-numeric text-[var(--lm-text)]">{gpu.driver || '—'}</dd>
        </div>
        <div>
          <dt className="text-[var(--lm-text-faint)]">CUDA / SM</dt>
          <dd className="lm-numeric text-[var(--lm-text)]">
            {gpu.cuda ?? '—'} {gpu.compute_cap ? `· sm_${gpu.compute_cap.replace('.', '')}` : ''}
          </dd>
        </div>
      </dl>
    </Panel>
  );
}

export function GpuPanel() {
  const gpus = useGpus();

  return (
    <div className="space-y-3">
      <PanelHeader
        title="GPUs"
        description="Polled at gpu.poll_active_sec while an instance is running."
      />
      {gpus.isPending ? (
        <LoadingPanel>Reading GPU inventory…</LoadingPanel>
      ) : gpus.isError ? (
        // The state this screen used to be missing. Without it a failed read rendered "No GPUs
        // detected" — a claim about the host that the app has no basis for making.
        <QueryError
          title="The GPU inventory could not be read"
          error={gpus.error}
          onRetry={() => void gpus.refetch()}
        />
      ) : (gpus.data?.length ?? 0) === 0 ? (
        <EmptyState
          icon={<Cpu />}
          title="No GPUs detected"
          description="Instances will run CPU-only until a supported device is found."
        />
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {(gpus.data ?? []).map((gpu) => (
            <GpuCard key={gpu.uuid} gpu={gpu} />
          ))}
        </div>
      )}
    </div>
  );
}
