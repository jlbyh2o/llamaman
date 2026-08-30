/**
 * The instance form's validation, mirroring the server's.
 *
 * Every rule here exists on the Go side too — `internal/instances/validate.go`,
 * `model.FlagSet.Validate`, `internal/instances/ports.go`'s table and
 * `internal/instances/extraflags.go` — and DESIGN section 4 says why the duplication is deliberate:
 * "zod resolvers give per-field validation identical to the server's". The daemon stays the
 * authority; this only lets the form say the same "no" without a round trip, in the same words, at
 * the field that caused it.
 *
 * What is NOT duplicated, on purpose:
 *
 *  - `cache_type_k`/`cache_type_v`, `pooling`, `numa` and `rope_scaling` — "their vocabularies
 *    belong to llama.cpp and change with it, so pinning them in Go would reject a build's own new
 *    option" (`FlagSet.Validate`). The selects offer the common values and the fields accept
 *    anything.
 *  - The draft-vocabulary check of D34 — it needs both models' parsed GGUF metadata, which lives on
 *    the server. The form shows the model's own parse state and the API's three-valued answer.
 *  - The live bind probe behind `port_unavailable`. Only the daemon can bind a port; the rules that
 *    do not need a syscall (range, the management port, the internal pool, another instance's
 *    claim) are checked here and the probe still runs at save time.
 */

import { z } from 'zod';
import { parseExtraFlags } from './extraFlags';
import { FLASH_ATTN_VALUES, NGL_MODES, SPLIT_MODES } from './values';
import type { InstanceFormValues } from './values';

/** D11's grammar: this string becomes the systemd unit id `llamaman-instance@<name>.service`. */
export const NAME_PATTERN = /^[a-z0-9][a-z0-9-]{0,31}$/;

export const NAME_MESSAGE =
  'an instance name must be 1-32 characters of lowercase letters, digits and hyphens, ' +
  'starting with a letter or digit — it becomes a systemd unit name';

export const PORT_MIN = 1024;
export const PORT_MAX = 65535;

/** One live instance's claim on both of its ports (section 2.8's exclusion set). */
export interface PortClaim {
  instance_id: string;
  name: string;
  public_port: number;
  internal_port: number;
}

/**
 * What the port rules are evaluated against.
 *
 * The pool bounds and the management port are settings (`instances.internal_port_min`/`_max`,
 * `ui.port_desired`), so a screen passes whatever it knows and falls back to the registry defaults.
 * A wrong guess here can only produce a *hint*: the daemon re-runs the whole table at save time.
 */
export interface FormContext {
  claims: readonly PortClaim[];
  internalPortMin: number;
  internalPortMax: number;
  /** `ui.port_desired` and the port the management listener actually landed on. */
  managementPorts: readonly number[];
  /** The instance being edited, whose own claim must not conflict with itself. */
  excludeInstanceId?: string;
}

/** The settings-registry defaults, used until a screen knows better. */
export const DEFAULT_FORM_CONTEXT: FormContext = {
  claims: [],
  internalPortMin: 21000,
  internalPortMax: 21999,
  managementPorts: [5526],
};

/* -------------------------------------------------------------------------- */
/* number fields, which are strings in the form                                */
/* -------------------------------------------------------------------------- */

type Issue = { path: keyof InstanceFormValues; message: string };

const INT_MESSAGE = 'must be a whole number';
const NUM_MESSAGE = 'must be a number';

interface NumberRule {
  field: keyof InstanceFormValues;
  /** Reject anything with a fractional part. */
  integer?: boolean;
  min?: number;
  max?: number;
  /** The message when `min` is violated; defaults to a generated one. */
  minMessage?: string;
}

function checkNumber(value: string, rule: NumberRule): Issue | null {
  const raw = value.trim();
  if (raw === '') return null;
  const n = Number(raw);
  if (!Number.isFinite(n)) {
    return { path: rule.field, message: rule.integer ? INT_MESSAGE : NUM_MESSAGE };
  }
  if (rule.integer && !Number.isInteger(n)) return { path: rule.field, message: INT_MESSAGE };
  if (rule.min !== undefined && n < rule.min) {
    return {
      path: rule.field,
      message: rule.minMessage ?? `must be ${rule.min} or greater`,
    };
  }
  if (rule.max !== undefined && n > rule.max) {
    return { path: rule.field, message: `must be ${rule.max} or less` };
  }
  return null;
}

/**
 * `ctx_size`, `batch_size`, `ubatch_size` and `parallel` are the four `FlagSet.Validate` checks
 * with a message quoted from it: null means "do not pass the flag", and zero would be an argument.
 */
const POSITIVE_INT_FIELDS: NumberRule[] = [
  { field: 'ctx_size', integer: true, min: 1, minMessage: 'ctx_size must be greater than zero' },
  {
    field: 'batch_size',
    integer: true,
    min: 1,
    minMessage: 'batch_size must be greater than zero',
  },
  {
    field: 'ubatch_size',
    integer: true,
    min: 1,
    minMessage: 'ubatch_size must be greater than zero',
  },
  { field: 'parallel', integer: true, min: 1, minMessage: 'parallel must be greater than zero' },
];

