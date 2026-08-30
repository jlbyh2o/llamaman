import type { ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { Cpu, Gauge } from 'lucide-react';
import { Badge, FlagBadge, Panel, Progress, StatusBadge } from '../../components';
import type { Instance, Model } from '../../api/types';
import { formatBytes, formatCount, formatRelative } from '../../format';

/**
 * One instance, as the dashboard shows it (DESIGN section 4, screen 3: "instance cards — state,
 * port, model, slots busy, tokens/sec when metrics are on").
 *
 * Everything on the card comes from the `instances` join of section 3.10 — config ⋈ status, one
 * row — so a card is one object and never a fan-out of per-instance requests. `instance.status`
 * frames patch that row in place (section 3.14), which is why the state, the slot count and the
 * VRAM figure move without this screen asking again.
 *
 * The four derived flags of section 2.8 are badges *beside* the state, never a state: an instance
 * serving traffic can carry `restart_required` and still be `ready`, and a card that collapsed the
 * two would say something false.
 */
export function InstanceCard({ instance, model }: { instance: Instance; model?: Model }) {
  const status = instance.status;
  const slotsTotal = status.slots_total ?? 0;
  const slotsBusy = status.slots_busy ?? 0;

  return (
    <Panel className="min-w-0">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <Link
            to="/instances/$id"
            params={{ id: instance.id }}
            className="block truncate text-sm font-semibold text-[var(--lm-text)] hover:text-[var(--lm-accent)]"
          >
            {instance.display_name || instance.name}
          </Link>
          <p className="lm-numeric truncate text-xs text-[var(--lm-text-faint)]">
            {model ? `${model.repo_id}${model.quant_label ? ` · ${model.quant_label}` : ''}` : '—'}
          </p>
        </div>
        <StatusBadge kind="instance" state={status.state} />
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-1.5">
        <Badge tone="neutral">:{instance.public_port}</Badge>
        {instance.autostart ? <Badge tone="neutral">autostart</Badge> : null}
        {instance.restart_required ? <FlagBadge flag="restart_required" /> : null}
        {instance.stale_version ? <FlagBadge flag="stale_version" /> : null}
        {instance.inhibited ? (
          <FlagBadge flag="inhibited" reason={instance.inhibit_reason ?? null} />
        ) : null}
        {instance.draft_unverified ? <FlagBadge flag="draft_unverified" /> : null}
      </div>

      {slotsTotal > 0 ? (
        <Progress
          className="mt-3"
          size="sm"
          value={(slotsBusy / slotsTotal) * 100}
          tone={slotsBusy >= slotsTotal ? 'warn' : 'accent'}
          label="Slots"
          detail={`${formatCount(slotsBusy)} / ${formatCount(slotsTotal)} busy`}
        />
      ) : null}

      <dl className="mt-3 grid grid-cols-3 gap-2 text-xs">
        <Stat
          icon={<Gauge />}
          label="VRAM"
          value={status.vram_bytes ? formatBytes(status.vram_bytes) : '—'}
          title={status.gpu_attribution ? `Attribution: ${status.gpu_attribution}` : undefined}
        />
        <Stat
          icon={<Cpu />}
          label="Resident"
          value={status.rss_bytes ? formatBytes(status.rss_bytes) : '—'}
        />
        <Stat
          label="Served"
          value={status.requests_served ? formatCount(status.requests_served) : '—'}
        />
      </dl>

      <p className="mt-2 truncate text-xs text-[var(--lm-text-faint)]">
        {status.last_error
          ? status.last_error
          : status.ready_at
            ? `Ready since ${formatRelative(status.ready_at)}`
            : `Changed ${formatRelative(status.last_change_at)}`}
      </p>
    </Panel>
  );
}

function Stat({
  icon,
  label,
  value,
  title,
}: {
  icon?: ReactNode;
  label: string;
  value: string;
  title?: string | undefined;
}) {
  return (
    <div className="min-w-0" title={title}>
      <dt className="flex items-center gap-1 text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
        {icon ? (
          <span aria-hidden className="[&>svg]:size-3">
            {icon}
          </span>
        ) : null}
        {label}
      </dt>
      <dd className="lm-numeric mt-0.5 truncate text-[13px] text-[var(--lm-text)]">{value}</dd>
    </div>
  );
}
