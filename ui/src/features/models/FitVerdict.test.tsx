/**
 * The fit verdict tells the truth in all five of its states.
 *
 * This is the one rendering in the models area where being wrong is expensive: a person decides
 * whether to spend twenty gigabytes of bandwidth on what this component says. SPEC section 3.2 asks
 * for three verdicts — fits in VRAM, partial offload, won't run — and DESIGN sections 3.9 and 8.7
 * add two failure modes that are *not* verdicts and must never be dressed as one:
 *
 *  - `vram_unknown`: a device would not report its free memory (F14 forbids substituting 0), so the
 *    honest answer is "unknown", not "won't run".
 *  - an unmeasurable file: the batch endpoint reports a quant whose GGUF header it could not read
 *    as `wont_run` with the reason in `notes` and no devices, rather than sinking the picker — so
 *    the UI has to tell that apart from a real refusal.
 *
 * Rendering goes through `react-dom/server`, which needs no DOM environment (see vitest.config.ts
 * for why that is deliberate). Every assertion below is about the words and the data attributes a
 * user or a screen reader would get, not about styling.
 */

import { renderToStaticMarkup } from 'react-dom/server';
import type { ReactElement } from 'react';
import { describe, expect, it } from 'vitest';

import type { FitDevice, FitReport } from '../../api/types';
import { FitDeviceBar, FitReportDetail, FitVerdict, FitVerdictBadge } from './FitVerdict';
import { deviceScale, deviceSegments, fitOutcome, fitSummary, shortestDevice } from './fit';

const GiB = 1024 ** 3;

function device(overrides: Partial<FitDevice> = {}): FitDevice {
  return {
    index: 0,
    uuid: 'GPU-0000',
    name: 'NVIDIA GeForce RTX 4090',
    free_bytes: 23 * GiB,
    total_bytes: 24 * GiB,
    assigned_bytes: 8 * GiB,
    ok: true,
    short_by_bytes: 0,
    weights_bytes: 5 * GiB,
    kv_bytes: 1 * GiB,
    extra_bytes: GiB / 2,
    backend_overhead_bytes: GiB / 4,
    margin_bytes: GiB,
    reserve_bytes: 0,
    ...overrides,
  };
}

function report(overrides: Partial<FitReport> = {}): FitReport {
  return {
    file: 'Qwen3-8B-Q4_K_M.gguf',
    weights_bytes: 5 * GiB,
    weights_offloaded_bytes: 5 * GiB,
    kv_bytes: GiB,
    kv_offloaded_bytes: GiB,
    kv_swa_bytes: 0,
    compute_bytes: GiB / 2,
    compute_act_bytes: GiB / 8,
    compute_attn_bytes: GiB / 8,
    compute_logits_bytes: GiB / 8,
    compute_moe_bytes: 0,
    backend_overhead_bytes: GiB / 4,
    margin_bytes: GiB,
    margin_bytes_per_gpu: GiB,
    reserve_bytes: 0,
    reserve_bytes_per_gpu: 0,
    required_vram_bytes: 8 * GiB,
    spill_to_ram_bytes: 0,
    system_ram_free_bytes: 48 * GiB,
    system_ram_known: true,
    per_gpu: [device()],
    verdict: 'fits',
    n_gpu_layers: 37,
    max_n_gpu_layers: 37,
    max_ctx_at_full_offload: 32768,
    per_slot_ctx: 8192,
    confidence: 'modeled',
    calibration_samples: 0,
    calibration_clamped: false,
    vram_unknown: false,
    notes: [],
    recommendation: {
      n_gpu_layers: 37,
      flash_attn: true,
      type_k: 'q8_0',
      type_v: 'q8_0',
      reason: 'full offload fits',
    },
    inputs: {
      arch: 'qwen3',
      n_layer: 36,
      n_layer_swa: 0,
      n_head_kv: [8],
      n_head: 32,
      head_dim_k: 128,
      head_dim_v: 128,
      n_ctx: 8192,
      kv_ctx: 8192,
      n_batch: 2048,
      n_ubatch: 512,
      n_parallel: 1,
      n_embd: 4096,
      n_ff: 14336,
      n_vocab: 151936,
      n_expert: 0,
      n_expert_used: 0,
      type_k: 'f16',
      type_v: 'f16',
      flash_attn: true,
    },
    ...overrides,
  };
}

