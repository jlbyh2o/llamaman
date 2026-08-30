/**
 * The fit verdict, rendered.
 *
 * SPEC section 3.2 asks for one thing here and asks for it precisely: a verdict per quantization,
 * per detected GPU — *fits in VRAM / partial offload / won't run* — with the memory that produced
 * it visible and a recommended quant highlighted. This file is that rendering, and it is a pure
 * function of a `FitReportDTO`: it fetches nothing, so the same component serves the quant picker,
 * an expanded row, and the component tests.
 *
 * Five states, not three. `unknown` (a device that would not report its free memory) and
 * `unmeasured` (a file whose GGUF header the daemon could not read) are outcomes of the *report*,
 * not of the model, and rendering either as "won't run" would be a confident lie — F14 and section
 * 3.9's `vram_unknown` exist precisely so that the difference survives to here.
 *
 * The bar is per device because the test is per device (section 8.7): `∀ g : assigned(g) ≤ free(g)
 * − reserve(g)`. There is deliberately no aggregate bar, because a single bar over summed VRAM
 * would say "fits" for a 23 GB + 4 GB pair that can place nothing.
 */

import type { ReactNode } from 'react';
import { AlertTriangle, Sparkles } from 'lucide-react';

import { Badge, StatusBadge } from '../../components';
import { cn } from '../../components/cn';
import type { FitDevice, FitReport } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { deviceScale, deviceSegments, fitOutcome, fitSummary, OUTCOME_LABELS } from './fit';
import type { FitOutcome, FitSegment } from './fit';

/* -- the badge ------------------------------------------------------------- */

/**
 * The three verdicts keep the colors `StatusBadge`'s `fit` map already decides for them. The two
 * non-verdicts get a neutral badge of their own: they are the absence of an answer, and dressing
 * either in the danger tone would put them on the same footing as a refusal.
 */
export function FitVerdictBadge({ report, className }: { report: FitReport; className?: string }) {
  const outcome = fitOutcome(report);
  if (outcome === 'unknown' || outcome === 'unmeasured') {
    return (
      <Badge tone="neutral" dot className={className} data-outcome={outcome}>
        {OUTCOME_LABELS[outcome]}
      </Badge>
    );
  }
  return (
    <StatusBadge kind="fit" state={outcome} {...(className === undefined ? {} : { className })} />
  );
}

/** The "largest quantization that still fits" marker of section 3.9's `recommended_file`. */
export function RecommendedBadge({ className }: { className?: string }) {
  return (
    <Badge tone="accent" icon={<Sparkles />} className={className}>
      Recommended
    </Badge>
  );
}

/* -- the compact cell ------------------------------------------------------ */

export interface FitVerdictProps {
  report: FitReport;
  /** Marks this row as `recommended_file`. */
  recommended?: boolean;
  className?: string;
}

/** Badge plus the one line that explains it — what a table cell shows without being expanded. */
export function FitVerdict({ report, recommended = false, className }: FitVerdictProps) {
  const outcome = fitOutcome(report);
  return (
    <div className={cn('flex flex-col items-start gap-1', className)} data-outcome={outcome}>
      <div className="flex flex-wrap items-center gap-1.5">
        <FitVerdictBadge report={report} />
        {recommended ? <RecommendedBadge /> : null}
      </div>
      <p className="text-xs text-[var(--lm-text-muted)]">{fitSummary(report)}</p>
    </div>
  );
}

/* -- the margin visualization --------------------------------------------- */

const SEGMENT_COLOR: Record<FitSegment['id'], string> = {
  weights: 'var(--lm-chart-1)',
  kv: 'var(--lm-chart-2)',
  compute: 'var(--lm-chart-3)',
  overhead: 'var(--lm-chart-4)',
  margin: 'var(--lm-chart-5)',
  reserve: 'var(--lm-chart-6)',
};

/**
 * One device's bar.
 *
 * The scale is that device's *free* memory — the quantity the verdict is tested against — widened
 * to the assignment itself when the assignment is larger, so an overflow is drawn as overflow
 * rather than clipped at 100%. The free-memory line is marked, which is what turns "this is long"
 * into "this is 2.1 GB too long".
 */
