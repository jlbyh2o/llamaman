/**
 * The instance form's value model.
 *
 * DESIGN section 4 calls this "~40 interdependent optional fields", and the interesting word is
 * *optional*: in `model.FlagSet` a null field means "do not pass the flag", which is a different
 * statement from passing its zero value (`parallel: null` lets llama-server choose; `parallel: 0`
 * would be an argument). An HTML form cannot hold `null | number` in an `<input>`, so every
 * optional scalar is a **string** here and the empty string is the null:
 *
 *   ''      → the key is omitted from `flags_json` entirely
 *   '4096'  → the number 4096
 *
 * and every optional boolean is a **tri-state** `'' | 'on' | 'off'` for the same reason —
 * `--mlock` off is a decision, and "not set" is a different one.
 *
 * Conversion happens exactly here, in `flagsFromValues` and `valuesFromInstance`, so the round trip
 * is one function each way and can be tested without a DOM. The schema in `schema.ts` validates the
 * string form; nothing else in the feature parses a number out of a field.
 */

import type { FlagSet, Instance, NGpuLayers } from '../../api/types';

/** `'' | 'on' | 'off'` — the wire's `null | true | false` in a form control. */
export type TriState = '' | 'on' | 'off';

/** The `n_gpu_layers` modes of D51. `auto` renders no `-ngl` at all, which is the whole point. */
export const NGL_MODES = ['auto', 'all', 'none', 'count'] as const;
export type NglMode = (typeof NGL_MODES)[number];

export const AUTH_MODES = ['token', 'none'] as const;
export const RESTART_POLICIES = ['always', 'on-failure', 'never'] as const;
export const FLASH_ATTN_VALUES = ['', 'on', 'off', 'auto'] as const;
export const SPLIT_MODES = ['', 'none', 'layer', 'row'] as const;

/**
 * KV cache types offered in the two selects.
 *
 * `cache_type_k`/`cache_type_v` are deliberately NOT closed by `model.FlagSet.Validate` — "their
 * vocabularies belong to llama.cpp and change with it, so pinning them in Go would reject a build's
 * own new option". The list is therefore a convenience, not a constraint, and the field also accepts
 * anything typed into it.
 */
export const CACHE_TYPES = [
  'f32',
  'f16',
  'bf16',
  'q8_0',
  'q5_1',
  'q5_0',
  'q4_1',
  'q4_0',
  'iq4_nl',
] as const;

export interface InstanceFormValues {
  /* identity ------------------------------------------------------------- */
  name: string;
  display_name: string;
  description: string;

  /* models --------------------------------------------------------------- */
  model_id: string;
  /** '' detaches the projector. Cleared through the PATCH contract's empty string. */
  mmproj_model_id: string;
  draft_model_id: string;

  /* placement ------------------------------------------------------------ */
  public_port: string;
  internal_port: string;
  auth_mode: (typeof AUTH_MODES)[number];
  autostart: boolean;
  restart_policy: (typeof RESTART_POLICIES)[number];
  restart_max: string;
  restart_window_sec: string;

  /* model & context ------------------------------------------------------ */
  ctx_size: string;
  parallel: string;
  alias: string;

  /* performance ---------------------------------------------------------- */
  ngl_mode: NglMode;
  ngl_count: string;
  batch_size: string;
  ubatch_size: string;
  threads: string;
  threads_batch: string;
  flash_attn: (typeof FLASH_ATTN_VALUES)[number];
  cache_type_k: string;
  cache_type_v: string;
  split_mode: (typeof SPLIT_MODES)[number];
  tensor_split: string;
  main_gpu: string;
  /** The UUIDs the user picked. Resolved to `CUDA<n>` at save time and stored as provenance. */
  device_uuids: string[];
  /** `--device`, rendered verbatim. Recomputed from `device_uuids` on save (D66). */
  device_filter: string;
  mlock: TriState;
  no_mmap: TriState;
  cont_batching: TriState;

  /* advanced ------------------------------------------------------------- */
  embedding: TriState;
  pooling: string;
  rerank: TriState;
  jinja: TriState;
  chat_template: string;
  chat_template_file: string;
  rope_scaling: string;
  rope_freq_base: string;
  rope_freq_scale: string;
  yarn_ext_factor: string;
  yarn_attn_factor: string;
  n_keep: string;
  n_predict: string;
  defrag_thold: string;
  cache_reuse: string;
  numa: string;
  cpu_mask: string;
  prio: string;
  slot_save_path: string;
  props_endpoint: TriState;
  slots_endpoint: TriState;
  metrics_endpoint: TriState;
  log_verbosity: string;

