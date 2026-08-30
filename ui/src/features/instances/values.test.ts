/**
 * The value model, the escape hatch's parser, the pickers' rules, and the daemon's refusals landing
 * on fields.
 *
 * All of it is pure, and all of it is the half of the form that decides what actually reaches
 * `flags_json` — where a mistaken `undefined` and a mistaken `0` are two different bugs, one silent.
 */

import { describe, expect, it } from 'vitest';
import { duplicatedFlags, flagName, parseExtraFlags, splitWords } from './extraFlags';
import {
  draftChoices,
  draftCompatibility,
  mmprojChoices,
  primaryModelChoices,
} from './modelChoices';
import { portClaims, portHint, suggestPort } from './ports';
import { DEFAULT_FORM_CONTEXT } from './schema';
import { refusalReport } from './serverErrors';
import {
  applyFlagsToValues,
  createBody,
  emptyFormValues,
  flagsFromValues,
  parseTensorSplit,
  patchBody,
  resolveDeviceFilter,
  valuesFromInstance,
} from './values';
import { makeInstance, makeModel, validFormValues } from './fixtures';
import { ApiError } from '../../api/errors';

describe('null is not zero — the form → FlagSet conversion', () => {
  it('omits every blank field rather than passing its zero value', () => {
    const flags = flagsFromValues(emptyFormValues());
    // The only keys a brand-new instance carries: the offload decision, and the three endpoints the
    // supervisor and the gateway read (section 2.8's `flags_json` example turns all three on).
    expect(flags).toEqual({
      n_gpu_layers: { mode: 'auto' },
      props_endpoint: true,
      slots_endpoint: true,
      metrics_endpoint: true,
    });
    expect('ctx_size' in flags).toBe(false);
    expect('parallel' in flags).toBe(false);
    expect('mlock' in flags).toBe(false);
  });

  it('sends `false` when a tri-state says off, because off is a decision', () => {
    const flags = flagsFromValues(validFormValues({ mlock: 'off', cont_batching: 'off' }));
    expect(flags.mlock).toBe(false);
    expect(flags.cont_batching).toBe(false);
  });

  it('sends nothing when a tri-state is unset', () => {
    const flags = flagsFromValues(validFormValues({ mlock: '' }));
    expect('mlock' in flags).toBe(false);
  });

  it('renders the four -ngl modes as the object D51 defines', () => {
    expect(flagsFromValues(validFormValues({ ngl_mode: 'auto' })).n_gpu_layers).toEqual({
      mode: 'auto',
    });
    expect(flagsFromValues(validFormValues({ ngl_mode: 'all' })).n_gpu_layers).toEqual({
      mode: 'all',
    });
    expect(flagsFromValues(validFormValues({ ngl_mode: 'none' })).n_gpu_layers).toEqual({
      mode: 'none',
    });
    expect(
      flagsFromValues(validFormValues({ ngl_mode: 'count', ngl_count: '37' })).n_gpu_layers,
    ).toEqual({ mode: 'count', count: 37 });
  });

  it('keeps the draft object out of the flags when nothing is tuned', () => {
    expect('draft' in flagsFromValues(validFormValues())).toBe(false);
    expect(flagsFromValues(validFormValues({ draft_p_min: '0.75' })).draft).toEqual({
      p_min: 0.75,
    });
  });

  it('round-trips a saved instance through the form without inventing flags', () => {
    const instance = makeInstance();
    const flags = flagsFromValues(valuesFromInstance(instance));
    expect(flags).toEqual(instance.flags);
  });

  it('applies a preset over the flags and leaves the identity alone', () => {
    const values = validFormValues({ name: 'qwen3-8b', public_port: '8081' });
    const next = applyFlagsToValues(values, { ctx_size: 32_768, flash_attn: 'off' }, '--lora x');
    expect(next.name).toBe('qwen3-8b');
    expect(next.public_port).toBe('8081');
    expect(next.ctx_size).toBe('32768');
    expect(next.flash_attn).toBe('off');
    expect(next.extra_flags).toBe('--lora x');
  });

  it('takes either separator for a tensor split', () => {
    expect(parseTensorSplit('0.5, 0.5')).toEqual([0.5, 0.5]);
    expect(parseTensorSplit('0.6 0.4')).toEqual([0.6, 0.4]);
    expect(parseTensorSplit('')).toEqual([]);
  });

  it('resolves picked UUIDs into --device labels in index order (D66)', () => {
    const devices = [
      { uuid: 'GPU-b', index: 1 },
      { uuid: 'GPU-a', index: 0 },
    ];
    expect(resolveDeviceFilter(['GPU-b', 'GPU-a'], devices)).toBe('CUDA0,CUDA1');
  });

  it('drops a UUID the host no longer reports rather than guessing an index', () => {
    expect(resolveDeviceFilter(['GPU-gone'], [{ uuid: 'GPU-a', index: 0 }])).toBe('');
  });
});

