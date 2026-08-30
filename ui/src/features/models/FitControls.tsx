/**
 * The controls the fit estimate is a function of.
 *
 * SPEC section 3.2 names the KV cache formula in full —
 * `n_layer × n_ctx × n_head_kv × (head_dim_k + head_dim_v) × bytes/elem` — and two of its terms are
 * things a person chooses rather than things a model has: the context length, and the `-ctk`/`-ctv`
 * cache types that set `bytes/elem`. Those are the controls here, plus the two that change the
 * other side of section 8.4's arithmetic: parallel slots (the KV cache is per slot) and flash
 * attention (which halves the attention compute buffer, and is what makes a quantized V cache legal
 * at all on most builds).
 *
 * Every change re-keys the batch query, so the whole quant table re-verdicts. That is the point:
 * "will a 32k context still fit" is one control away from "will an 8k context still fit", and
 * finding out costs a cached round trip rather than a download.
 */

import { RotateCcw } from 'lucide-react';

import { Button, Panel, PanelHeader, Select } from '../../components';
import { cn } from '../../components/cn';
import type { FitDevice } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { CACHE_TYPES, CTX_CHOICES, DEFAULT_FIT_SETTINGS, cacheTypeWarning } from './api';
import type { FitSettings } from './api';

export interface FitControlsProps {
  settings: FitSettings;
  onChange: (next: FitSettings) => void;
  /** The devices the last report saw. Empty until the first estimate returns. */
  devices: readonly FitDevice[];
  /** `n_ctx_train` from the Hub's `gguf` summary, offered as a choice when it is known. */
  trainedCtx?: number | null;
  busy?: boolean;
  className?: string;
}

