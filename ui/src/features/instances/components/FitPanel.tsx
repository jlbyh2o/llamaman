/**
 * The live fit panel.
 *
 * DESIGN section 4, screen 5: "a live **fit panel** that re-estimates on every change and a
 * rendered-argv preview underneath". Section 3.9 defines what it reads, and two of its sentences
 * decide how this renders:
 *
 *  - `required_vram_bytes` is "a TOTAL, never the test". The verdict is `∀ g: per_gpu[g].ok`, so the
 *    per-device rows are the answer and the total is context beneath them.
 *  - `margin_bytes_per_gpu` and `reserve_bytes_per_gpu` are charged to *every* participating device,
 *    "never divided among them" — so both are labeled per GPU here, beside the reporting totals.
 *
 * `confidence` is shown because a modeled estimate and a calibrated one are different claims
 * (section 8.7), and hiding the difference is how a fit calculator loses its credibility the first
 * time it is wrong.
 */

import { AlertTriangle, Gauge, Sparkles } from 'lucide-react';
import { Badge, Button, Meter, Panel, PanelHeader, StatusBadge } from '../../../components';
import { formatBytes, formatCount } from '../../../format';
import { ApiError } from '../../../api/errors';
import type { FitReport, FitRecommendation } from '../../../api/types';

export interface FitPanelProps {
  report?: FitReport | undefined;
  loading?: boolean;
  error?: unknown;
  /** Applies `recommendation` to the form: `-ngl`, `-fa` and the two cache types (section 3.9). */
  onApplyRecommendation?: (recommendation: FitRecommendation) => void;
  /** Shown when there is nothing to estimate yet. */
  placeholder?: string;
}

function Row({ label, value, muted }: { label: string; value: string; muted?: boolean }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-xs">
      <span className={muted ? 'text-[var(--lm-text-faint)]' : 'text-[var(--lm-text-muted)]'}>
        {label}
      </span>
      <span className="lm-numeric text-[var(--lm-text)]">{value}</span>
    </div>
  );
}

