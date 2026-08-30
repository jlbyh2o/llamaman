/**
 * The form refuses exactly what the daemon refuses.
 *
 * Every case below has a counterpart on the Go side — `internal/instances/validate.go`,
 * `model.FlagSet.Validate`, `internal/instances/ports.go`'s rule table,
 * `internal/instances/extraflags.go` — and the point of the suite is that the two never drift into
 * "the form let me save it and the daemon said no", or the worse direction: "the form refused
 * something the daemon would have accepted".
 *
 * The resolver is exercised the way the component uses it (`zodResolver` → react-hook-form field
 * errors), so a passing test means the message really does land on that field.
 */

import { describe, expect, it } from 'vitest';
import { zodResolver } from '@hookform/resolvers/zod';
import { createInstanceFormSchema, checkPort, DEFAULT_FORM_CONTEXT, NAME_PATTERN } from './schema';
import type { FormContext } from './schema';
import { validFormValues } from './fixtures';
import type { InstanceFormValues } from './values';

/**
 * The host these cases are judged against: another instance holding 8082/21002, and the row being
 * edited holding 8081/21001 — which is exactly the case `excludeInstanceId` exists for, since a
 * PATCH that changes nothing about the ports must not fail its own validation.
 */
const CTX: FormContext = {
  ...DEFAULT_FORM_CONTEXT,
  managementPorts: [5526],
  excludeInstanceId: 'inst_self',
  claims: [
    { instance_id: 'inst_other', name: 'gemma', public_port: 8082, internal_port: 21002 },
    { instance_id: 'inst_self', name: 'qwen3-8b', public_port: 8081, internal_port: 21001 },
  ],
};

/** Parse and return a `field → message` map, exactly as the resolver hands it to the form. */
async function errorsFor(
  values: Partial<InstanceFormValues>,
  ctx: FormContext = CTX,
): Promise<Record<string, string>> {
  const resolver = zodResolver(createInstanceFormSchema(ctx));
  const result = await resolver(
    validFormValues(values) as never,
    undefined as never,
    { fields: {}, shouldUseNativeValidation: false } as never,
  );
  const out: Record<string, string> = {};
  for (const [field, error] of Object.entries(result.errors ?? {})) {
    const message = (error as { message?: unknown }).message;
    if (typeof message === 'string') out[field] = message;
  }
  return out;
}

describe('name — D11, because it becomes a systemd unit id', () => {
  it.each([
    ['q', true],
    ['qwen3-8b', true],
    ['0', true],
    ['a'.repeat(32), true],
    ['a'.repeat(33), false],
    ['', false],
    ['Qwen', false],
    ['-leading', false],
    ['has space', false],
    ['under_score', false],
    ['trailing.', false],
  ])('%s → %s', (name, valid) => {
    expect(NAME_PATTERN.test(name)).toBe(valid);
  });

  it('reports the unit-name reason, not a generic pattern failure', async () => {
    const errors = await errorsFor({ name: 'Qwen' });
    expect(errors['name']).toContain('systemd unit name');
  });

  it('requires a name at all', async () => {
    expect(await errorsFor({ name: '' })).toHaveProperty('name');
  });
});

describe('model', () => {
  it('is required — an instance with no model has nothing to serve', async () => {
    expect(await errorsFor({ model_id: '' })).toHaveProperty('model_id');
  });
});

describe('the four positive integers of FlagSet.Validate', () => {
  it.each(['ctx_size', 'batch_size', 'ubatch_size', 'parallel'] as const)(
    '%s rejects zero with the server’s own wording',
    async (field) => {
      const errors = await errorsFor({ [field]: '0' } as Partial<InstanceFormValues>);
      expect(errors[field]).toBe(`${field} must be greater than zero`);
    },
  );

  it.each(['ctx_size', 'batch_size', 'ubatch_size', 'parallel'] as const)(
    '%s rejects a negative value',
    async (field) => {
      expect(await errorsFor({ [field]: '-1' } as Partial<InstanceFormValues>)).toHaveProperty(
        field,
      );
    },
  );

  it('accepts an empty value — null means "do not pass the flag"', async () => {
    const errors = await errorsFor({ ctx_size: '', batch_size: '', parallel: '' });
    expect(errors).toEqual({});
  });

  it('rejects a fraction where llama.cpp takes a count', async () => {
    expect(await errorsFor({ ctx_size: '8192.5' })).toHaveProperty('ctx_size');
  });

  it('rejects text', async () => {
    expect(await errorsFor({ parallel: 'four' })).toHaveProperty('parallel');
  });
});

