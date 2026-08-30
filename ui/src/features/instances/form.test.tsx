/**
 * The form renders every flag it claims to, in both themes, and says the right things about a
 * configuration that is legal but unwise.
 *
 * Rendering goes through `react-dom/server`, which needs no DOM environment — the same choice, for
 * the same reasons, as `components/kit.test.tsx` (see `vitest.config.ts`). What that proves here is
 * what matters for a sixty-field form: that every field is actually mounted with its upstream flag
 * beside it, that the panes exist, and that no color is spelled out in the markup rather than drawn
 * from the token palette.
 *
 * Field *validation* edges live in `schema.test.ts`, exercised through the same resolver this
 * component uses.
 */

import { renderToStaticMarkup } from 'react-dom/server';
import type { ReactElement } from 'react';
import { describe, expect, it } from 'vitest';

import { InstanceForm } from './components/InstanceForm';
import { FitPanel } from './components/FitPanel';
import { AdvisoryList } from './components/AdvisoryList';
import { ArgvPreview } from './components/ArgvPreview';
import { InstanceActions } from './components/InstanceActions';
import { InstanceBadges } from './components/InstanceBadges';
import { StartsTable } from './components/StartsTable';
import { UsageSparkline, sparklinePath } from './components/UsageSparkline';
import { NglAdvisoryCard } from './components/NglAdvisoryCard';
import { RemediationCard } from './components/RemediationCard';
import { formAdvisories } from './advisories';
import { remediationFor } from './remediation';
import { DEFAULT_FORM_CONTEXT } from './schema';
import { makeFitReport, makeInstance, makeModel, validFormValues } from './fixtures';
import { emptyFormValues, valuesFromInstance } from './values';
import type { InstanceStart } from '../../api/types';
import { ApiError } from '../../api/errors';

function render(node: ReactElement, theme: 'dark' | 'light' = 'dark'): string {
  return renderToStaticMarkup(<div data-theme={theme}>{node}</div>);
}

const LITERAL_COLOR = /#[0-9a-f]{3,8}\b|(?<!var\([^)]*)\b(?:rgb|hsl)a?\(/i;

const MODELS = [
  makeModel(),
  makeModel({
    id: 'mdl_draft',
    repo_id: 'bartowski/Qwen3-0.6B-GGUF',
    primary_file: 'Qwen3-0.6B-Q4_K_M.gguf',
    total_bytes: 400_000_000,
  }),
  makeModel({
    id: 'mdl_proj',
    kind: 'mmproj',
    primary_file: 'mmproj-Qwen3-8B-f16.gguf',
  }),
  makeModel({
    id: 'mdl_other_vocab',
    repo_id: 'unsloth/gemma-3-1b-GGUF',
    primary_file: 'gemma-3-1b-Q4_K_M.gguf',
    tokenizer_model: 'llama',
    n_vocab: 262_144,
  }),
];

function form(overrides: Partial<Parameters<typeof InstanceForm>[0]> = {}): ReactElement {
  return (
    <InstanceForm
      mode="create"
      defaultValues={validFormValues()}
      models={MODELS}
      devices={[
        { uuid: 'GPU-aaaa', index: 0, name: 'NVIDIA RTX 4090', freeBytes: 23_000_000_000 },
        { uuid: 'GPU-bbbb', index: 1, name: 'NVIDIA RTX 3090', freeBytes: 21_000_000_000 },
      ]}
      formContext={DEFAULT_FORM_CONTEXT}
      pane="model"
      onPaneChange={() => {}}
      onSubmit={() => {}}
      onCancel={() => {}}
      {...overrides}
    />
  );
}

