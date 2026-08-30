import { Link } from '@tanstack/react-router';
import { Cpu } from 'lucide-react';
import { LoadingPanel, Meter, Panel, PanelHeader, QueryError } from '../../components';
import { formatBytes, formatPercent } from '../../format';
import { useGpus } from '../../features/system/queries';

/**
 * The GPU VRAM meters of DESIGN section 4, screen 3.
 *
 * The dashboard's job is to answer "is anything wrong" before anything is clicked, and on a GPU host
 * VRAM headroom is the first place the answer lives: an instance that will not start, a bench that
 * refuses, a fit verdict that flipped to `wont_run` are all downstream of a number that was on this
 * screen the whole time. `useGpus()` used to be read by exactly one component — the System screen's
 * GPU tab, three clicks away — so the landing view answered the question with the one fact it did
 * not have.
 *
 * It is a SUMMARY, not the System screen's card grid: a bar per device and the two numbers a person
 * reads at a glance, with a link to the detail. Section 4 is explicit that nothing on the dashboard
 * is a control surface.
 *
 * A device whose memory the driver did not report shows a dash rather than a meter at zero — D16's
 * rule, and the reason every VRAM field on the wire is nullable: "0 B free" reads as full, which is
 * the opposite of "we could not ask".
 */
export function GpuPanel() {
  const gpus = useGpus();

  // A host with no NVIDIA card answers with an empty list, and that is not a gap to explain — it is
  // a CPU host, and a permanent "no GPUs" panel on its dashboard is noise forever.
  if (!gpus.isPending && !gpus.isError && (gpus.data?.length ?? 0) === 0) return null;

  return (
    <Panel>
      <PanelHeader
        title="GPUs"
        description="VRAM in use right now, per device."
        actions={
          <Link
            to="/system"
            search={{ tab: 'gpus' }}
            className="text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
          >
            Details
          </Link>
        }
      />

      {gpus.isPending ? (
        <LoadingPanel>Reading the GPU inventory…</LoadingPanel>
      ) : gpus.isError ? (
        <QueryError
          title="The GPU inventory could not be read"
          error={gpus.error}
          onRetry={() => void gpus.refetch()}
          dense
          className="mt-3"
        />
      ) : (
        <ul className="mt-3 space-y-3">
          {(gpus.data ?? []).map((gpu) => {
            const known = gpu.vram_total !== null && gpu.vram_used !== null;
            return (
              <li key={gpu.uuid}>
                <div className="flex items-baseline justify-between gap-2">
                  <span className="truncate text-xs font-medium text-[var(--lm-text)]">
                    <Cpu aria-hidden className="mr-1 inline size-3 text-[var(--lm-text-faint)]" />
                    {gpu.name}
                  </span>
                  <span className="lm-numeric shrink-0 text-[11px] text-[var(--lm-text-faint)]">
                    {gpu.util_pct === null || gpu.util_pct === undefined
                      ? '—'
                      : formatPercent(gpu.util_pct, { alreadyPercent: true })}
                    {gpu.temp_c === null || gpu.temp_c === undefined ? '' : ` · ${gpu.temp_c}°C`}
                  </span>
                </div>
                {known ? (
                  <Meter
                    className="mt-1.5"
                    used={gpu.vram_used ?? 0}
                    total={gpu.vram_total ?? 0}
                    label="VRAM"
                    detail={`${formatBytes(gpu.vram_free)} free of ${formatBytes(gpu.vram_total)}`}
                  />
                ) : (
                  <p className="mt-1.5 text-[11px] text-[var(--lm-text-muted)]">
                    VRAM is not known — the last driver poll did not answer for this device.
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}