  /* speculative decoding -------------------------------------------------- */
  draft_n_max: string;
  draft_n_min: string;
  draft_p_min: string;
  draft_ctx_size: string;
  draft_n_gpu_layers: string;

  /* the escape hatch ------------------------------------------------------ */
  extra_flags: string;
}

/** A brand-new instance: llama.cpp's own defaults everywhere the design does not have an opinion. */
export function emptyFormValues(): InstanceFormValues {
  return {
    name: '',
    display_name: '',
    description: '',

    model_id: '',
    mmproj_model_id: '',
    draft_model_id: '',

    public_port: '',
    internal_port: '',
    auth_mode: 'token',
    autostart: false,
    restart_policy: 'on-failure',
    restart_max: '5',
    restart_window_sec: '600',

    ctx_size: '',
    parallel: '',
    alias: '',

    // `auto` is the honest default: llama.cpp's own --fit knows the free VRAM at load time and we
    // do not (D51). The fit panel shows what we estimate beside it.
    ngl_mode: 'auto',
    ngl_count: '',
    batch_size: '',
    ubatch_size: '',
    threads: '',
    threads_batch: '',
    flash_attn: '',
    cache_type_k: '',
    cache_type_v: '',
    split_mode: '',
    tensor_split: '',
    main_gpu: '',
    device_uuids: [],
    device_filter: '',
    mlock: '',
    no_mmap: '',
    cont_batching: '',

    embedding: '',
    pooling: '',
    rerank: '',
    jinja: '',
    chat_template: '',
    chat_template_file: '',
    rope_scaling: '',
    rope_freq_base: '',
    rope_freq_scale: '',
    yarn_ext_factor: '',
    yarn_attn_factor: '',
    n_keep: '',
    n_predict: '',
    defrag_thold: '',
    cache_reuse: '',
    numa: '',
    cpu_mask: '',
    prio: '',
    slot_save_path: '',
    props_endpoint: 'on',
    slots_endpoint: 'on',
    metrics_endpoint: 'on',
    log_verbosity: '',

    draft_n_max: '',
    draft_n_min: '',
    draft_p_min: '',
    draft_ctx_size: '',
    draft_n_gpu_layers: '',

    extra_flags: '',
  };
}

/* -------------------------------------------------------------------------- */
/* wire → form                                                                 */
/* -------------------------------------------------------------------------- */

const str = (v: string | null | undefined): string => v ?? '';
const num = (v: number | null | undefined): string =>
  v === null || v === undefined ? '' : String(v);
const tri = (v: boolean | null | undefined): TriState =>
  v === null || v === undefined ? '' : v ? 'on' : 'off';

function nglMode(ngl: NGpuLayers | null | undefined): NglMode {
  const mode = ngl?.mode;
  return (NGL_MODES as readonly string[]).includes(mode ?? '') ? (mode as NglMode) : 'auto';
}

/**
 * The flag half of the form, and only that half.
 *
 * Shared by `valuesFromInstance` and `applyFlagsToValues` because a preset *is* exactly this: one
 * `flags_json` document plus `extra_flags` (section 2.8's `flag_presets`), with no identity, no
 * ports and no models of its own.
 */
export type FlagFormValues = Omit<
  InstanceFormValues,
  | 'name'
  | 'display_name'
  | 'description'
  | 'model_id'
  | 'mmproj_model_id'
  | 'draft_model_id'
  | 'public_port'
  | 'internal_port'
  | 'auth_mode'
  | 'autostart'
  | 'restart_policy'
  | 'restart_max'
  | 'restart_window_sec'
>;

