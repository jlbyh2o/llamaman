/**
 * The shared plumbing of the models area.
 *
 * Three jobs, all of them small, all of them here so that no screen has to repeat them:
 *
 *  1. **List envelopes.** DESIGN section 3 fixes the list shape as `{items,total,next_cursor}`, and
 *     `api/types.ts` names it `ListPage<T>`. The generated `schema.d.ts` cannot: Go emits those
 *     schemas as `List[github.com/jlbyh2o/llamaman/internal/api.ModelDTO]`, and a `$ref` name
 *     containing dots is emitted as a chain of index accesses (`components["schemas"]["List[github.com"]["jlbyh2o"]…`)
 *     that resolves to nothing. So every list endpoint's success type degrades to `any`, and a
 *     screen that trusted it would compile no matter what it read. `asPage()` is the one place that
 *     re-imposes the shape — defensively, so a body that is not a page yields an empty page rather
 *     than a crash halfway down a render.
 *  2. **Optional query params.** `exactOptionalPropertyTypes` is on, so writing `{q: undefined}`
 *     into a `{q?: string}` parameter object is a type error. `compact()` drops the undefined keys
 *     and narrows the value type, which is exactly what the URL builder does at runtime anyway.
 *  3. **Search-param vocabularies.** The router hands every filter through as `string | undefined`
 *     (searchParams.ts deliberately `.catch()`es rather than throwing on a hand-edited URL), and the
 *     API's own enums are narrower. The `oneOf` guards below turn "whatever was in the URL" into
 *     "a value this endpoint accepts, or nothing".
 */

import type { FlagSet, ListPage } from '../../api/types';

/* -- list envelopes -------------------------------------------------------- */

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Impose `{items,total,next_cursor}` on a list body. A bare array is accepted as its items. */
export function asPage<T>(value: unknown): ListPage<T> {
  if (Array.isArray(value)) return { items: value as T[], total: value.length, next_cursor: null };
  if (!isRecord(value) || !Array.isArray(value['items'])) return { items: [], total: 0 };
  const items = value['items'] as T[];
  const total = typeof value['total'] === 'number' ? value['total'] : items.length;
  const cursor = value['next_cursor'];
  return { items, total, next_cursor: typeof cursor === 'string' ? cursor : null };
}

/* -- optional params ------------------------------------------------------- */

type Defined<T> = { [K in keyof T]?: Exclude<T[K], undefined> };

/** Drop the keys whose value is `undefined`, which `exactOptionalPropertyTypes` forbids writing. */
export function compact<T extends Record<string, unknown>>(input: T): Defined<T> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(input)) {
    if (value !== undefined) out[key] = value;
  }
  return out as Defined<T>;
}

/** `oneOf(MODEL_SORTS)('size')` → `'size'`; anything else → `undefined`. */
export function oneOf<const T extends readonly string[]>(
  values: T,
): (candidate: string | undefined) => T[number] | undefined {
  const set: ReadonlySet<string> = new Set(values);
  return (candidate) =>
    candidate !== undefined && set.has(candidate) ? (candidate as T[number]) : undefined;
}

/* -- the vocabularies the models area filters on --------------------------- */

export const MODEL_SORTS = ['repo', 'size', 'recent'] as const;
export const MODEL_KINDS = ['text', 'embedding', 'mmproj', 'unknown'] as const;
export const MODEL_FILTER_STATES = [
  'planned',
  'downloading',
  'incomplete',
  'verifying',
  'ready',
  'corrupt',
  'missing',
  'deleting',
  'deleted',
] as const;
export const HF_SORTS = ['downloads', 'likes', 'lastModified', 'trendingScore'] as const;

export const modelSort = oneOf(MODEL_SORTS);
export const modelKind = oneOf(MODEL_KINDS);
export const modelState = oneOf(MODEL_FILTER_STATES);
export const hfSort = oneOf(HF_SORTS);

/** What each `kind` means in a chip, in the product's own words rather than the column's. */
export const KIND_LABELS: Record<string, string> = {
  text: 'Text generation',
  embedding: 'Embeddings',
  mmproj: 'Projector',
  unknown: 'Unclassified',
};

/** `model_files.state` (DESIGN section 2.6) — a different enum from `models.state`. */
export const FILE_STATE_LABELS: Record<string, string> = {
  planned: 'Planned',
  downloading: 'Downloading',
  paused: 'Paused',
  verifying: 'Verifying',
  present: 'On disk',
  missing: 'Missing',
  corrupt: 'Corrupt',
};

/* -- the fit inputs a picker exposes --------------------------------------- */

/**
 * The knobs the quant picker offers, and the FlagSet they become.
 *
 * SPEC section 3.2 names the two that change the answer most: the context length and the
 * `-ctk`/`-ctv` cache types, because the KV cache is the term that scales with both. The rest are
 * here because section 8.3's KV formula reads them — `parallel` multiplies the cache, and flash
 * attention decides both the attention compute buffer (section 8.4) and whether a quantized V cache
 * is legal at all on most builds.
 */
export interface FitSettings {
  ctxSize: number;
  cacheTypeK: string;
  cacheTypeV: string;
  flashAttn: 'on' | 'off' | 'auto';
  parallel: number;
  /** Participating GPU UUIDs. Empty means every present device, which is the API's own default. */
  gpus: string[];
}

/** llama.cpp's `-ctk`/`-ctv` table (DESIGN section 8.3), with the sizes that make the choice. */
export const CACHE_TYPES: readonly { value: string; label: string; note: string }[] = [
  { value: 'f16', label: 'f16', note: '2 bytes per element — the default' },
  { value: 'bf16', label: 'bf16', note: '2 bytes per element' },
  { value: 'q8_0', label: 'q8_0', note: '1.06 bytes per element' },
  { value: 'q5_1', label: 'q5_1', note: '0.75 bytes per element' },
  { value: 'q5_0', label: 'q5_0', note: '0.69 bytes per element' },
  { value: 'q4_1', label: 'q4_1', note: '0.63 bytes per element' },
  { value: 'q4_0', label: 'q4_0', note: '0.56 bytes per element' },
  { value: 'iq4_nl', label: 'iq4_nl', note: '0.56 bytes per element' },
];

export const CTX_CHOICES = [2048, 4096, 8192, 16384, 32768, 65536, 131072] as const;

export const DEFAULT_FIT_SETTINGS: FitSettings = {
  ctxSize: 8192,
  cacheTypeK: 'f16',
  cacheTypeV: 'f16',
  flashAttn: 'on',
  parallel: 1,
  gpus: [],
};

/** The FlagSet subset `POST /fit/estimate*` takes (DESIGN section 3.9). */
export function fitFlags(settings: FitSettings): FlagSet {
  return {
    ctx_size: settings.ctxSize,
    cache_type_k: settings.cacheTypeK,
    cache_type_v: settings.cacheTypeV,
    flash_attn: settings.flashAttn,
    parallel: settings.parallel,
  };
}

/**
 * A quantized V cache needs flash attention on most builds (DESIGN section 8.7's note). The picker
 * says so *before* the estimate comes back, because the report would otherwise look like a
 * calculation rather than a configuration mistake.
 */
export function cacheTypeWarning(settings: FitSettings): string | null {
  const quantized = (name: string) => name !== 'f16' && name !== 'bf16' && name !== 'f32';
  if (quantized(settings.cacheTypeV) && settings.flashAttn === 'off') {
    return 'A quantized V cache requires flash attention on most builds. Turn it on, or keep -ctv at f16.';
  }
  return null;
}