/** The three states the daemon can actually answer with, plus the two it answers around. */
const FITS = report();

const PARTIAL = report({
  verdict: 'partial',
  n_gpu_layers: 22,
  spill_to_ram_bytes: 3 * GiB,
  per_gpu: [device({ ok: false, assigned_bytes: 26 * GiB, short_by_bytes: 3 * GiB })],
});

const WONT_RUN = report({
  verdict: 'wont_run',
  n_gpu_layers: 0,
  per_gpu: [
    device(),
    device({
      index: 1,
      uuid: 'GPU-0001',
      name: 'NVIDIA RTX A2000',
      free_bytes: 4 * GiB,
      total_bytes: 6 * GiB,
      assigned_bytes: 8 * GiB,
      ok: false,
      short_by_bytes: 4 * GiB,
    }),
  ],
  notes: ['GPU 1 is short by 4.0 GiB — try `--tensor-split 0.85,0.15`'],
});

const VRAM_UNKNOWN = report({
  verdict: 'wont_run',
  vram_unknown: true,
  per_gpu: [device({ free_bytes: null, total_bytes: null, ok: false })],
  notes: ['nvidia-smi did not answer; free memory is unknown rather than zero'],
});

const UNMEASURED = report({
  verdict: 'wont_run',
  weights_bytes: 0,
  weights_offloaded_bytes: 0,
  per_gpu: [],
  notes: ['the Hub would not serve a range request for this file'],
});

function render(node: ReactElement, theme: 'dark' | 'light' = 'dark'): string {
  return renderToStaticMarkup(<div data-theme={theme}>{node}</div>);
}

describe('fitOutcome', () => {
  it('reports the three verdicts unchanged', () => {
    expect(fitOutcome(FITS)).toBe('fits');
    expect(fitOutcome(PARTIAL)).toBe('partial');
    expect(fitOutcome(WONT_RUN)).toBe('wont_run');
  });

  it('never calls unreadable VRAM a refusal', () => {
    // The daemon still has to put *something* in `verdict`, and it puts `wont_run` there. The
    // component's job is to notice `vram_unknown` first (F14).
    expect(VRAM_UNKNOWN.verdict).toBe('wont_run');
    expect(fitOutcome(VRAM_UNKNOWN)).toBe('unknown');
  });

  it('never calls an unmeasurable file a refusal', () => {
    expect(UNMEASURED.verdict).toBe('wont_run');
    expect(fitOutcome(UNMEASURED)).toBe('unmeasured');
  });
});

describe('FitVerdictBadge', () => {
  const cases: [FitReport, string][] = [
    [FITS, 'Fits in VRAM'],
    [PARTIAL, 'Partial offload'],
    [WONT_RUN, 'Won&#x27;t run'],
    [VRAM_UNKNOWN, 'VRAM unknown'],
    [UNMEASURED, 'Not measurable'],
  ];

  for (const [subject, label] of cases) {
    it(`labels ${fitOutcome(subject)} as "${label}"`, () => {
      expect(render(<FitVerdictBadge report={subject} />)).toContain(label);
    });
  }

  it('does not say "won\'t run" for either non-verdict', () => {
    expect(render(<FitVerdictBadge report={VRAM_UNKNOWN} />)).not.toContain('run');
    expect(render(<FitVerdictBadge report={UNMEASURED} />)).not.toContain('run');
  });

  it('renders identically in both themes', () => {
    for (const [subject] of cases) {
      const dark = render(<FitVerdictBadge report={subject} />, 'dark');
      const light = render(<FitVerdictBadge report={subject} />, 'light');
      expect(dark.replace('data-theme="dark"', '')).toBe(light.replace('data-theme="light"', ''));
    }
  });
});