describe('the three panes', () => {
  it('offers all three, named as DESIGN section 4 names them', () => {
    const html = render(form());
    for (const label of ['Model &amp; context', 'Performance', 'Advanced']) {
      expect(html).toContain(label);
    }
  });

  it('renders the model pane’s identity, model, context and network fields', () => {
    const html = render(form({ pane: 'model' }));
    for (const label of [
      'Name',
      'Display name',
      'Description',
      'Model',
      'Vision projector',
      'Draft model',
      'Context size',
      'Parallel slots',
      'Served model name',
      'Public port',
      'Internal port',
      'Authentication',
      'Restart policy',
      'Restart budget',
      'Restart window',
    ]) {
      expect(html).toContain(label);
    }
  });

  it('renders the performance pane down to the KV cache selects', () => {
    const html = render(form({ pane: 'performance' }));
    for (const flag of ['-ngl', '--device', '-sm', '-mg', '-ts', '-b', '-ub', '-t', '-tb', '-fa']) {
      expect(html).toContain(flag);
    }
    expect(html).toContain('K cache type');
    expect(html).toContain('V cache type');
    expect(html).toContain('-ctk');
    expect(html).toContain('-ctv');
    expect(html).toContain('--mlock');
    expect(html).toContain('--no-mmap');
  });

  it('renders the advanced pane, including the endpoints and the escape hatch', () => {
    const html = render(form({ pane: 'advanced' }));
    for (const flag of [
      '--embedding',
      '--reranking',
      '--pooling',
      '--jinja',
      '--chat-template',
      '--chat-template-file',
      '--keep',
      '--predict',
      '--defrag-thold',
      '--cache-reuse',
      '--rope-scaling',
      '--rope-freq-base',
      '--rope-freq-scale',
      '--yarn-ext-factor',
      '--yarn-attn-factor',
      '--draft-max',
      '--draft-min',
      '--draft-p-min',
      '-cd',
      '-ngld',
      '--numa',
      '--prio',
      '--verbosity',
      '--slot-save-path',
      '--props',
      '--slots',
      '--metrics',
    ]) {
      expect(html).toContain(flag);
    }
    expect(html).toContain('Extra flags');
  });

  it('names no color of its own, in either theme', () => {
    for (const theme of ['dark', 'light'] as const) {
      for (const pane of ['model', 'performance', 'advanced'] as const) {
        const html = render(form({ pane }), theme);
        expect(html).not.toMatch(LITERAL_COLOR);
      }
    }
  });
});

describe('what the form says about the values it is given', () => {
  it('explains that `auto` passes no -ngl at all', () => {
    expect(render(form({ pane: 'performance' }))).toContain('--fit');
  });

  it('warns when the build predates --fit', () => {
    const html = render(form({ pane: 'performance', supportsFit: false }));
    expect(html).toContain('predates --fit');
  });

  it('carries the fit advisory beside `auto` when an estimate exists', () => {
    const html = render(
      form({ pane: 'performance', fitReport: makeFitReport(), supportsFit: true }),
    );
    expect(html).toContain('37 of 36 layers fit');
  });

  it('offers the pinned count when the estimate is there', () => {
    const html = render(form({ pane: 'performance', fitReport: makeFitReport() }));
    expect(html).toContain('Pin 37');
  });

  it('surfaces a stale-generation conflict as its own banner, not a field error', () => {
    const conflict = new ApiError(
      409,
      { code: 'conflict_generation', message: 'the instance changed', details: {} },
      'PATCH /api/v1/instances/{id}',
    );
    const html = render(form({ mode: 'edit', submitError: conflict }));
    expect(html).toContain('Someone else changed this instance');
    expect(html).toContain('role="alert"');
  });

  it('reports a transport failure as the daemon not answering', () => {
    const html = render(form({ submitError: new Error('offline') }));
    expect(html).toContain('did not answer');
  });

  it('mounts the three model pickers with their upstream flags', () => {
    const html = render(form({ pane: 'model' }));
    expect(html).toContain('-m');
    expect(html).toContain('--mmproj');
    expect(html).toContain('-md');
  });
});