export function flagValues(f: FlagSet, extraFlags: string): FlagFormValues {
  const draft = f.draft ?? null;
  return {
    ctx_size: num(f.ctx_size),
    parallel: num(f.parallel),
    alias: str(f.alias),

    ngl_mode: nglMode(f.n_gpu_layers),
    ngl_count: num(f.n_gpu_layers?.count),
    batch_size: num(f.batch_size),
    ubatch_size: num(f.ubatch_size),
    threads: num(f.threads),
    threads_batch: num(f.threads_batch),
    flash_attn: (FLASH_ATTN_VALUES as readonly string[]).includes(f.flash_attn ?? '')
      ? (f.flash_attn as InstanceFormValues['flash_attn'])
      : '',
    cache_type_k: str(f.cache_type_k),
    cache_type_v: str(f.cache_type_v),
    split_mode: (SPLIT_MODES as readonly string[]).includes(f.split_mode ?? '')
      ? (f.split_mode as InstanceFormValues['split_mode'])
      : '',
    tensor_split: (f.tensor_split ?? []).join(', '),
    main_gpu: num(f.main_gpu),
    device_uuids: [...(f.device_uuids ?? [])],
    device_filter: str(f.device_filter),
    mlock: tri(f.mlock),
    no_mmap: tri(f.no_mmap),
    cont_batching: tri(f.cont_batching),

    embedding: tri(f.embedding),
    pooling: str(f.pooling),
    rerank: tri(f.rerank),
    jinja: tri(f.jinja),
    chat_template: str(f.chat_template),
    chat_template_file: str(f.chat_template_file),
    rope_scaling: str(f.rope_scaling),
    rope_freq_base: num(f.rope_freq_base),
    rope_freq_scale: num(f.rope_freq_scale),
    yarn_ext_factor: num(f.yarn_ext_factor),
    yarn_attn_factor: num(f.yarn_attn_factor),
    n_keep: num(f.n_keep),
    n_predict: num(f.n_predict),
    defrag_thold: num(f.defrag_thold),
    cache_reuse: num(f.cache_reuse),
    numa: str(f.numa),
    cpu_mask: str(f.cpu_mask),
    prio: num(f.prio),
    slot_save_path: str(f.slot_save_path),
    props_endpoint: tri(f.props_endpoint),
    slots_endpoint: tri(f.slots_endpoint),
    metrics_endpoint: tri(f.metrics_endpoint),
    log_verbosity: num(f.log_verbosity),

    draft_n_max: num(draft?.n_max),
    draft_n_min: num(draft?.n_min),
    draft_p_min: num(draft?.p_min),
    draft_ctx_size: num(draft?.ctx_size),
    draft_n_gpu_layers: num(draft?.n_gpu_layers),

    extra_flags: extraFlags,
  };
}

/** Seed the form from a saved instance. Every omitted flag comes back as the empty string. */
export function valuesFromInstance(instance: Instance): InstanceFormValues {
  return {
    ...emptyFormValues(),
    ...flagValues(instance.flags, instance.extra_flags),

    name: instance.name,
    display_name: str(instance.display_name),
    description: str(instance.description),

    model_id: str(instance.model_id),
    mmproj_model_id: str(instance.mmproj_model_id),
    draft_model_id: str(instance.draft_model_id),

    public_port: String(instance.public_port),
    internal_port: String(instance.internal_port),
    auth_mode: instance.auth_mode === 'none' ? 'none' : 'token',
    autostart: instance.autostart,
    restart_policy:
      instance.restart_policy === 'always'
        ? 'always'
        : instance.restart_policy === 'never'
          ? 'never'
          : 'on-failure',
    restart_max: String(instance.restart_max),
    restart_window_sec: String(instance.restart_window_sec),
  };
}

/** Seed the flag half of the form from a preset, leaving identity, ports and models alone. */
export function applyFlagsToValues(
  values: InstanceFormValues,
  flags: FlagSet,
  extraFlags: string,
): InstanceFormValues {
  return { ...values, ...flagValues(flags, extraFlags) };
}

/* -------------------------------------------------------------------------- */
/* form → wire                                                                 */
/* -------------------------------------------------------------------------- */

const parseInt10 = (v: string): number | undefined => {
  const t = v.trim();
  if (t === '') return undefined;
  const n = Number(t);
  return Number.isFinite(n) ? Math.trunc(n) : undefined;
};

const parseFloatOrUndef = (v: string): number | undefined => {
  const t = v.trim();
  if (t === '') return undefined;
  const n = Number(t);
  return Number.isFinite(n) ? n : undefined;
};

const parseTri = (v: TriState): boolean | undefined =>
  v === '' ? undefined : v === 'on' ? true : false;

/** `'0.5, 0.5'` and `'0.5 0.5'` both mean the same thing to a human, so both are accepted. */
export function parseTensorSplit(v: string): number[] {
  return v
    .split(/[,\s]+/)
    .map((part) => part.trim())
    .filter((part) => part !== '')
    .map((part) => Number(part))
    .filter((n) => Number.isFinite(n));
}