const OTHER_NUMBER_FIELDS: NumberRule[] = [
  { field: 'threads', integer: true, min: 1, minMessage: 'threads must be greater than zero' },
  {
    field: 'threads_batch',
    integer: true,
    min: 1,
    minMessage: 'threads_batch must be greater than zero',
  },
  { field: 'main_gpu', integer: true, min: 0, minMessage: 'main_gpu may not be negative' },
  { field: 'rope_freq_base', min: 0 },
  { field: 'rope_freq_scale', min: 0 },
  { field: 'yarn_ext_factor' },
  { field: 'yarn_attn_factor' },
  { field: 'n_keep', integer: true, min: -1 },
  { field: 'n_predict', integer: true, min: -2 },
  { field: 'defrag_thold' },
  { field: 'cache_reuse', integer: true, min: 0 },
  { field: 'prio', integer: true, min: 0, max: 3 },
  { field: 'log_verbosity', integer: true },
  { field: 'restart_max', integer: true, min: 0 },
  { field: 'restart_window_sec', integer: true, min: 1 },
  { field: 'draft_n_max', integer: true, min: 0 },
  { field: 'draft_n_min', integer: true, min: 0 },
  { field: 'draft_p_min', min: 0, max: 1 },
  { field: 'draft_ctx_size', integer: true, min: 1 },
  { field: 'draft_n_gpu_layers', integer: true, min: 0 },
];

/* -------------------------------------------------------------------------- */
/* ports                                                                       */
/* -------------------------------------------------------------------------- */

export type PortKind = 'public' | 'internal';

export interface PortVerdict {
  ok: boolean;
  /** `port_unavailable`'s `reason`, spelled as `model.PortReason` does. */
  reason?:
    | 'bind_failed'
    | 'reserved_management'
    | 'reserved_internal_pool'
    | 'outside_internal_pool'
    | 'in_use_by_instance';
  message?: string;
}

/**
 * Section 2.8's port table, minus the bind probe.
 *
 * Exported because the field's live "availability hint" and the resolver's error are the same
 * judgment, and showing one while validating with the other is how a form starts lying.
 */
export function checkPort(kind: PortKind, port: number, ctx: FormContext): PortVerdict {
  if (!Number.isInteger(port)) return { ok: false, reason: 'bind_failed', message: INT_MESSAGE };
  if (port < PORT_MIN || port > PORT_MAX) {
    return {
      ok: false,
      reason: 'bind_failed',
      message: `port ${port} is outside the ${PORT_MIN}-${PORT_MAX} range an unprivileged daemon can bind`,
    };
  }

  const inPool = port >= ctx.internalPortMin && port <= ctx.internalPortMax;

  if (kind === 'public') {
    if (ctx.managementPorts.includes(port)) {
      return {
        ok: false,
        reason: 'reserved_management',
        message: `port ${port} is the management UI's`,
      };
    }
    if (inPool) {
      return {
        ok: false,
        reason: 'reserved_internal_pool',
        message:
          `port ${port} is inside the internal pool ` +
          `[${ctx.internalPortMin}, ${ctx.internalPortMax}] instances draw from`,
      };
    }
  } else if (!inPool) {
    return {
      ok: false,
      reason: 'outside_internal_pool',
      message: `an internal port must be inside the pool [${ctx.internalPortMin}, ${ctx.internalPortMax}]`,
    };
  }

  for (const claim of ctx.claims) {
    if (claim.instance_id === ctx.excludeInstanceId) continue;
    if (claim.public_port === port || claim.internal_port === port) {
      return {
        ok: false,
        reason: 'in_use_by_instance',
        message: `port ${port} is already held by the instance "${claim.name}"`,
      };
    }
  }

  return { ok: true };
}

/* -------------------------------------------------------------------------- */
/* the schema                                                                  */
/* -------------------------------------------------------------------------- */

const triState = z.enum(['', 'on', 'off']);
const text = z.string();

/**
 * The whole form, as a zod schema over the string-shaped value model.
 *
 * It is a factory rather than a constant because three of section 2.8's port rules are relative to
 * *this host* — the management port, the internal pool and the other instances' claims — and a
 * schema that could not see them would have to drop them, which is exactly the check a user most
 * wants before a save.
 */