describe('FitVerdict', () => {
  it('carries the outcome as a data attribute, so a table row can be styled from it', () => {
    expect(render(<FitVerdict report={PARTIAL} />)).toContain('data-outcome="partial"');
  });

  it('shows the recommended marker only when this row is the recommendation', () => {
    expect(render(<FitVerdict report={FITS} recommended />)).toContain('Recommended');
    expect(render(<FitVerdict report={FITS} />)).not.toContain('Recommended');
  });

  it('summarizes a fit by how much of the model lands on the GPU', () => {
    // `-ngl` counts the output layer, so a full offload is n_layer + 1 and must not read "37 of 36".
    expect(fitSummary(FITS)).toBe('Every layer on GPU');
    expect(fitSummary(report({ n_gpu_layers: 20 }))).toBe('20 of 36 layers on GPU');
    expect(fitSummary(PARTIAL)).toContain('the rest in system RAM');
  });

  it('names the device that is short rather than reporting a total', () => {
    // Section 8.7: "notes names that device … rather than reporting a total the user cannot act on".
    expect(shortestDevice(WONT_RUN)?.index).toBe(1);
    expect(fitSummary(WONT_RUN)).toBe('GPU 1 is short by 4.00 GiB');
    expect(fitSummary(WONT_RUN)).not.toContain('8.00 GiB');
  });

  it('explains an unmeasurable file with the daemon’s own reason', () => {
    expect(fitSummary(UNMEASURED)).toBe('the Hub would not serve a range request for this file');
  });
});

describe('the margin visualization', () => {
  it('breaks a device into the terms of section 8.4, dropping the empty ones', () => {
    const segments = deviceSegments(device());
    expect(segments.map((segment) => segment.id)).toEqual([
      'weights',
      'kv',
      'compute',
      'overhead',
      'margin',
    ]);
    // reserve_bytes is 0 here, so it is not drawn — and the margin IS, because headroom the
    // estimate deliberately does not spend has to stay visible.
    expect(segments.some((segment) => segment.id === 'reserve')).toBe(false);
    expect(segments.some((segment) => segment.id === 'margin')).toBe(true);
  });

  it('scales a comfortable device to its free memory', () => {
    expect(deviceScale(device())).toBe(23 * GiB);
  });

  it('grows the scale past free memory so an overflow is drawn rather than clipped', () => {
    const over = device({ free_bytes: 4 * GiB, assigned_bytes: 8 * GiB, ok: false });
    expect(deviceScale(over)).toBe(8 * GiB);
  });

  it('has no scale at all when the device would not report its memory', () => {
    expect(deviceScale(device({ free_bytes: null }))).toBeNull();
  });

  it('states the shortfall in words beside the bar', () => {
    const html = render(
      <FitDeviceBar
        device={device({ ok: false, assigned_bytes: 8 * GiB, short_by_bytes: 4 * GiB })}
      />,
    );
    expect(html).toContain('Short by');
    expect(html).toContain('4.00 GiB');
  });

  it('labels a bar for a screen reader with the numbers it draws', () => {
    const html = render(<FitDeviceBar device={device()} />);
    expect(html).toContain(
      'aria-label="GPU 0 NVIDIA GeForce RTX 4090: 8.00 GiB assigned of 23.00 GiB free"',
    );
  });

  it('says "unknown" rather than a number for an unreadable device', () => {
    const html = render(<FitDeviceBar device={device({ free_bytes: null, total_bytes: null })} />);
    expect(html).toContain('free memory unknown');
    expect(html).not.toContain('0 B');
  });
});

describe('FitReportDetail', () => {
  it('labels the summed VRAM as a total across devices, never as the test', () => {
    const html = render(<FitReportDetail report={WONT_RUN} />);
    expect(html).toContain('Total across 2 GPUs');
  });

  it('renders one bar per participating device', () => {
    const html = render(<FitReportDetail report={WONT_RUN} />);
    expect(html).toContain('NVIDIA GeForce RTX 4090');
    expect(html).toContain('NVIDIA RTX A2000');
  });

  it('surfaces the daemon’s notes', () => {
    expect(render(<FitReportDetail report={WONT_RUN} />)).toContain('tensor-split');
  });

  it('replaces the whole breakdown with the reason when nothing could be measured', () => {
    const html = render(<FitReportDetail report={UNMEASURED} />);
    expect(html).toContain('could not be measured');
    expect(html).toContain('range request');
    expect(html).not.toContain('Recommendation');
  });

  it('shows how a calibrated estimate earned that word', () => {
    const calibrated = report({ confidence: 'calibrated', calibration_samples: 12 });
    expect(render(<FitReportDetail report={calibrated} />)).toContain('12 observations');
  });

  it('names no color of its own — every one is a token', () => {
    const html = render(<FitReportDetail report={WONT_RUN} />);
    expect(html).not.toMatch(/#[0-9a-f]{3,8}\b/i);
    expect(html).not.toMatch(/(?<!var\([^)]*)\b(?:rgb|hsl)a?\(/i);
  });
});