export function FitControls({
  settings,
  onChange,
  devices,
  trainedCtx,
  busy = false,
  className,
}: FitControlsProps) {
  const set = <K extends keyof FitSettings>(key: K, value: FitSettings[K]) =>
    onChange({ ...settings, [key]: value });

  const ctxValues = [...new Set([...CTX_CHOICES, ...(trainedCtx ? [trainedCtx] : [])])].sort(
    (a, b) => a - b,
  );
  const warning = cacheTypeWarning(settings);

  const toggleGpu = (uuid: string) => {
    // An empty selection means "every present device" to the API, so the UI has to expand it before
    // it can subtract from it — otherwise the first click would deselect nothing.
    const current = settings.gpus.length > 0 ? settings.gpus : devices.map((device) => device.uuid);
    const next = current.includes(uuid) ? current.filter((id) => id !== uuid) : [...current, uuid];
    // Deselecting the last device would send an empty list, which the API reads as "every present
    // device" — the exact opposite of what the click asked for. So the last one cannot be turned
    // off; "estimate against no GPU at all" is not a question this control can pose.
    if (next.length === 0) return;
    // Selecting everything is the same statement as selecting nothing, and the shorter one keeps
    // the URL and the query key stable.
    set('gpus', next.length === devices.length ? [] : next);
  };

  const selected = (uuid: string) => settings.gpus.length === 0 || settings.gpus.includes(uuid);

  return (
    <Panel className={cn('space-y-4', className)}>
      <PanelHeader
        title="Fit estimate"
        description="Weights, KV cache, compute buffers and safety margin against this host's live VRAM."
        actions={
          <Button
            size="sm"
            variant="ghost"
            icon={<RotateCcw />}
            onClick={() => onChange({ ...DEFAULT_FIT_SETTINGS })}
            disabled={busy}
          >
            Reset
          </Button>
        }
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <label className="block">
          <span className="flex items-baseline gap-2 text-xs font-medium text-[var(--lm-text)]">
            Context length
            <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">--ctx-size</span>
          </span>
          <Select
            className="mt-1"
            mono
            value={String(settings.ctxSize)}
            onValueChange={(value) => set('ctxSize', Number(value))}
            disabled={busy}
            aria-label="Context length"
            options={ctxValues.map((value) => ({
              value: String(value),
              label: formatCount(value),
              ...(value === trainedCtx ? { description: 'Trained context length' } : {}),
            }))}
          />
        </label>

        <label className="block">
          <span className="flex items-baseline gap-2 text-xs font-medium text-[var(--lm-text)]">
            K cache type
            <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">-ctk</span>
          </span>
          <Select
            className="mt-1"
            mono
            value={settings.cacheTypeK}
            onValueChange={(value) => set('cacheTypeK', value)}
            disabled={busy}
            aria-label="K cache type"
            options={CACHE_TYPES.map((type) => ({
              value: type.value,
              label: type.label,
              description: type.note,
            }))}
          />
        </label>

        <label className="block">
          <span className="flex items-baseline gap-2 text-xs font-medium text-[var(--lm-text)]">
            V cache type
            <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">-ctv</span>
          </span>
          <Select
            className="mt-1"
            mono
            value={settings.cacheTypeV}
            onValueChange={(value) => set('cacheTypeV', value)}
            disabled={busy}
            aria-label="V cache type"
            options={CACHE_TYPES.map((type) => ({
              value: type.value,
              label: type.label,
              description: type.note,
            }))}
          />
        </label>

        <div className="grid grid-cols-2 gap-3">
          <label className="block">
            <span className="flex items-baseline gap-2 text-xs font-medium text-[var(--lm-text)]">
              Flash attn
              <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">-fa</span>
            </span>
            <Select
              className="mt-1"
              mono
              value={settings.flashAttn}
              onValueChange={(value) => set('flashAttn', value as FitSettings['flashAttn'])}
              disabled={busy}
              aria-label="Flash attention"
              options={[
                { value: 'on', label: 'on' },
                { value: 'off', label: 'off' },
                { value: 'auto', label: 'auto' },
              ]}
            />
          </label>

          <label className="block">
            <span className="flex items-baseline gap-2 text-xs font-medium text-[var(--lm-text)]">
              Slots
              <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">-np</span>
            </span>
            <Select
              className="mt-1"
              mono
              value={String(settings.parallel)}
              onValueChange={(value) => set('parallel', Number(value))}
              disabled={busy}
              aria-label="Parallel slots"
              options={[1, 2, 4, 8, 16].map((value) => ({
                value: String(value),
                label: String(value),
              }))}
            />
          </label>
        </div>
      </div>

      {devices.length > 1 ? (
        <div>
          <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
            Participating GPUs
          </p>
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {devices.map((device) => (
              <button
                key={device.uuid || String(device.index)}
                type="button"
                onClick={() => toggleGpu(device.uuid)}
                aria-pressed={selected(device.uuid)}
                disabled={busy}
                className={cn(
                  'rounded-[var(--lm-radius-full)] px-2.5 py-1 text-xs ring-1 ring-inset',
                  'transition-colors duration-[var(--lm-duration-fast)] disabled:opacity-50',
                  selected(device.uuid)
                    ? 'bg-[var(--lm-accent-soft)] text-[var(--lm-accent)] ring-[var(--lm-accent)]/35'
                    : 'bg-[var(--lm-neutral-soft)] text-[var(--lm-text-muted)] ring-[var(--lm-border-strong)]',
                )}
              >
                <span className="lm-numeric">GPU {device.index}</span> {device.name}
                {device.free_bytes !== null && device.free_bytes !== undefined ? (
                  <span className="lm-numeric ml-1.5 text-[var(--lm-text-faint)]">
                    {formatBytes(device.free_bytes)} free
                  </span>
                ) : null}
              </button>
            ))}
          </div>
          <p className="mt-1.5 text-xs text-[var(--lm-text-muted)]">
            Every layer lands on a specific device, so a quant fits only when it fits on each
            selected card. Nothing is pooled across them.
          </p>
        </div>
      ) : null}

      {warning ? (
        <p className="rounded-[var(--lm-radius)] border border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)] px-3 py-2 text-xs text-[var(--lm-text)]">
          {warning}
        </p>
      ) : null}
    </Panel>
  );
}