export function createInstanceFormSchema(ctx: FormContext = DEFAULT_FORM_CONTEXT) {
  return z
    .object({
      name: z.string().min(1, 'an instance needs a name').regex(NAME_PATTERN, NAME_MESSAGE),
      display_name: text,
      description: text,

      model_id: z.string().min(1, 'choose the model this instance serves'),
      mmproj_model_id: text,
      draft_model_id: text,

      public_port: text,
      internal_port: text,
      auth_mode: z.enum(['token', 'none']),
      autostart: z.boolean(),
      restart_policy: z.enum(['always', 'on-failure', 'never']),
      restart_max: text,
      restart_window_sec: text,

      ctx_size: text,
      parallel: text,
      alias: text,

      ngl_mode: z.enum(NGL_MODES),
      ngl_count: text,
      batch_size: text,
      ubatch_size: text,
      threads: text,
      threads_batch: text,
      flash_attn: z.enum(FLASH_ATTN_VALUES),
      cache_type_k: text,
      cache_type_v: text,
      split_mode: z.enum(SPLIT_MODES),
      tensor_split: text,
      main_gpu: text,
      device_uuids: z.array(z.string()),
      device_filter: text,
      mlock: triState,
      no_mmap: triState,
      cont_batching: triState,

      embedding: triState,
      pooling: text,
      rerank: triState,
      jinja: triState,
      chat_template: text,
      chat_template_file: text,
      rope_scaling: text,
      rope_freq_base: text,
      rope_freq_scale: text,
      yarn_ext_factor: text,
      yarn_attn_factor: text,
      n_keep: text,
      n_predict: text,
      defrag_thold: text,
      cache_reuse: text,
      numa: text,
      cpu_mask: text,
      prio: text,
      slot_save_path: text,
      props_endpoint: triState,
      slots_endpoint: triState,
      metrics_endpoint: triState,
      log_verbosity: text,

      draft_n_max: text,
      draft_n_min: text,
      draft_p_min: text,
      draft_ctx_size: text,
      draft_n_gpu_layers: text,

      extra_flags: text,
    })
    .superRefine((values, refine) => {
      const add = (issue: Issue) =>
        refine.addIssue({ code: 'custom', message: issue.message, path: [issue.path] });

      /* numbers ---------------------------------------------------------- */
      for (const rule of [...POSITIVE_INT_FIELDS, ...OTHER_NUMBER_FIELDS]) {
        const issue = checkNumber(values[rule.field] as string, rule);
        if (issue) add(issue);
      }

      /* -ngl, the tri-state plus a count (D51) --------------------------- */
      if (values.ngl_mode === 'count') {
        const raw = values.ngl_count.trim();
        if (raw === '') {
          add({
            path: 'ngl_count',
            message: 'n_gpu_layers.mode is "count" but no count was given',
          });
        } else {
          const issue = checkNumber(raw, {
            field: 'ngl_count',
            integer: true,
            min: 0,
            minMessage: 'n_gpu_layers.count may not be negative',
          });
          if (issue) add(issue);
        }
      }

      /* tensor_split, and D51's one cross-field guard --------------------- */
      const splitRaw = values.tensor_split.trim();
      if (splitRaw !== '') {
        const parts = splitRaw.split(/[,\s]+/).filter((p) => p !== '');
        parts.forEach((part, index) => {
          const n = Number(part);
          if (!Number.isFinite(n)) {
            add({ path: 'tensor_split', message: `tensor_split[${index}] is not a number` });
          } else if (n < 0) {
            add({ path: 'tensor_split', message: `tensor_split[${index}] is negative` });
          }
        });
        if (values.ngl_mode === 'auto') {
          add({
            path: 'tensor_split',
            message:
              'n_gpu_layers `auto` cannot be combined with an explicit tensor_split: llama.cpp ' +
              'disables --fit when either is pinned, so `auto` would decide nothing',
          });
        }
      }

      /* draft tuning ------------------------------------------------------ */
      const nMax = Number(values.draft_n_max.trim());
      const nMin = Number(values.draft_n_min.trim());
      if (
        values.draft_n_max.trim() !== '' &&
        values.draft_n_min.trim() !== '' &&
        Number.isFinite(nMax) &&
        Number.isFinite(nMin) &&
        nMin > nMax
      ) {
        add({
          path: 'draft_n_min',
          message: 'the minimum draft length cannot exceed the maximum',
        });
      }

      /* ports ------------------------------------------------------------- */
      const ports: { field: 'public_port' | 'internal_port'; kind: PortKind }[] = [
        { field: 'public_port', kind: 'public' },
        { field: 'internal_port', kind: 'internal' },
      ];
      // No "the two ports must differ" rule is needed, and adding one would be theater: the pool
      // partitions them. A public port inside [min, max] is already refused as reserved, and an
      // internal port outside it is already refused as out of pool, so the two sets are disjoint by
      // construction — which is exactly what section 2.8's table buys.
      for (const { field, kind } of ports) {
        const raw = values[field].trim();
        if (raw === '') continue; // omitted: the daemon allocates one (section 3.10)
        const n = Number(raw);
        if (!Number.isInteger(n)) {
          add({ path: field, message: INT_MESSAGE });
          continue;
        }
        const verdict = checkPort(kind, n, ctx);
        if (!verdict.ok && verdict.message) add({ path: field, message: verdict.message });
      }

      /* the escape hatch --------------------------------------------------- */
      if (values.extra_flags.trim() !== '') {
        const result = parseExtraFlags(values.extra_flags);
        if (result.error) add({ path: 'extra_flags', message: result.error });
      }
    });
}

export type InstanceFormSchema = ReturnType<typeof createInstanceFormSchema>;
