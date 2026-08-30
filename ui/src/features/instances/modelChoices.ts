/**
 * Which local models may be picked for which slot.
 *
 * Three pickers, three different questions, and one of them is D34's:
 *
 *  - **Primary** — anything that is not a projector and has not been deleted. A model that is still
 *    downloading is deliberately offerable: "the design deliberately supports creating an instance
 *    against a model that is still `planned` or `downloading` — that is the whole 'queue the
 *    download, configure the instance while it runs' flow" (section 3.10a).
 *  - **Projector** — `kind === 'mmproj'`, with the ones from the primary model's own repository
 *    first, because that is the pairing that is almost always meant.
 *  - **Draft** — the speculative-decoding model, and the only picker with a *correctness* filter.
 *    Section 3.10a's table is three-valued and this mirrors it exactly: both sides parsed and equal
 *    is `ok`; both parsed and different is `mismatch`, which the server refuses with
 *    `422 draft_vocab_mismatch` and which is therefore disabled here rather than offered and then
 *    rejected; either side unparsed is `deferred` — offerable, saved, and re-checked when the
 *    metadata lands.
 *
 * Pure functions over rows, so the rules can be tested without a picker.
 */

import { formatBytes } from '../../format';
import type { Model } from '../../api/types';

export type DraftCompatibility = 'ok' | 'deferred' | 'mismatch';

export interface ModelChoice {
  value: string;
  label: string;
  description?: string;
  disabled?: boolean;
  compatibility?: DraftCompatibility;
}

const NONE: ModelChoice = { value: '', label: 'None' };

/** `deleted` rows are history, not catalog (section 7.2 never issues a SQL DELETE). */
const live = (model: Model) => model.state !== 'deleted';

function describe(model: Model): string {
  const parts = [model.quant_label ?? model.file_type ?? '', formatBytes(model.total_bytes)];
  if (model.state !== 'ready') parts.push(model.state);
  return parts.filter(Boolean).join(' · ');
}

function label(model: Model): string {
  return `${model.repo_id} · ${model.primary_file}`;
}

export function primaryModelChoices(models: readonly Model[]): ModelChoice[] {
  return models
    .filter((model) => live(model) && model.kind !== 'mmproj')
    .map((model) => ({ value: model.id, label: label(model), description: describe(model) }));
}

export function mmprojChoices(models: readonly Model[], primary: Model | undefined): ModelChoice[] {
  const projectors = models.filter((model) => live(model) && model.kind === 'mmproj');
  const sameRepo = projectors.filter((model) => model.repo_id === primary?.repo_id);
  const others = projectors.filter((model) => model.repo_id !== primary?.repo_id);
  return [
    NONE,
    ...sameRepo.map((model) => ({
      value: model.id,
      label: label(model),
      description: `From this model’s repository · ${describe(model)}`,
    })),
    ...others.map((model) => ({
      value: model.id,
      label: label(model),
      description: describe(model),
    })),
  ];
}

/**
 * D34's three-valued check, per candidate.
 *
 * `tokenizer_model` and `n_vocab` are populated only by a GGUF parse (`models.gguf_parsed_at`), so
 * a null on either side is "not known yet", never "different".
 */
export function draftCompatibility(primary: Model | undefined, draft: Model): DraftCompatibility {
  if (!primary) return 'deferred';
  const bothParsed = Boolean(primary.gguf_parsed_at) && Boolean(draft.gguf_parsed_at);
  if (!bothParsed) return 'deferred';
  const sameTokenizer = (primary.tokenizer_model ?? null) === (draft.tokenizer_model ?? null);
  const sameVocab = (primary.n_vocab ?? null) === (draft.n_vocab ?? null);
  return sameTokenizer && sameVocab ? 'ok' : 'mismatch';
}

export function draftChoices(models: readonly Model[], primary: Model | undefined): ModelChoice[] {
  const candidates = models.filter(
    (model) => live(model) && model.kind !== 'mmproj' && model.id !== primary?.id,
  );
  const choices = candidates.map((model): ModelChoice => {
    const compatibility = draftCompatibility(primary, model);
    const size = describe(model);
    if (compatibility === 'mismatch') {
      return {
        value: model.id,
        label: label(model),
        description: `Different vocabulary (${model.tokenizer_model ?? 'unknown tokenizer'}, ${
          model.n_vocab ?? '?'
        } tokens) — speculative decoding would produce garbage`,
        disabled: true,
        compatibility,
      };
    }
    if (compatibility === 'deferred') {
      return {
        value: model.id,
        label: label(model),
        description: `${size} · vocabulary check deferred until the GGUF header is read`,
        compatibility,
      };
    }
    const bigger =
      primary !== undefined && model.total_bytes >= primary.total_bytes
        ? ' · larger than the primary model, which usually costs more than it saves'
        : '';
    return {
      value: model.id,
      label: label(model),
      description: `${size} · same tokenizer${bigger}`,
      compatibility,
    };
  });

  const order: Record<DraftCompatibility, number> = { ok: 0, deferred: 1, mismatch: 2 };
  choices.sort(
    (a, b) => order[a.compatibility ?? 'deferred'] - order[b.compatibility ?? 'deferred'],
  );
  return [NONE, ...choices];
}

/** The row a picker is currently pointing at, or undefined. */
export function findModel(models: readonly Model[], id: string): Model | undefined {
  return id === '' ? undefined : models.find((model) => model.id === id);
}