describe('-ngl, the four modes of D51', () => {
  it('needs a count in count mode', async () => {
    const errors = await errorsFor({ ngl_mode: 'count', ngl_count: '' });
    expect(errors['ngl_count']).toContain('no count was given');
  });

  it('rejects a negative count', async () => {
    const errors = await errorsFor({ ngl_mode: 'count', ngl_count: '-1' });
    expect(errors['ngl_count']).toContain('negative');
  });

  it('accepts zero layers in count mode — that is CPU-only, spelled explicitly', async () => {
    expect(await errorsFor({ ngl_mode: 'count', ngl_count: '0' })).toEqual({});
  });

  it('ignores the count outside count mode', async () => {
    expect(await errorsFor({ ngl_mode: 'auto', ngl_count: 'nonsense' })).toEqual({});
  });

  it('refuses `auto` together with an explicit tensor split (ngl_auto_conflict)', async () => {
    const errors = await errorsFor({ ngl_mode: 'auto', tensor_split: '0.5, 0.5' });
    expect(errors['tensor_split']).toContain('--fit');
  });

  it('allows a tensor split in every other mode', async () => {
    expect(await errorsFor({ ngl_mode: 'all', tensor_split: '0.5, 0.5' })).toEqual({});
  });
});

describe('tensor split', () => {
  it('rejects a negative weight, naming its index like the server does', async () => {
    const errors = await errorsFor({ ngl_mode: 'all', tensor_split: '0.5, -0.2' });
    expect(errors['tensor_split']).toBe('tensor_split[1] is negative');
  });

  it('rejects a non-number', async () => {
    const errors = await errorsFor({ ngl_mode: 'all', tensor_split: '0.5, half' });
    expect(errors['tensor_split']).toContain('not a number');
  });

  it('accepts either separator, because a human writes both', async () => {
    expect(await errorsFor({ ngl_mode: 'all', tensor_split: '0.5 0.5' })).toEqual({});
    expect(await errorsFor({ ngl_mode: 'all', tensor_split: '0.5,0.5' })).toEqual({});
  });
});

describe('main_gpu', () => {
  it('may not be negative', async () => {
    expect(await errorsFor({ main_gpu: '-1' })).toHaveProperty('main_gpu');
  });

  it('may be zero — the first device in the --device list', async () => {
    expect(await errorsFor({ main_gpu: '0' })).toEqual({});
  });
});

describe('ports — section 2.8’s table, minus the bind probe', () => {
  it('refuses a privileged port', async () => {
    const errors = await errorsFor({ public_port: '80' });
    expect(errors['public_port']).toContain('1024-65535');
  });

  it('refuses a port above the range', async () => {
    expect(await errorsFor({ public_port: '70000' })).toHaveProperty('public_port');
  });

  it('refuses the management UI’s port as a public port', async () => {
    const errors = await errorsFor({ public_port: '5526' });
    expect(errors['public_port']).toContain('management UI');
  });

  it('refuses a public port inside the internal pool', async () => {
    const errors = await errorsFor({ public_port: '21500' });
    expect(errors['public_port']).toContain('internal pool');
  });

  it('refuses an internal port outside the pool', async () => {
    const errors = await errorsFor({ internal_port: '9000' });
    expect(errors['internal_port']).toContain('inside the pool');
  });

  it('refuses a port another live instance holds, and names it', async () => {
    const errors = await errorsFor({ public_port: '8082' });
    expect(errors['public_port']).toContain('gemma');
  });

  it('allows the edited instance to keep its own ports', async () => {
    expect(await errorsFor({ public_port: '8081', internal_port: '21001' })).toEqual({});
  });

  it('refuses those same ports when a different instance is being created', async () => {
    const { excludeInstanceId: _omitted, ...creating } = CTX;
    const errors = await errorsFor({ public_port: '8081' }, creating);
    expect(errors['public_port']).toContain('qwen3-8b');
  });

  it('cannot be given the same port twice — the pool partitions the two fields', async () => {
    // A public port inside the pool is refused as reserved; an internal port outside it is refused
    // as out of pool. So the collision the table prevents shows up as those two rules, not a third.
    const errors = await errorsFor(
      { public_port: '21001', internal_port: '8081' },
      { ...CTX, claims: [] },
    );
    expect(errors['public_port']).toContain('internal pool');
    expect(errors['internal_port']).toContain('inside the pool');
  });

  it('accepts both blank — the daemon allocates them', async () => {
    expect(await errorsFor({ public_port: '', internal_port: '' })).toEqual({});
  });

  it('checkPort agrees with the resolver about why', () => {
    expect(checkPort('public', 5526, CTX).reason).toBe('reserved_management');
    expect(checkPort('public', 21500, CTX).reason).toBe('reserved_internal_pool');
    expect(checkPort('internal', 9000, CTX).reason).toBe('outside_internal_pool');
    expect(checkPort('public', 8082, CTX).reason).toBe('in_use_by_instance');
    expect(checkPort('public', 8090, CTX).ok).toBe(true);
  });
});

