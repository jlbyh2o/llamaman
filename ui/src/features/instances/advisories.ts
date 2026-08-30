/**
 * Things worth saying that are not refusals.
 *
 * The form has two channels and they mean different things. An **error** is a configuration the
 * daemon would reject, mirrored from the server's own rules (`schema.ts`). An **advisory** is a
 * configuration the daemon will happily save and that will then do something the user probably did
 * not mean — `-ub` larger than `-b`, a context that does not divide across the slots, a draft
 * tuning with no draft model attached, the metrics endpoint turned off so `requests_served` reads
 * as unavailable rather than zero (section 2.9).
 *
 * Keeping them apart is what lets the escape hatch stay an escape hatch: `extra_flags` that
 * duplicates a modeled field is a warning, never a block, because the renderer appends it last and
 * the user may well mean it.
 */

import { duplicatedFlags } from './extraFlags';
import { parseTensorSplit } from './values';
import type { InstanceFormValues } from './values';

export interface Advisory {
  /** Stable identifier, so a test names a rule rather than a sentence. */
  code: string;
  message: string;
  /** The field to point at, when there is one. */
  field?: keyof InstanceFormValues;
  tone: 'info' | 'warn';
}

export interface AdvisoryContext {
  /**
   * `llamacpp_versions.supports_fit` for the active build. `false` is section 5.7's first rule:
   * `-ngl auto` renders as `-ngl 999` on a build that predates `--fit`, so "auto" is not what it
   * says. `undefined` means no build is active, or the daemon did not say.
   */
  supportsFit?: boolean;
  /** How many devices `--device` currently selects, for the `-ts`/`-mg` index rules. */
  deviceCount: number;
  /** Whether a draft model is attached, which is what makes the draft tuning mean anything. */
  hasDraftModel: boolean;
  /** Whether the chosen model reports vision tensors, which is what an mmproj pairs with. */
  modelHasVision?: boolean;
  /** Flags the active build's own `--help` does not advertise (section 5.7's churn guard). */
  unknownFlags?: readonly string[];
}

const int = (v: string): number | null => {
  const t = v.trim();
  if (t === '') return null;
  const n = Number(t);
  return Number.isFinite(n) ? n : null;
};

/** Every advisory that applies to these values, in the order the form shows them. */
export function formAdvisories(
  values: InstanceFormValues,
  ctx: AdvisoryContext,
): readonly Advisory[] {
  const out: Advisory[] = [];

  if (values.ngl_mode === 'auto' && ctx.supportsFit === false) {
    out.push({
      code: 'ngl_auto_without_fit',
      field: 'ngl_mode',
      tone: 'warn',
      message: 'This build predates --fit, so `auto` behaves as `all` (-ngl 999).',
    });
  }

  const batch = int(values.batch_size);
  const ubatch = int(values.ubatch_size);
  if (batch !== null && ubatch !== null && ubatch > batch) {
    out.push({
      code: 'ubatch_over_batch',
      field: 'ubatch_size',
      tone: 'warn',
      message: `The physical batch (-ub ${ubatch}) is larger than the logical one (-b ${batch}); llama-server refuses that at startup.`,
    });
  }

  const ctxSize = int(values.ctx_size);
  const parallel = int(values.parallel);
  if (ctxSize !== null && parallel !== null && parallel > 1) {
    const perSlot = Math.floor(ctxSize / parallel);
    const remainder = ctxSize - perSlot * parallel;
    out.push({
      code: 'ctx_per_slot',
      field: 'ctx_size',
      tone: remainder === 0 ? 'info' : 'warn',
      message:
        `-c is the TOTAL context, shared across the ${parallel} slots: ${perSlot} tokens each` +
        (remainder === 0 ? '.' : `, with ${remainder} left over.`),
    });
  }

  const split = parseTensorSplit(values.tensor_split);
  if (split.length > 0 && ctx.deviceCount > 0 && split.length !== ctx.deviceCount) {
    out.push({
      code: 'tensor_split_arity',
      field: 'tensor_split',
      tone: 'warn',
      message: `${split.length} split weights for ${ctx.deviceCount} selected device${ctx.deviceCount === 1 ? '' : 's'} — the indices are into the --device list, not nvidia-smi's.`,
    });
  }

  const mainGpu = int(values.main_gpu);
  if (mainGpu !== null && ctx.deviceCount > 0 && mainGpu >= ctx.deviceCount) {
    out.push({
      code: 'main_gpu_index',
      field: 'main_gpu',
      tone: 'warn',
      message: `--main-gpu is an index into the --device list, which currently has ${ctx.deviceCount} entr${ctx.deviceCount === 1 ? 'y' : 'ies'}.`,
    });
  }

  if (values.embedding === 'on' && values.rerank === 'on') {
    out.push({
      code: 'embedding_and_rerank',
      tone: 'warn',
      message: 'Embedding and reranking are different server modes; enable one.',
    });
  }

  const draftTuned = [
    values.draft_n_max,
    values.draft_n_min,
    values.draft_p_min,
    values.draft_ctx_size,
    values.draft_n_gpu_layers,
  ].some((v) => v.trim() !== '');
  if (draftTuned && !ctx.hasDraftModel) {
    out.push({
      code: 'draft_without_model',
      field: 'draft_model_id',
      tone: 'warn',
      message:
        'Speculative decoding is tuned but no draft model is attached, so none of it renders.',
    });
  }

  if (values.mmproj_model_id !== '' && ctx.modelHasVision === false) {
    out.push({
      code: 'mmproj_without_vision',
      field: 'mmproj_model_id',
      tone: 'warn',
      message: 'This model does not report vision tensors; a projector will not be used.',
    });
  }

  if (values.metrics_endpoint === 'off') {
    out.push({
      code: 'metrics_disabled',
      field: 'metrics_endpoint',
      tone: 'info',
      message:
        'With --metrics off, requests served reads as "metrics disabled" rather than as a count.',
    });
  }

  if (values.restart_policy === 'never') {
    out.push({
      code: 'restart_never',
      field: 'restart_policy',
      tone: 'info',
      message: 'A crash will leave this instance stopped, flagged "inhibited: policy never".',
    });
  }

  if (values.auth_mode === 'none') {
    out.push({
      code: 'auth_none',
      field: 'auth_mode',
      tone: 'warn',
      message:
        'Anyone who can reach the public port can use this instance. Traffic is still counted.',
    });
  }

  const duplicated = duplicatedFlags(values.extra_flags);
  if (duplicated.length > 0) {
    out.push({
      code: 'extra_flags_duplicate',
      field: 'extra_flags',
      tone: 'warn',
      message: `${duplicated.join(', ')} ${duplicated.length === 1 ? 'is' : 'are'} already set by a field above. Extra flags are appended last, so this value wins.`,
    });
  }

  if (ctx.unknownFlags && ctx.unknownFlags.length > 0) {
    out.push({
      code: 'unknown_flags',
      tone: 'warn',
      message: `The active build does not advertise ${ctx.unknownFlags.join(', ')}. llama.cpp ships nightlies daily, so this is a warning, not a refusal.`,
    });
  }

  return out;
}