describe('request bodies', () => {
  it('omits a blank port on create, so the daemon allocates one', () => {
    const body = createBody(validFormValues({ public_port: '', internal_port: '' }));
    expect('public_port' in body).toBe(false);
    expect('internal_port' in body).toBe(false);
  });

  it('sends the empty string on patch, which is how the API clears a nullable id', () => {
    const body = patchBody(validFormValues({ draft_model_id: '' }), 7);
    expect(body['draft_model_id']).toBe('');
    expect(body['generation']).toBe(7);
  });

  it('never sends autostart or desired_state in a config patch', () => {
    const body = patchBody(validFormValues({ autostart: true }), 1);
    expect('autostart' in body).toBe(false);
    expect('desired_state' in body).toBe(false);
  });
});

describe('extra_flags — POSIX words, not a shell', () => {
  it('splits on unquoted whitespace', () => {
    expect(splitWords('--a b  --c')).toEqual({ ok: true, words: ['--a', 'b', '--c'] });
  });

  it('takes single quotes literally', () => {
    expect(splitWords("--x 'a b'")).toEqual({ ok: true, words: ['--x', 'a b'] });
  });

  it('honors the two escapes double quotes allow, and nothing else', () => {
    expect(splitWords('"a\\"b"')).toEqual({ ok: true, words: ['a"b'] });
    expect(splitWords('"a\\nb"')).toEqual({ ok: true, words: ['a\\nb'] });
  });

  it('treats shell metacharacters as ordinary text — no expansion happens anywhere', () => {
    const result = splitWords('$(reboot) `id` *.gguf ~/x; rm -rf /');
    expect(result).toEqual({
      ok: true,
      words: ['$(reboot)', '`id`', '*.gguf', '~/x;', 'rm', '-rf', '/'],
    });
  });

  it('refuses an unterminated quote rather than closing it silently', () => {
    expect(splitWords('"abc')).toEqual({ ok: false, error: 'unterminated double quote' });
    expect(splitWords("'abc")).toEqual({ ok: false, error: 'unterminated single quote' });
  });

  it('reads flag names the way the renderer does', () => {
    expect(flagName('--port')).toBe('--port');
    expect(flagName('--port=8080')).toBe('--port');
    expect(flagName('8080')).toBeNull();
    expect(flagName('-0.5')).toBeNull();
    expect(flagName('-')).toBeNull();
  });

  it('names the forbidden override in the refusal', () => {
    const result = parseExtraFlags('--api-key secret');
    expect(result.forbidden).toBe('--api-key');
    expect(result.error).toContain('--api-key');
  });

  it('lists flags that duplicate a modeled field without refusing them', () => {
    expect(duplicatedFlags('-c 4096 --jinja --lora x')).toEqual(['-c', '--jinja']);
  });
});

