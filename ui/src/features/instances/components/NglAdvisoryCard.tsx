/**
 * "auto — llama.cpp will choose; we estimate 37 of 37 layers fit."
 *
 * DESIGN section 5.7 writes that sentence out and gives it a button: `POST /instances/{id}/pin-ngl`
 * "rewrites the FlagSet to `{"mode":"count","count":37}`. Pinning is an explicit config edit with a
 * new `config_hash`, a bumped `generation` and a `restart_required` flag — never something the
 * launcher does behind the user's back."
 *
 * Which is why the card says all three things before the click: what is configured now, what we
 * estimate, and that pinning changes the saved configuration rather than the running one.
 */

import { Pin } from 'lucide-react';
import { Badge, Button, Mono, Panel, PanelHeader } from '../../../components';
import type { FitReport, NGpuLayers } from '../../../api/types';

export interface NglAdvisoryCardProps {
  ngl: NGpuLayers | null | undefined;
  report?: FitReport | undefined;
  loading?: boolean;
  /** Absent when this daemon does not serve `pin-ngl`, or the instance is deleted. */
  onPin?: (() => void) | undefined;
  pinning?: boolean;
}

const DESCRIPTION: Record<string, string> = {
  auto: 'No -ngl is passed at all, so llama.cpp’s own --fit decides at load time.',
  all: 'Every layer is offloaded (-ngl 999).',
  none: 'Nothing is offloaded (-ngl 0): this instance runs on the CPU.',
  count: 'A fixed number of layers is offloaded.',
};

export function NglAdvisoryCard({
  ngl,
  report,
  loading = false,
  onPin,
  pinning = false,
}: NglAdvisoryCardProps) {
  const mode = ngl?.mode ?? 'auto';
  const configured =
    mode === 'count'
      ? `-ngl ${ngl?.count ?? '?'}`
      : mode === 'all'
        ? '-ngl 999'
        : mode === 'none'
          ? '-ngl 0'
          : 'no -ngl';

  return (
    <Panel>
      <PanelHeader
        title="GPU offload"
        description={DESCRIPTION[mode] ?? DESCRIPTION['auto']}
        actions={
          <Badge tone={mode === 'auto' ? 'accent' : 'neutral'}>
            <Mono>{configured}</Mono>
          </Badge>
        }
      />

      {mode === 'auto' ? (
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <p className="text-xs text-[var(--lm-text-muted)]">
            {report
              ? `We estimate ${report.max_n_gpu_layers} of ${report.inputs.n_layer} layers fit on the selected devices.`
              : loading
                ? 'Estimating…'
                : 'No estimate available for this configuration yet.'}
          </p>
          {report && onPin ? (
            <Button size="sm" icon={<Pin />} loading={pinning} onClick={onPin}>
              Pin {report.max_n_gpu_layers} layers
            </Button>
          ) : null}
          {report && onPin ? (
            <span className="text-[11px] text-[var(--lm-text-faint)]">
              Pinning is a configuration edit: it changes the saved config, and a running instance
              then shows “restart required”.
            </span>
          ) : null}
        </div>
      ) : null}
    </Panel>
  );
}