export function FitDeviceBar({ device }: { device: FitDevice }) {
  const segments = deviceSegments(device);
  const scale = deviceScale(device);
  const free = device.free_bytes ?? null;
  const known = scale !== null && scale > 0;

  const label = known
    ? `GPU ${device.index} ${device.name}: ${formatBytes(device.assigned_bytes)} assigned of ${formatBytes(free)} free`
    : `GPU ${device.index} ${device.name}: free memory unknown`;

  return (
    <div className="space-y-1">
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="truncate text-[var(--lm-text)]">
          <span className="lm-numeric text-[var(--lm-text-faint)]">GPU {device.index}</span>{' '}
          {device.name}
        </span>
        <span className="lm-numeric shrink-0 text-[var(--lm-text-muted)]">
          {formatBytes(device.assigned_bytes)}
          {known ? ` / ${formatBytes(free)} free` : ' / unknown'}
        </span>
      </div>

      <div
        role="img"
        aria-label={label}
        className={cn(
          'relative h-3 w-full overflow-hidden rounded-[var(--lm-radius-sm)]',
          'bg-[var(--lm-surface-sunken)] ring-1 ring-inset',
          device.ok ? 'ring-[var(--lm-border)]' : 'ring-[var(--lm-danger)]/50',
        )}
      >
        {known ? (
          <>
            <div className="flex h-full w-full">
              {segments.map((segment) => (
                <div
                  key={segment.id}
                  // A native title, not a Radix tooltip: a 3-pixel band is not a focus target, the
                  // legend below carries the same numbers in text, and this keeps the bar renderable
                  // without a provider — which is what lets the component tests mount it bare.
                  title={`${segment.label}: ${formatBytes(segment.bytes)}`}
                  className="h-full min-w-px"
                  style={{
                    width: `${(segment.bytes / scale) * 100}%`,
                    background: SEGMENT_COLOR[segment.id],
                    opacity: segment.id === 'margin' || segment.id === 'reserve' ? 0.45 : 1,
                  }}
                />
              ))}
            </div>
            {free !== null && free < device.assigned_bytes ? (
              <span
                aria-hidden
                className="absolute inset-y-0 w-px bg-[var(--lm-danger)]"
                style={{ left: `${(free / scale) * 100}%` }}
              />
            ) : null}
          </>
        ) : (
          <div className="h-full w-full animate-pulse bg-[var(--lm-neutral-soft)]" />
        )}
      </div>

      {!device.ok && device.short_by_bytes > 0 ? (
        <p className="flex items-center gap-1.5 text-xs text-[var(--lm-danger)]">
          <AlertTriangle aria-hidden className="size-3.5 shrink-0" />
          Short by {formatBytes(device.short_by_bytes)}
        </p>
      ) : null}
    </div>
  );
}

/** The legend for the bands, so a colored strip is a breakdown rather than decoration. */
export function FitLegend({ segments }: { segments: readonly FitSegment[] }) {
  if (segments.length === 0) return null;
  return (
    <ul className="flex flex-wrap gap-x-3 gap-y-1">
      {segments.map((segment) => (
        <li
          key={segment.id}
          className="flex items-center gap-1.5 text-xs text-[var(--lm-text-muted)]"
        >
          <span
            aria-hidden
            className="size-2 shrink-0 rounded-[var(--lm-radius-sm)]"
            style={{
              background: SEGMENT_COLOR[segment.id],
              opacity: segment.id === 'margin' || segment.id === 'reserve' ? 0.45 : 1,
            }}
          />
          {segment.label}
          <span className="lm-numeric text-[var(--lm-text-faint)]">
            {formatBytes(segment.bytes)}
          </span>
        </li>
      ))}
    </ul>
  );
}

/* -- the full report ------------------------------------------------------- */

function Term({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">{label}</dt>
      <dd className="lm-numeric mt-0.5 truncate text-[13px] text-[var(--lm-text)]">{children}</dd>
    </div>
  );
}

export interface FitReportDetailProps {
  report: FitReport;
  className?: string;
}

/**
 * Everything one report knows: the per-device bars, the terms behind them, the recommendation, and
 * the notes. `required_vram_bytes` appears here labeled "across N GPUs" and never as a verdict —
 * section 8.7 is explicit that it is a reporting total and that nothing may test against it.
 */