export function FitPanel({
  report,
  loading = false,
  error,
  onApplyRecommendation,
  placeholder = 'Pick a model to see where its memory goes.',
}: FitPanelProps) {
  if (error) {
    const unavailable = error instanceof ApiError && error.code === 'fit_unavailable';
    return (
      <Panel>
        <PanelHeader title="Fit" description="Section 8's estimate for this configuration" />
        <p className="mt-3 text-xs text-[var(--lm-text-muted)]">
          {unavailable
            ? 'This model’s GGUF header has not been parsed yet, so there is nothing to measure. ' +
              'The estimate appears once the download finishes and the header is read.'
            : error instanceof ApiError
              ? error.message
              : 'The estimate could not be computed.'}
        </p>
      </Panel>
    );
  }

  if (!report) {
    return (
      <Panel>
        <PanelHeader title="Fit" description="Section 8's estimate for this configuration" />
        <p className="mt-3 text-xs text-[var(--lm-text-muted)]">
          {loading ? 'Estimating…' : placeholder}
        </p>
      </Panel>
    );
  }

  const recommendation = report.recommendation;

  return (
    <Panel>
      <PanelHeader
        title="Fit"
        description={
          report.confidence === 'calibrated'
            ? `Calibrated against ${formatCount(report.calibration_samples)} observed loads`
            : 'Modeled — no observation from this host yet'
        }
        actions={<StatusBadge kind="fit" state={report.verdict} />}
      />

      {report.vram_unknown ? (
        <p className="mt-3 flex items-start gap-1.5 text-xs text-[var(--lm-warn)]">
          <AlertTriangle aria-hidden className="mt-0.5 size-3.5 shrink-0" />
          The GPUs could not be read, so this is a RAM-only verdict.
        </p>
      ) : null}

      <div className="mt-3 space-y-3">
        {report.per_gpu.map((gpu) => {
          const free = gpu.free_bytes ?? 0;
          const total = gpu.total_bytes ?? Math.max(free, gpu.assigned_bytes);
          return (
            <div key={gpu.uuid}>
              <Meter
                used={gpu.assigned_bytes}
                total={total > 0 ? total : gpu.assigned_bytes}
                label={
                  <span className="flex items-center gap-1.5">
                    <span className="truncate">
                      CUDA{gpu.index} · {gpu.name}
                    </span>
                    {gpu.ok ? null : (
                      <Badge tone="danger">short {formatBytes(gpu.short_by_bytes)}</Badge>
                    )}
                  </span>
                }
                detail={`${formatBytes(gpu.assigned_bytes)} of ${formatBytes(free)} free`}
              />
            </div>
          );
        })}
      </div>

      <div className="mt-4 space-y-1 border-t border-[var(--lm-border)] pt-3">
        <Row label="Weights (offloaded)" value={formatBytes(report.weights_offloaded_bytes)} />
        <Row label="KV cache" value={formatBytes(report.kv_bytes)} />
        {report.kv_swa_bytes > 0 ? (
          <Row label="KV cache (sliding window)" value={formatBytes(report.kv_swa_bytes)} muted />
        ) : null}
        <Row label="Compute buffers" value={formatBytes(report.compute_bytes)} />
        <Row
          label="Backend overhead, per GPU"
          value={formatBytes(report.backend_overhead_bytes)}
          muted
        />
        <Row
          label="Safety margin, per GPU"
          value={formatBytes(report.margin_bytes_per_gpu)}
          muted
        />
        {report.reserve_bytes_per_gpu > 0 ? (
          <Row label="Reserved, per GPU" value={formatBytes(report.reserve_bytes_per_gpu)} muted />
        ) : null}
        <Row label="Required VRAM, total" value={formatBytes(report.required_vram_bytes)} />
        {report.spill_to_ram_bytes > 0 ? (
          <Row label="Spilled to system RAM" value={formatBytes(report.spill_to_ram_bytes)} />
        ) : null}
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-[var(--lm-border)] pt-3">
        <Badge tone="neutral" icon={<Gauge />}>
          {report.max_n_gpu_layers} of {report.inputs.n_layer} layers fit
        </Badge>
        <Badge tone="neutral">{formatCount(report.per_slot_ctx)} tokens per slot</Badge>
        <Badge tone="neutral">
          max context at full offload {formatCount(report.max_ctx_at_full_offload)}
        </Badge>
      </div>

      {onApplyRecommendation ? (
        <div className="mt-3 flex items-start justify-between gap-3 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-raised)] p-3">
          <div className="min-w-0">
            <p className="text-xs font-medium text-[var(--lm-text)]">Recommended</p>
            <p className="lm-numeric mt-0.5 text-[12px] text-[var(--lm-text-muted)]">
              -ngl {recommendation.n_gpu_layers} · -fa {recommendation.flash_attn ? 'on' : 'off'} ·
              -ctk {recommendation.type_k} · -ctv {recommendation.type_v}
            </p>
            {recommendation.reason ? (
              <p className="mt-1 text-xs text-[var(--lm-text-muted)]">{recommendation.reason}</p>
            ) : null}
          </div>
          <Button
            size="sm"
            icon={<Sparkles />}
            onClick={() => onApplyRecommendation(recommendation)}
          >
            Apply
          </Button>
        </div>
      ) : null}

      {report.notes.length > 0 ? (
        <ul className="mt-3 space-y-1">
          {report.notes.map((note) => (
            <li key={note} className="flex items-start gap-1.5 text-xs text-[var(--lm-text-muted)]">
              <AlertTriangle aria-hidden className="mt-0.5 size-3 shrink-0 text-[var(--lm-warn)]" />
              {note}
            </li>
          ))}
        </ul>
      ) : null}
    </Panel>
  );
}