/** Set a key only when it has a value: an omitted key is `null` on the wire, which is the point. */
function put<T extends object, K extends keyof T>(
  target: T,
  key: K,
  value: T[K] | undefined,
): void {
  if (value !== undefined) target[key] = value;
}

/**
 * The form's flag half as a `model.FlagSet`.
 *
 * `device_filter` is recomputed from `device_uuids` by the caller (`resolveDeviceFilter`) before
 * this runs, because the UUID → `CUDA<n>` resolution happens at save time and needs the live GPU
 * list, which is not part of the form's own state (D66, section 5.7).
 */
export function flagsFromValues(values: InstanceFormValues): FlagSet {
  const flags: FlagSet = {};

  put(flags, 'ctx_size', parseInt10(values.ctx_size));
  if (values.ngl_mode === 'count') {
    const count = parseInt10(values.ngl_count);
    flags.n_gpu_layers = count === undefined ? { mode: 'count' } : { mode: 'count', count };
  } else {
    flags.n_gpu_layers = { mode: values.ngl_mode };
  }

  put(flags, 'batch_size', parseInt10(values.batch_size));
  put(flags, 'ubatch_size', parseInt10(values.ubatch_size));
  put(flags, 'parallel', parseInt10(values.parallel));
  put(flags, 'threads', parseInt10(values.threads));
  put(flags, 'threads_batch', parseInt10(values.threads_batch));

  if (values.flash_attn !== '') flags.flash_attn = values.flash_attn;
  if (values.cache_type_k.trim() !== '') flags.cache_type_k = values.cache_type_k.trim();
  if (values.cache_type_v.trim() !== '') flags.cache_type_v = values.cache_type_v.trim();

  if (values.split_mode !== '') flags.split_mode = values.split_mode;
  const split = parseTensorSplit(values.tensor_split);
  if (split.length > 0) flags.tensor_split = split;
  put(flags, 'main_gpu', parseInt10(values.main_gpu));
  if (values.device_filter.trim() !== '') flags.device_filter = values.device_filter.trim();
  if (values.device_uuids.length > 0) flags.device_uuids = [...values.device_uuids];

  put(flags, 'mlock', parseTri(values.mlock));
  put(flags, 'no_mmap', parseTri(values.no_mmap));
  put(flags, 'cont_batching', parseTri(values.cont_batching));

  put(flags, 'embedding', parseTri(values.embedding));
  if (values.pooling.trim() !== '') flags.pooling = values.pooling.trim();
  put(flags, 'rerank', parseTri(values.rerank));

  if (values.alias.trim() !== '') flags.alias = values.alias.trim();
  if (values.chat_template.trim() !== '') flags.chat_template = values.chat_template.trim();
  if (values.chat_template_file.trim() !== '') {
    flags.chat_template_file = values.chat_template_file.trim();
  }
  put(flags, 'jinja', parseTri(values.jinja));

  if (values.rope_scaling.trim() !== '') flags.rope_scaling = values.rope_scaling.trim();
  put(flags, 'rope_freq_base', parseFloatOrUndef(values.rope_freq_base));
  put(flags, 'rope_freq_scale', parseFloatOrUndef(values.rope_freq_scale));
  put(flags, 'yarn_ext_factor', parseFloatOrUndef(values.yarn_ext_factor));
  put(flags, 'yarn_attn_factor', parseFloatOrUndef(values.yarn_attn_factor));

  put(flags, 'n_keep', parseInt10(values.n_keep));
  put(flags, 'n_predict', parseInt10(values.n_predict));
  put(flags, 'defrag_thold', parseFloatOrUndef(values.defrag_thold));
  put(flags, 'cache_reuse', parseInt10(values.cache_reuse));

  if (values.numa.trim() !== '') flags.numa = values.numa.trim();
  if (values.cpu_mask.trim() !== '') flags.cpu_mask = values.cpu_mask.trim();
  put(flags, 'prio', parseInt10(values.prio));
  if (values.slot_save_path.trim() !== '') flags.slot_save_path = values.slot_save_path.trim();

  put(flags, 'props_endpoint', parseTri(values.props_endpoint));
  put(flags, 'slots_endpoint', parseTri(values.slots_endpoint));
  put(flags, 'metrics_endpoint', parseTri(values.metrics_endpoint));
  put(flags, 'log_verbosity', parseInt10(values.log_verbosity));

  const draft: NonNullable<FlagSet['draft']> = {};
  put(draft, 'n_max', parseInt10(values.draft_n_max));
  put(draft, 'n_min', parseInt10(values.draft_n_min));
  put(draft, 'p_min', parseFloatOrUndef(values.draft_p_min));
  put(draft, 'ctx_size', parseInt10(values.draft_ctx_size));
  put(draft, 'n_gpu_layers', parseInt10(values.draft_n_gpu_layers));
  if (Object.keys(draft).length > 0) flags.draft = draft;

  return flags;
}