describe('advisories — legal, saved, and probably not what you meant', () => {
  it('flags a physical batch larger than the logical one', () => {
    const advisories = formAdvisories(validFormValues({ batch_size: '512', ubatch_size: '2048' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    expect(advisories.map((a) => a.code)).toContain('ubatch_over_batch');
  });

  it('explains that -c is the total context, and says how it divides', () => {
    const advisories = formAdvisories(validFormValues({ ctx_size: '8192', parallel: '4' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    const perSlot = advisories.find((a) => a.code === 'ctx_per_slot');
    expect(perSlot?.message).toContain('2048 tokens each');
  });

  it('warns when the context does not divide evenly', () => {
    const advisories = formAdvisories(validFormValues({ ctx_size: '8000', parallel: '3' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    expect(advisories.find((a) => a.code === 'ctx_per_slot')?.tone).toBe('warn');
  });

  it('notices draft tuning with no draft model attached', () => {
    const advisories = formAdvisories(validFormValues({ draft_n_max: '16' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    expect(advisories.map((a) => a.code)).toContain('draft_without_model');
  });

  it('says what turning --metrics off costs', () => {
    const advisories = formAdvisories(validFormValues({ metrics_endpoint: 'off' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    expect(advisories.find((a) => a.code === 'metrics_disabled')?.message).toContain(
      'metrics disabled',
    );
  });

  it('warns rather than refuses when extra_flags duplicates a modeled field', () => {
    const advisories = formAdvisories(validFormValues({ extra_flags: '-c 4096 --jinja' }), {
      deviceCount: 0,
      hasDraftModel: false,
    });
    const duplicate = advisories.find((a) => a.code === 'extra_flags_duplicate');
    expect(duplicate?.tone).toBe('warn');
    expect(duplicate?.message).toContain('-c');
    expect(duplicate?.message).toContain('appended last');
  });

  it('counts tensor-split weights against the selected devices', () => {
    const advisories = formAdvisories(
      validFormValues({ ngl_mode: 'all', tensor_split: '0.5, 0.3, 0.2' }),
      { deviceCount: 2, hasDraftModel: false },
    );
    expect(advisories.map((a) => a.code)).toContain('tensor_split_arity');
  });

  it('says nothing at all about a plain configuration', () => {
    const advisories = formAdvisories(emptyFormValues(), { deviceCount: 0, hasDraftModel: false });
    expect(advisories.map((a) => a.code)).toEqual([]);
  });

  it('renders each advisory with its code, so a screen can be inspected', () => {
    const html = render(
      <AdvisoryList
        advisories={formAdvisories(validFormValues({ auth_mode: 'none' }), {
          deviceCount: 0,
          hasDraftModel: false,
        })}
      />,
    );
    expect(html).toContain('data-advisory="auth_none"');
  });
});

describe('the fit panel', () => {
  it('shows the verdict, the per-device row and the per-GPU terms', () => {
    const html = render(<FitPanel report={makeFitReport()} />);
    expect(html).toContain('Fits in VRAM');
    expect(html).toContain('CUDA0');
    expect(html).toContain('Safety margin, per GPU');
    expect(html).toContain('Backend overhead, per GPU');
    expect(html).toContain('Required VRAM, total');
  });

  it('marks a device that is short, rather than only failing the verdict', () => {
    const report = makeFitReport({
      verdict: 'wont_run',
      per_gpu: [
        {
          ...makeFitReport().per_gpu[0]!,
          ok: false,
          short_by_bytes: 2_147_483_648,
        },
      ],
    });
    const html = render(<FitPanel report={report} />);
    expect(html).toContain('short 2.00 GiB');
    expect(html).toContain('Won&#x27;t run');
  });

  it('distinguishes a modeled estimate from a calibrated one', () => {
    const html = render(<FitPanel report={makeFitReport({ confidence: 'modeled' })} />);
    expect(html).toContain('no observation from this host yet');
  });
});

describe('the offload advisory on the instance page', () => {
  it('says what `auto` means and what we would have chosen', () => {
    const html = render(
      <NglAdvisoryCard ngl={{ mode: 'auto' }} report={makeFitReport()} onPin={() => {}} />,
    );
    expect(html).toContain('llama.cpp’s own --fit decides at load time');
    expect(html).toContain('37 of 36 layers fit');
    expect(html).toContain('Pin 37 layers');
    expect(html).toContain('restart required');
  });

  it('offers no pin for a configuration that already names a count', () => {
    const html = render(
      <NglAdvisoryCard
        ngl={{ mode: 'count', count: 24 }}
        report={makeFitReport()}
        onPin={() => {}}
      />,
    );
    expect(html).toContain('-ngl 24');
    expect(html).not.toContain('Pin ');
  });

  it('shows the CPU-only case as the decision it is', () => {
    expect(render(<NglAdvisoryCard ngl={{ mode: 'none' }} />)).toContain('runs on the CPU');
  });
});

describe('the command-line preview', () => {
  it('is the daemon’s rendering, wrapped one flag per line', () => {
    const html = render(
      <ArgvPreview
        argv={['/versions/b1/bin/llama-server', '-m', '/models/q.gguf', '-c', '8192', '--jinja']}
        unit="llamaman-instance@qwen3-8b.service"
      />,
    );
    expect(html).toContain('-m /models/q.gguf');
    expect(html).toContain('llamaman-instance@qwen3-8b.service');
  });

  it('says why there is nothing to show rather than showing an empty box', () => {
    const html = render(
      <ArgvPreview unavailable="This daemon does not serve /instances/validate yet." />,
    );
    expect(html).toContain('does not serve /instances/validate yet');
  });

  it('badges the flags the active build does not advertise', () => {
    const html = render(
      <ArgvPreview argv={['llama-server', '--newthing']} unknownFlags={['--newthing']} />,
    );
    expect(html).toContain('--newthing');
  });
});

describe('the instance controls', () => {
  it('offers stop while running and start while stopped', () => {
    expect(
      render(
        <InstanceActions state="ready" desiredState="running" autostart onAction={() => {}} />,
      ),
    ).toContain('Stop');
    expect(
      render(
        <InstanceActions
          state="stopped"
          desiredState="stopped"
          autostart={false}
          onAction={() => {}}
        />,
      ),
    ).toContain('Start');
  });

  it('offers reset failed only where it means something', () => {
    const looping = render(
      <InstanceActions
        state="crash-looping"
        desiredState="running"
        autostart
        onAction={() => {}}
      />,
    );
    expect(looping).toContain('Reset failed');
    const ready = render(
      <InstanceActions state="ready" desiredState="running" autostart onAction={() => {}} />,
    );
    expect(ready).not.toContain('Reset failed');
  });

  it('disables everything when the daemon cannot control units', () => {
    const html = render(
      <InstanceActions state="ready" desiredState="running" autostart onAction={null} />,
    );
    expect(html).toContain('disabled');
    expect(html).toContain('cannot control units');
  });
});

describe('the derived flags are badges, never states', () => {
  it('shows restart-required beside a ready instance', () => {
    const html = render(<InstanceBadges instance={makeInstance({ restart_required: true })} />);
    expect(html).toContain('Restart required');
  });

  it('shows a draft mismatch as an error, not as the deferred badge', () => {
    const html = render(
      <InstanceBadges
        instance={makeInstance({ draft_validation: 'mismatch', draft_unverified: false })}
      />,
    );
    expect(html).toContain('Draft mismatch');
  });

  it('renders nothing for a clean instance', () => {
    expect(render(<InstanceBadges instance={makeInstance()} />)).toBe(
      '<div data-theme="dark"></div>',
    );
  });
});

describe('the start ledger', () => {
  const start = (overrides: Partial<InstanceStart>): InstanceStart =>
    ({
      id: 'st_1',
      at: '2026-08-20T09:00:00Z',
      trigger: 'user',
      config_hash: 'a'.repeat(64),
      effective_config_hash: 'a'.repeat(64),
      override: {},
      argv: ['llama-server'],
      llamacpp_version_id: 'b10621',
      ready_at: null,
      outcome: null,
      exit_code: null,
      error_code: null,
      error_message: null,
      detail: {},
      ended_at: null,
      ...overrides,
    }) as InstanceStart;

  it('shows the open row as in flight rather than as an outcome', () => {
    expect(render(<StartsTable starts={[start({})]} />)).toContain('In flight');
  });

  it('shows a preflight refusal that never rendered a command line', () => {
    const html = render(
      <StartsTable
        starts={[
          start({
            argv: [],
            outcome: 'failed',
            error_code: 'port_conflict',
            exit_code: 78,
            ended_at: '2026-08-20T09:00:05Z',
          }),
        ]}
      />,
    );
    expect(html).toContain('port_conflict');
    expect(html).toContain('exit 78');
  });

  it('shows a safe start with its override inline', () => {
    const html = render(
      <StartsTable starts={[start({ trigger: 'safe_start', override: { ctx_size: 2048 } })]} />,
    );
    expect(html).toContain('-ngl 0');
    expect(html).toContain('ctx_size');
  });
});

describe('the remediation card', () => {
  it('answers a crash loop with safe start', () => {
    const remediation = remediationFor({ inhibited: true, inhibitReason: 'crash_loop' });
    expect(remediation?.action?.kind).toBe('safe-start');
    expect(render(<RemediationCard remediation={remediation!} onAction={() => {}} />)).toContain(
      'Safe start',
    );
  });

  it('answers exit 72 with the model, and prints degraded-mode commands verbatim', () => {
    const remediation = remediationFor({ exitCode: 72 });
    expect(remediation?.action?.kind).toBe('models');
    const html = render(
      <RemediationCard
        remediation={remediation!}
        hints={['sudo systemctl disable llamaman-instance@qwen3-8b.service']}
      />,
    );
    expect(html).toContain('sudo systemctl disable llamaman-instance@qwen3-8b.service');
  });

  it('says nothing when there is nothing to say', () => {
    expect(remediationFor({ exitCode: 0 })).toBeNull();
    expect(remediationFor({})).toBeNull();
  });

  it('reads a clean exit under on-failure as a decision, not a failure', () => {
    const remediation = remediationFor({ inhibited: true, inhibitReason: 'clean_exit' });
    expect(remediation?.title).toContain('exited cleanly');
  });
});

describe('the usage sparkline', () => {
  it('scales the series into the box, newest last', () => {
    expect(sparklinePath([0, 5, 10], 40)).toBe('0.00,40.00 50.00,20.00 100.00,0.00');
  });

  it('says "no traffic yet" rather than drawing a flat line', () => {
    const html = render(<UsageSparkline points={[{ label: '2026-08-20', value: 0 }]} />);
    expect(html).toContain('No traffic yet');
  });

  it('summarizes the series for a screen reader', () => {
    const html = render(
      <UsageSparkline
        points={[
          { label: '2026-08-19', value: 10 },
          { label: '2026-08-20', value: 32 },
        ]}
      />,
    );
    expect(html).toContain('42 requests over 2 days');
  });
});

describe('an edited instance seeds the form from its own row', () => {
  it('round-trips the saved flags into the fields', () => {
    const values = valuesFromInstance(makeInstance());
    expect(values.name).toBe('qwen3-8b');
    expect(values.ctx_size).toBe('8192');
    expect(values.ngl_mode).toBe('all');
    expect(values.cache_type_k).toBe('q8_0');
    expect(values.flash_attn).toBe('on');
    expect(values.jinja).toBe('on');
    // A flag the row does not carry stays blank, which is "do not pass it".
    expect(values.threads).toBe('');
    expect(values.mlock).toBe('');
  });

  it('renders in edit mode without the boot toggle, which is not a config edit', () => {
    const html = render(
      form({ mode: 'edit', defaultValues: valuesFromInstance(makeInstance()), pane: 'model' }),
    );
    expect(html).toContain('Autostart is toggled from the instance itself');
  });
});
