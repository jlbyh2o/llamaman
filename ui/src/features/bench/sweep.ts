/**
 * The sweep document.
 *
 * `POST /bench/runs` and `GET /bench/preflight` both take the sweep as a JSON-encoded string
 * (DESIGN section 3.13's own example: `"sweep":{"n_gpu_layers":[0,20,"all"],"n_batch":[512,2048],
 * "flash_attn":[true,false],"type_k":["f16","q8_0"],"tests":[{"pp":512},{"tg":128},…]}`). This module
 * is the client's one definition of that shape — the sweep builder edits a `Sweep`, and everything
 * that talks to the API goes through `encodeSweep`.
 *
 * Every axis is optional and means "one point, the model's default" when absent or empty — which is
 * why `sweepPointCount` multiplies lengths with a floor of 1 rather than rejecting an empty axis.
 * `tests` is the one axis that always contributes at least one point, because a sweep with no test
 * measures nothing (§10.1: `-p`/`-n`/`-d` come from the sweep point, not the FlagSet).
 */

/** One `llama-bench` invocation shape: prompt length, generation length, and context depth. */
export interface SweepTest {
  pp?: number;
  tg?: number;
  depth?: number;
}

export interface Sweep {
  n_gpu_layers?: (number | 'all' | 'none')[];
  n_batch?: number[];
  n_ubatch?: number[];
  n_threads?: number[];
  flash_attn?: boolean[];
  type_k?: string[];
  type_v?: string[];
  split_mode?: string[];
  tensor_split?: string[];
  tests?: SweepTest[];
}

/** `-ctk`/`-ctv` vocabulary llama.cpp actually accepts (§10.1: "direct, same vocabulary"). */
export const CACHE_TYPES = [
  'f16',
  'f32',
  'bf16',
  'q8_0',
  'q4_0',
  'q4_1',
  'q5_0',
  'q5_1',
  'iq4_nl',
] as const;

export const SPLIT_MODES = ['none', 'layer', 'row'] as const;

/** A single test that measures generation only — the sweep builder's starting point. */
export const DEFAULT_SWEEP: Sweep = { tests: [{ tg: 128 }] };

/** Soft, client-side guidance only — the server's `422 sweep_too_large` is the real limit. */
export const SWEEP_SOFT_CAP = 300;

function len(axis: readonly unknown[] | undefined): number {
  return axis && axis.length > 0 ? axis.length : 1;
}

/** The cross-product size: what `bench_points` would expand to (§10: "before anything executes"). */
export function sweepPointCount(sweep: Sweep): number {
  return (
    len(sweep.n_gpu_layers) *
    len(sweep.n_batch) *
    len(sweep.n_ubatch) *
    len(sweep.n_threads) *
    len(sweep.flash_attn) *
    len(sweep.type_k) *
    len(sweep.type_v) *
    len(sweep.split_mode) *
    len(sweep.tensor_split) *
    Math.max(1, sweep.tests?.length ?? 0)
  );
}

/** Drop empty arrays so the wire form matches "an absent axis is one default point" exactly. */
function pruned(sweep: Sweep): Sweep {
  const out: Sweep = {};
  for (const [key, value] of Object.entries(sweep) as [keyof Sweep, unknown][]) {
    if (Array.isArray(value) && value.length > 0) (out as Record<string, unknown>)[key] = value;
  }
  return out;
}

/** The JSON string the API expects, or undefined for an all-default sweep (a single point). */
export function encodeSweep(sweep: Sweep): string | undefined {
  const clean = pruned(sweep);
  if (Object.keys(clean).length === 0) return undefined;
  return JSON.stringify(clean);
}

export function decodeSweep(json: string | null | undefined): Sweep {
  if (!json) return {};
  try {
    const parsed: unknown = JSON.parse(json);
    return typeof parsed === 'object' && parsed !== null ? (parsed as Sweep) : {};
  } catch {
    return {};
  }
}

/** Parse a comma-separated numeric axis: "0, 20, 40" -> [0, 20, 40]. Blank entries are dropped. */
export function parseNumberList(text: string): number[] {
  return text
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part.length > 0)
    .map(Number)
    .filter((n) => Number.isFinite(n));
}

/** Parse the one axis that mixes numbers with the `all`/`none` literals (§10.1: `-ngl`). */
export function parseNGpuLayersList(text: string): (number | 'all' | 'none')[] {
  return text
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part.length > 0)
    .map((part) => {
      const lower = part.toLowerCase();
      if (lower === 'all' || lower === 'none') return lower;
      return Number(part);
    })
    .filter((v) => v === 'all' || v === 'none' || Number.isFinite(v));
}

/** Parse a comma-separated string axis, trimmed and de-blanked: type_k, split_mode, tensor_split. */
export function parseStringList(text: string): string[] {
  return text
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part.length > 0);
}

export function formatList(values: readonly (string | number)[] | undefined): string {
  return (values ?? []).join(', ');
}