export function FitReportDetail({ report, className }: FitReportDetailProps) {
  const outcome = fitOutcome(report);

  if (outcome === 'unmeasured') {
    return (
      <div
        className={cn('space-y-2', className)}
        data-outcome={outcome}
        data-verdict={report.verdict}
      >
        <p className="text-sm text-[var(--lm-text)]">This file could not be measured.</p>
        <ul className="space-y-1 text-xs text-[var(--lm-text-muted)]">
          {report.notes.map((note) => (
            <li key={note}>{note}</li>
          ))}
        </ul>
        <p className="text-xs text-[var(--lm-text-faint)]">
          The estimate reads the GGUF header over an HTTP range request before anything is
          downloaded. A repository that will not serve one can still be downloaded — the numbers
          become exact once the first shard lands.
        </p>
      </div>
    );
  }

  const legend = report.per_gpu[0] ? deviceSegments(report.per_gpu[0]) : [];

  return (
    <div
      className={cn('space-y-4', className)}
      data-outcome={outcome}
      data-verdict={report.verdict}
    >
      {report.per_gpu.length > 0 ? (
        <div className="space-y-3">
          {report.per_gpu.map((device) => (
            <FitDeviceBar key={device.uuid || String(device.index)} device={device} />
          ))}
          <FitLegend segments={legend} />
        </div>
      ) : (
        <p className="text-sm text-[var(--lm-text-muted)]">
          No GPU was detected, so this is a system-RAM estimate.
        </p>
      )}

      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Term label="Weights">{formatBytes(report.weights_bytes)}</Term>
        <Term label="KV cache">
          {formatBytes(report.kv_bytes + report.kv_swa_bytes)}
          {report.kv_swa_bytes > 0 ? (
            <span className="text-[var(--lm-text-faint)]">
              {' '}
              incl. {formatBytes(report.kv_swa_bytes)} sliding
            </span>
          ) : null}
        </Term>
        <Term label="Compute buffers">{formatBytes(report.compute_bytes)}</Term>
        <Term label={`Total across ${formatCount(Math.max(1, report.per_gpu.length))} GPUs`}>
          {formatBytes(report.required_vram_bytes)}
        </Term>
        <Term label="Spill to RAM">{formatBytes(report.spill_to_ram_bytes)}</Term>
        <Term label="System RAM free">
          {report.system_ram_known ? formatBytes(report.system_ram_free_bytes) : 'unknown'}
        </Term>
        <Term label="Max layers on GPU">
          {formatCount(report.max_n_gpu_layers)} / {formatCount(report.inputs.n_layer)}
        </Term>
        <Term label="Max context, full offload">{formatCount(report.max_ctx_at_full_offload)}</Term>
      </dl>

      <div className="rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3">
        <p className="text-xs font-medium text-[var(--lm-text)]">
          Recommendation
          <span className="ml-2 font-normal text-[var(--lm-text-faint)]">
            confidence: {report.confidence}
            {report.confidence === 'calibrated'
              ? ` (${formatCount(report.calibration_samples)} observations)`
              : ''}
          </span>
        </p>
        <p className="lm-numeric mt-1 text-[13px] text-[var(--lm-text)]">
          -ngl {formatCount(report.recommendation.n_gpu_layers)} · -fa{' '}
          {report.recommendation.flash_attn ? 'on' : 'off'} · -ctk {report.recommendation.type_k} ·
          -ctv {report.recommendation.type_v}
          {report.recommendation.n_ctx ? ` · -c ${formatCount(report.recommendation.n_ctx)}` : ''}
        </p>
        {report.recommendation.reason ? (
          <p className="mt-1 text-xs text-[var(--lm-text-muted)]">{report.recommendation.reason}</p>
        ) : null}
      </div>

      {report.notes.length > 0 ? (
        <ul className="space-y-1">
          {report.notes.map((note) => (
            <li key={note} className="flex items-start gap-1.5 text-xs text-[var(--lm-text-muted)]">
              <AlertTriangle
                aria-hidden
                className="mt-0.5 size-3.5 shrink-0 text-[var(--lm-warn)]"
              />
              {note}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/** Exported for the tests, which assert the five outcomes render distinguishably. */
export type { FitOutcome };
