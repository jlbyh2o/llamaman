/**
 * The fit calculator, read from the UI's side — DESIGN section 3.9, section 8.
 *
 * `POST /fit/estimate-batch` is a POST that behaves like a read: the same repository, files, flags
 * and device selection always produce the same report, and nothing on the host changes because it
 * was asked. So it is modeled as a query, keyed on exactly those inputs — which is what makes the
 * context-length and cache-type controls feel live: moving a control changes the key, and the
 * cached report for the previous setting is still there when it is moved back.
 *
 * Three rules from section 8 are load-bearing for anything that renders a report, and every helper
 * below exists to keep them from being re-derived incorrectly:
 *
 *  1. **The verdict is per-GPU, never a sum.** `verdict === 'fits'` ⟺ every `per_gpu[].ok`. A
 *     23 GB + 4 GB pair has 27 GB free and can still place nothing, because llama.cpp does not pool
 *     VRAM across devices. `required_vram_bytes` is a reporting total and is labeled as one.
 *  2. **Unknown VRAM is not zero (F14).** A device whose memory could not be read reports
 *     `free_bytes: null`, and `vram_unknown` on the report. The DTO's own comment is explicit: a
 *     consumer must render "unknown" rather than "won't run".
 *  3. **A quant the daemon could not peek is not a quant that does not fit.** The batch endpoint
 *     reports an unreadable file as `wont_run` with the reason in `notes` and no devices at all,
 *     rather than sinking the whole picker — so the UI has to tell that apart from a real refusal.
 */

import { useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { FitBatchReport, FitDevice, FitReport } from '../../api/types';
import { formatBytes } from '../../format';
import { compact, fitFlags } from './api';
import type { FitSettings } from './api';

export interface FitBatchInput {
  repoId: string;
  revision?: string | undefined;
  /** One file per quantization group — shard 1 carries the geometry (section 7.3). */
  files: string[];
  settings: FitSettings;
}

/**
 * `POST /api/v1/fit/estimate-batch` — one report per quant plus `recommended_file`, the largest
 * quantization that still fits. This is the query the whole quant picker is a rendering of.
 */
export function useFitBatch(input: FitBatchInput): UseQueryResult<FitBatchReport, Error> {
  const { repoId, revision, files, settings } = input;
  return useQuery({
    queryKey: queryKeys.list(
      'fit',
      compact({
        repo: repoId,
        revision,
        files: [...files].sort().join(','),
        ctx: settings.ctxSize,
        ctk: settings.cacheTypeK,
        ctv: settings.cacheTypeV,
        fa: settings.flashAttn,
        parallel: settings.parallel,
        gpus: [...settings.gpus].sort().join(','),
      }),
    ),
    queryFn: () =>
      api.post('/api/v1/fit/estimate-batch', {
        body: {
          repo_id: repoId,
          files,
          flags: fitFlags(settings),
          ...(revision === undefined ? {} : { revision }),
          ...(settings.gpus.length > 0 ? { gpus: settings.gpus } : {}),
        },
      }),
    enabled: repoId !== '' && files.length > 0,
    // Free VRAM moves when an instance starts, so a report is not kept for long — but it is kept
    // long enough that dragging a context slider back and forth does not re-probe the host.
    staleTime: 20_000,
    retry: false,
  });
}

/* -- reading one report ---------------------------------------------------- */

/**
 * What a report actually says, as one word.
 *
 * `unmeasured` and `unknown` are deliberately not folded into `wont_run`: the first means the
 * daemon could not read the GGUF header, the second means it could not read the card's memory, and
 * neither is evidence that the model would fail to load.
 */
export type FitOutcome = 'fits' | 'partial' | 'wont_run' | 'unknown' | 'unmeasured';

export function fitOutcome(report: FitReport): FitOutcome {
  if (report.per_gpu.length === 0 && report.weights_bytes === 0) return 'unmeasured';
  if (report.vram_unknown) return 'unknown';
  if (report.verdict === 'fits' || report.verdict === 'partial') return report.verdict;
  return 'wont_run';
}

export const OUTCOME_LABELS: Record<FitOutcome, string> = {
  fits: 'Fits in VRAM',
  partial: 'Partial offload',
  wont_run: "Won't run",
  unknown: 'VRAM unknown',
  unmeasured: 'Not measurable',
};

/**
 * The one-line explanation under the verdict.
 *
 * For `partial` this is the number that matters — how much spills to system RAM — and for
 * `wont_run` it is the device that is short, because section 8.7 requires naming that device rather
 * than reporting a total the user cannot act on.
 */
export function fitSummary(report: FitReport): string {
  switch (fitOutcome(report)) {
    case 'fits':
      // `-ngl` counts the output layer too, so a full offload of an L-layer model is L+1 — printing
      // "37 of 36" would look like an arithmetic bug rather than like a complete answer.
      return report.n_gpu_layers >= report.inputs.n_layer
        ? 'Every layer on GPU'
        : `${report.n_gpu_layers} of ${report.inputs.n_layer} layers on GPU`;
    case 'partial': {
      return `${report.n_gpu_layers} of ${report.inputs.n_layer} layers on GPU, the rest in system RAM`;
    }
    case 'wont_run': {
      const worst = shortestDevice(report);
      if (worst) return `GPU ${worst.index} is short by ${formatBytes(worst.short_by_bytes)}`;
      return 'No layer placement fits these devices';
    }
    case 'unknown':
      return 'At least one device would not report its free memory';
    case 'unmeasured':
      return report.notes[0] ?? 'This file could not be measured';
  }
}

/** The device that is furthest from fitting — the one section 8.7 says to name. */
export function shortestDevice(report: FitReport): FitDevice | null {
  let worst: FitDevice | null = null;
  for (const device of report.per_gpu) {
    if (device.ok) continue;
    if (!worst || device.short_by_bytes > worst.short_by_bytes) worst = device;
  }
  return worst;
}

/** Every device the reports mention, de-duplicated — the GPU picker's own option list. */
export function devicesOf(reports: readonly FitReport[]): FitDevice[] {
  const seen = new Map<string, FitDevice>();
  for (const report of reports) {
    for (const device of report.per_gpu) {
      if (!seen.has(device.uuid)) seen.set(device.uuid, device);
    }
  }
  return [...seen.values()].sort((a, b) => a.index - b.index);
}

/* -- the margin visualization --------------------------------------------- */

export interface FitSegment {
  id: 'weights' | 'kv' | 'compute' | 'overhead' | 'margin' | 'reserve';
  label: string;
  bytes: number;
}

/**
 * `assigned(g, n)` broken into the terms of section 8.4, in the order they are charged.
 *
 * The safety margin and the caller's reserve are shown as their own bands rather than folded into
 * the total, because the whole point of `fit.margin_mib` is that it is headroom the estimate is
 * deliberately *not* spending — a bar that hid it would make a comfortable fit and a fit that only
 * survives because of the margin look identical.
 */
export function deviceSegments(device: FitDevice): FitSegment[] {
  const all: FitSegment[] = [
    { id: 'weights', label: 'Weights', bytes: device.weights_bytes },
    { id: 'kv', label: 'KV cache', bytes: device.kv_bytes },
    { id: 'compute', label: 'Compute buffers', bytes: device.extra_bytes },
    { id: 'overhead', label: 'Backend overhead', bytes: device.backend_overhead_bytes },
    { id: 'margin', label: 'Safety margin', bytes: device.margin_bytes },
    { id: 'reserve', label: 'Reserved', bytes: device.reserve_bytes },
  ];
  return all.filter((segment) => segment.bytes > 0);
}

/**
 * The denominator for one device's bar.
 *
 * Free VRAM when it is known, because "will it fit" is a question about free memory, not installed
 * memory. When the device is over its budget the scale grows to the assignment itself, so the
 * overflow is visible as overflow instead of being clipped at the end of the bar.
 */
export function deviceScale(device: FitDevice): number | null {
  const free = device.free_bytes;
  if (free === null || free === undefined) return null;
  return Math.max(free, device.assigned_bytes);
}