describe('extra_flags — SPEC section 3.3’s escape hatch', () => {
  it.each([
    '--host 0.0.0.0',
    '--port 9000',
    '-m /other.gguf',
    '--model /other.gguf',
    '--api-key x',
  ])('refuses %s', async (value) => {
    const errors = await errorsFor({ extra_flags: value });
    expect(errors['extra_flags']).toContain('may not override');
  });

  it('refuses the `=` spelling of a forbidden flag too', async () => {
    expect(await errorsFor({ extra_flags: '--port=9000' })).toHaveProperty('extra_flags');
  });

  it('refuses an unterminated quote rather than guessing', async () => {
    const errors = await errorsFor({ extra_flags: '--lora "adapter.gguf' });
    expect(errors['extra_flags']).toContain('unterminated');
  });

  it('refuses a trailing backslash', async () => {
    expect(await errorsFor({ extra_flags: 'foo \\' })).toHaveProperty('extra_flags');
  });

  it('accepts an unmodeled flag, which is the whole point', async () => {
    expect(
      await errorsFor({ extra_flags: '--lora /models/adapter.gguf --lora-scale 0.8' }),
    ).toEqual({});
  });

  it('does not mistake a negative number for a flag', async () => {
    expect(await errorsFor({ extra_flags: '--some-threshold -0.5' })).toEqual({});
  });
});

describe('restart policy budget', () => {
  it('refuses a negative budget', async () => {
    expect(await errorsFor({ restart_max: '-1' })).toHaveProperty('restart_max');
  });

  it('refuses a zero-length window', async () => {
    expect(await errorsFor({ restart_window_sec: '0' })).toHaveProperty('restart_window_sec');
  });
});

describe('speculative decoding tuning', () => {
  it('keeps the acceptance threshold a probability', async () => {
    expect(await errorsFor({ draft_p_min: '1.5' })).toHaveProperty('draft_p_min');
    expect(await errorsFor({ draft_p_min: '-0.1' })).toHaveProperty('draft_p_min');
    expect(await errorsFor({ draft_p_min: '0.75' })).toEqual({});
  });

  it('refuses a minimum larger than the maximum', async () => {
    const errors = await errorsFor({ draft_n_max: '4', draft_n_min: '8' });
    expect(errors['draft_n_min']).toContain('cannot exceed');
  });
});

describe('a configuration that is fine', () => {
  it('produces no errors at all', async () => {
    expect(
      await errorsFor({
        ngl_mode: 'count',
        ngl_count: '37',
        flash_attn: 'auto',
        cache_type_k: 'q8_0',
        cache_type_v: 'q8_0',
        split_mode: 'layer',
        tensor_split: '0.5, 0.5',
        batch_size: '2048',
        ubatch_size: '512',
        threads: '16',
        mlock: 'on',
        no_mmap: 'off',
        cont_batching: 'on',
        jinja: 'on',
        extra_flags: '--lora /models/adapter.gguf',
      }),
    ).toEqual({});
  });
});