describe('the model pickers', () => {
  const primary = makeModel();
  const sameVocab = makeModel({ id: 'mdl_draft', primary_file: 'Qwen3-0.6B-Q4_K_M.gguf' });
  const otherVocab = makeModel({
    id: 'mdl_gemma',
    tokenizer_model: 'llama',
    n_vocab: 262_144,
  });
  const unparsed = makeModel({
    id: 'mdl_downloading',
    state: 'downloading',
    gguf_parsed_at: null,
    tokenizer_model: null,
    n_vocab: null,
  });
  const projector = makeModel({ id: 'mdl_proj', kind: 'mmproj' });
  const deleted = makeModel({ id: 'mdl_gone', state: 'deleted' });
  const all = [primary, sameVocab, otherVocab, unparsed, projector, deleted];

  it('offers a still-downloading model as the primary — that flow is the point of D34', () => {
    const ids = primaryModelChoices(all).map((choice) => choice.value);
    expect(ids).toContain('mdl_downloading');
    expect(ids).not.toContain('mdl_proj');
    expect(ids).not.toContain('mdl_gone');
  });

  it('puts the projectors from the primary model’s own repository first', () => {
    const choices = mmprojChoices(all, primary);
    expect(choices[0]?.value).toBe('');
    expect(choices[1]?.value).toBe('mdl_proj');
    expect(choices[1]?.description).toContain('this model’s repository');
  });

  it('implements section 3.10a’s three-valued draft check', () => {
    expect(draftCompatibility(primary, sameVocab)).toBe('ok');
    expect(draftCompatibility(primary, otherVocab)).toBe('mismatch');
    expect(draftCompatibility(primary, unparsed)).toBe('deferred');
    expect(draftCompatibility(unparsed, sameVocab)).toBe('deferred');
  });

  it('disables the mismatched draft instead of letting the save be refused', () => {
    const choices = draftChoices(all, primary);
    const mismatch = choices.find((choice) => choice.value === 'mdl_gemma');
    expect(mismatch?.disabled).toBe(true);
    expect(mismatch?.description).toContain('Different vocabulary');
  });

  it('keeps a deferred draft selectable and says the check is owed', () => {
    const deferred = draftChoices(all, primary).find(
      (choice) => choice.value === 'mdl_downloading',
    );
    expect(deferred?.disabled).toBeUndefined();
    expect(deferred?.description).toContain('deferred');
  });

  it('never offers the primary model as its own draft', () => {
    expect(draftChoices(all, primary).map((c) => c.value)).not.toContain(primary.id);
  });

  it('warns when the draft is not smaller than the primary', () => {
    const choices = draftChoices([primary, sameVocab], primary);
    expect(choices.find((c) => c.value === 'mdl_draft')?.description).toContain('larger');
  });
});

describe('port hints', () => {
  const ctx = {
    ...DEFAULT_FORM_CONTEXT,
    managementPorts: [5526],
    claims: portClaims([
      makeInstance({ id: 'a', name: 'gemma', public_port: 8080, internal_port: 21000 }),
      makeInstance({
        id: 'b',
        name: 'deleted-one',
        public_port: 8081,
        internal_port: 21001,
        deleted_at: '2026-08-20T00:00:00Z',
      }),
    ]),
  };

  it('does not count a soft-deleted instance as a port holder (D68)', () => {
    expect(ctx.claims.map((claim) => claim.instance_id)).toEqual(['a']);
  });

  it('walks to the first free public port', () => {
    expect(suggestPort('public', ctx)).toBe(8081);
  });

  it('walks inside the pool for an internal port', () => {
    expect(suggestPort('internal', ctx)).toBe(21001);
  });

  it('says a blank field means the daemon allocates one', () => {
    expect(portHint('public', '', ctx)?.message).toContain('daemon allocates');
  });

  it('marks a taken port as a conflict rather than a warning', () => {
    expect(portHint('public', '8080', ctx)?.tone).toBe('danger');
  });

  it('marks a reserved port as a warning', () => {
    expect(portHint('public', '5526', ctx)?.tone).toBe('warn');
  });
});

describe('the daemon’s refusals land on the field that caused them', () => {
  const refuse = (code: string, details: Record<string, unknown> = {}) =>
    new ApiError(422, { code, message: `${code} message`, details }, 'POST /api/v1/instances');

  it('puts a taken name on the name field', () => {
    expect(refusalReport(refuse('instance_name_taken')).fields[0]?.field).toBe('name');
  });

  it('tells the two port fields apart by the pool', () => {
    expect(refusalReport(refuse('port_unavailable', { port: 21001 })).fields[0]?.field).toBe(
      'internal_port',
    );
    expect(refusalReport(refuse('port_unavailable', { port: 8081 })).fields[0]?.field).toBe(
      'public_port',
    );
  });

  it('puts ngl_auto_conflict on the tensor split, which is the half a user can change', () => {
    expect(refusalReport(refuse('ngl_auto_conflict')).fields[0]?.field).toBe('tensor_split');
  });

  it('puts a forbidden override on the escape hatch', () => {
    expect(refusalReport(refuse('extra_flag_forbidden')).fields[0]?.field).toBe('extra_flags');
  });

  it('reports a generation conflict as a banner with a reload, not as a bad field', () => {
    const report = refusalReport(refuse('conflict_generation'));
    expect(report.fields).toEqual([]);
    expect(report.stale).toBe(true);
    expect(report.banner).toContain('Someone else changed this instance');
  });

  it('says the daemon is unreachable when the failure was not an answer at all', () => {
    expect(refusalReport(new Error('network')).banner).toContain('did not answer');
  });

  it('reports nothing when there was no error', () => {
    expect(refusalReport(undefined)).toEqual({ fields: [] });
  });
});