/**
 * Resolve the picked GPU UUIDs into the `--device` value.
 *
 * `nvidia-smi index == gpus.gpu_index == llama.cpp's CUDA<n>` holds because the launcher sets no
 * `CUDA_VISIBLE_DEVICES` (D66), so the label is the device's own index. Devices the host no longer
 * reports are dropped rather than guessed at — a `--device CUDA7` for a card that is gone is how a
 * start fails at load time instead of at save time.
 */
export function resolveDeviceFilter(
  uuids: readonly string[],
  devices: readonly { uuid: string; index: number }[],
): string {
  const byUuid = new Map(devices.map((d) => [d.uuid, d.index]));
  return uuids
    .map((uuid) => byUuid.get(uuid))
    .filter((index): index is number => index !== undefined)
    .sort((a, b) => a - b)
    .map((index) => `CUDA${index}`)
    .join(',');
}

/** `POST /api/v1/instances`. Ports are omitted when blank, and the daemon allocates them. */
export function createBody(values: InstanceFormValues): Record<string, unknown> {
  const body: Record<string, unknown> = {
    name: values.name.trim(),
    model_id: values.model_id,
    auth_mode: values.auth_mode,
    autostart: values.autostart,
    restart_policy: values.restart_policy,
    flags: flagsFromValues(values),
    extra_flags: values.extra_flags,
  };
  if (values.display_name.trim() !== '') body['display_name'] = values.display_name.trim();
  if (values.description.trim() !== '') body['description'] = values.description.trim();
  if (values.mmproj_model_id !== '') body['mmproj_model_id'] = values.mmproj_model_id;
  if (values.draft_model_id !== '') body['draft_model_id'] = values.draft_model_id;

  const publicPort = parseInt10(values.public_port);
  if (publicPort !== undefined) body['public_port'] = publicPort;
  const internalPort = parseInt10(values.internal_port);
  if (internalPort !== undefined) body['internal_port'] = internalPort;

  const restartMax = parseInt10(values.restart_max);
  if (restartMax !== undefined) body['restart_max'] = restartMax;
  const restartWindow = parseInt10(values.restart_window_sec);
  if (restartWindow !== undefined) body['restart_window_sec'] = restartWindow;

  return body;
}

/**
 * `PATCH /api/v1/instances/{id}`.
 *
 * The empty string is how the partial update says "clear this": `"draft_model_id": ""` detaches the
 * draft model (section 3.10's PATCH contract). `autostart` and `desired_state` are deliberately
 * absent — autostart is `PUT /instances/{id}/autostart` and desired state belongs to start/stop, so
 * an edit to an unrelated field can never change what happens at the next boot.
 */
export function patchBody(values: InstanceFormValues, generation: number): Record<string, unknown> {
  const body: Record<string, unknown> = {
    generation,
    name: values.name.trim(),
    display_name: values.display_name.trim(),
    description: values.description.trim(),
    model_id: values.model_id,
    mmproj_model_id: values.mmproj_model_id,
    draft_model_id: values.draft_model_id,
    auth_mode: values.auth_mode,
    restart_policy: values.restart_policy,
    flags: flagsFromValues(values),
    extra_flags: values.extra_flags,
  };
  const publicPort = parseInt10(values.public_port);
  if (publicPort !== undefined) body['public_port'] = publicPort;
  const internalPort = parseInt10(values.internal_port);
  if (internalPort !== undefined) body['internal_port'] = internalPort;
  const restartMax = parseInt10(values.restart_max);
  if (restartMax !== undefined) body['restart_max'] = restartMax;
  const restartWindow = parseInt10(values.restart_window_sec);
  if (restartWindow !== undefined) body['restart_window_sec'] = restartWindow;
  return body;
}
