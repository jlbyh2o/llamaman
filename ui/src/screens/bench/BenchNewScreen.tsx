/**
 * `/bench/new` — the sweep builder (DESIGN section 4, screen 13).
 *
 * Every axis left blank means "one point, the default"; `sweepPointCount` in `features/bench/sweep`
 * is the same cross-product `bench_points` will expand to, computed here for the instant local
 * estimate and confirmed by the debounced call to `GET /bench/preflight`, which is the one that
 * knows about GPU conflicts and can turn history into an actual time estimate.
 *
 * "Run now" is gated behind a confirmation whenever the preflight reports a conflict — the
 * exclusivity warning names every instance that would be stopped, because `on_conflict:
 * "stop_and_restore"` acts on other people's running instances without asking twice otherwise.
 */

import { useMemo, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { AlertTriangle, Info, Plus, Trash2 } from 'lucide-react';
import type { Model } from '../../api/types';
import {
  Button,
  ConfirmDialog,
  EmptyState,
  FieldGroup,
  FormField,
  Input,
  Panel,
  PanelHeader,
  Select,
  Switch,
  describeError,
  toast,
} from '../../components';
import type { FieldRenderProps } from '../../components';
import { formatEstimate } from '../../format';
import {
  useCreateBenchRun,
  useDebouncedValue,
  usePreflight,
  useReadyModels,
  useStartBenchRun,
} from '../../features/bench/hooks';
import type { Sweep, SweepTest } from '../../features/bench/sweep';
import {
  CACHE_TYPES,
  SPLIT_MODES,
  SWEEP_SOFT_CAP,
  parseNGpuLayersList,
  parseNumberList,
  parseStringList,
  sweepPointCount,
} from '../../features/bench/sweep';

function modelLabel(model: Model): string {
  const quant = model.quant_label ? ` · ${model.quant_label}` : '';
  return `${model.repo_id}${quant}`;
}

/**
 * `Select`'s aria props are plain optionals (`'aria-describedby'?: string`), not `| undefined`, so
 * `FormField`'s render prop — which always hands back the key, `undefined` value included — cannot
 * be spread onto it directly under `exactOptionalPropertyTypes`. This drops the undefined ones.
 */
function selectFieldProps(field: FieldRenderProps): {
  id: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean;
} {
  return {
    id: field.id,
    ...(field['aria-describedby'] !== undefined
      ? { 'aria-describedby': field['aria-describedby'] }
      : {}),
    ...(field['aria-invalid'] !== undefined ? { 'aria-invalid': field['aria-invalid'] } : {}),
  };
}

/** One comma-separated axis, with the parsed count shown so a typo reads as "0 values" immediately. */
function AxisField({
  label,
  hint,
  flag,
  placeholder,
  value,
  onChange,
  count,
}: {
  label: string;
  hint: string;
  flag?: string;
  placeholder: string;
  value: string;
  onChange: (value: string) => void;
  count: number;
}) {
  return (
    <FormField
      label={label}
      hint={`${hint}${count > 0 ? ` — ${count} value${count === 1 ? '' : 's'}` : ' — model default'}`}
      {...(flag ? { flag } : {})}
    >
      {(field) => (
        <Input
          {...field}
          value={value}
          placeholder={placeholder}
          onChange={(event) => onChange(event.target.value)}
        />
      )}
    </FormField>
  );
}

export function BenchNewScreen() {
  const navigate = useNavigate();

  const models = useReadyModels();

  const [modelId, setModelId] = useState<string | undefined>(undefined);
  const [name, setName] = useState('');
  const [repetitions, setRepetitions] = useState(3);
  const [onConflict, setOnConflict] = useState<'stop_and_restore' | 'abort'>('stop_and_restore');

  const [ngl, setNgl] = useState('');
  const [batch, setBatch] = useState('');
  const [ubatch, setUbatch] = useState('');
  const [threads, setThreads] = useState('');
  const [flashOn, setFlashOn] = useState(false);
  const [flashOff, setFlashOff] = useState(false);
  const [typeK, setTypeK] = useState('');
  const [typeV, setTypeV] = useState('');
  const [splitMode, setSplitMode] = useState('');
  const [tensorSplit, setTensorSplit] = useState('');
  const [tests, setTests] = useState<SweepTest[]>([{ tg: 128 }]);

  const flashAttn =
    flashOn && flashOff ? [true, false] : flashOn ? [true] : flashOff ? [false] : [];

  const sweep: Sweep = useMemo(
    () => ({
      n_gpu_layers: parseNGpuLayersList(ngl),
      n_batch: parseNumberList(batch),
      n_ubatch: parseNumberList(ubatch),
      n_threads: parseNumberList(threads),
      flash_attn: flashAttn,
      type_k: parseStringList(typeK),
      type_v: parseStringList(typeV),
      split_mode: parseStringList(splitMode),
      tensor_split: parseStringList(tensorSplit),
      tests,
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [ngl, batch, ubatch, threads, flashOn, flashOff, typeK, typeV, splitMode, tensorSplit, tests],
  );

  const pointCount = sweepPointCount(sweep);
  const debouncedSweep = useDebouncedValue(sweep, 500);
  const debouncedModelId = useDebouncedValue(modelId, 300);

  const preflight = usePreflight({
    modelId: debouncedModelId,
    sweep: debouncedSweep,
    repetitions,
  });

  const create = useCreateBenchRun();
  const start = useStartBenchRun();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [pendingDraft, setPendingDraft] = useState(false);

  const conflicts = preflight.data?.conflicts ?? [];
  const busy = create.isPending || start.isPending;

  /** Set or clear one field of a test row. `raw === ''` clears it rather than storing `undefined`. */
  function updateTest(index: number, key: keyof SweepTest, raw: string) {
    setTests((prev) =>
      prev.map((t, i) => {
        if (i !== index) return t;
        const next = { ...t };
        if (raw === '') delete next[key];
        else next[key] = Number(raw);
        return next;
      }),
    );
  }

  function removeTest(index: number) {
    setTests((prev) => prev.filter((_, i) => i !== index));
  }

  async function submit(draft: boolean) {
    if (!modelId) return;
    try {
      const result = await create.mutateAsync({
        modelId,
        sweep,
        repetitions,
        onConflict,
        draft,
        ...(name.trim() ? { name: name.trim() } : {}),
      });
      if (!draft) {
        void navigate({ to: '/bench/$id', params: { id: result.run.id } });
        return;
      }
      // A draft with no conflicts and a deliberate "run now" click still needs a second request —
      // `start` — because `draft:false` already queued it above and this branch is the other one.
      void navigate({ to: '/bench/$id', params: { id: result.run.id } });
    } catch (err) {
      const { title, description } = describeError(err);
      toast.error(title, { description });
    }
  }

  function onRunNow() {
    if (conflicts.length > 0) {
      setPendingDraft(false);
      setConfirmOpen(true);
      return;
    }
    void submit(false);
  }

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-6">
      <PanelHeader
        level={1}
        title="New benchmark"
        description="Sweep n_gpu_layers, batch sizes and cache types across one model, measured with the active llama.cpp build's llama-bench."
      />

      <Panel className="space-y-4">
        <FieldGroup title="Model">
          <FormField label="Model to benchmark" required>
            {(field) => (
              <Select
                {...selectFieldProps(field)}
                value={modelId}
                onValueChange={setModelId}
                placeholder={models.isLoading ? 'Loading models…' : 'Choose a model'}
                options={(models.data?.items ?? []).map((model: Model) => ({
                  value: model.id,
                  label: modelLabel(model),
                  ...(model.arch ? { description: model.arch } : {}),
                }))}
              />
            )}
          </FormField>
          <FormField label="Name" hint="Optional — defaults to the model and the date.">
            {(field) => (
              <Input
                {...field}
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="qwen3-8b baseline"
              />
            )}
          </FormField>
          <FormField label="Repetitions" flag="-r" hint="llama-bench's own average, per point.">
            {(field) => (
              <Input
                {...field}
                type="number"
                min={1}
                max={20}
                mono
                value={repetitions}
                onChange={(event) => setRepetitions(Number(event.target.value) || 1)}
              />
            )}
          </FormField>
        </FieldGroup>

        <FieldGroup
          title="Sweep axes"
          description="Comma-separated values. Leave an axis blank to use the model's default for it."
        >
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <AxisField
              label="GPU layers"
              flag="-ngl"
              hint="Numbers, or all / none."
              placeholder="0, 20, all"
              value={ngl}
              onChange={setNgl}
              count={parseNGpuLayersList(ngl).length}
            />
            <AxisField
              label="Batch size"
              flag="-b"
              hint="Logical batch size."
              placeholder="512, 2048"
              value={batch}
              onChange={setBatch}
              count={parseNumberList(batch).length}
            />
            <AxisField
              label="Micro-batch size"
              flag="-ub"
              hint="Physical batch size."
              placeholder="256, 512"
              value={ubatch}
              onChange={setUbatch}
              count={parseNumberList(ubatch).length}
            />
            <AxisField
              label="Threads"
              flag="-t"
              hint="CPU threads for generation."
              placeholder="8, 16"
              value={threads}
              onChange={setThreads}
              count={parseNumberList(threads).length}
            />
            <AxisField
              label="Cache type (K)"
              flag="-ctk"
              hint={`One of ${CACHE_TYPES.join(', ')}.`}
              placeholder="f16, q8_0"
              value={typeK}
              onChange={setTypeK}
              count={parseStringList(typeK).length}
            />
            <AxisField
              label="Cache type (V)"
              flag="-ctv"
              hint={`One of ${CACHE_TYPES.join(', ')}.`}
              placeholder="f16, q8_0"
              value={typeV}
              onChange={setTypeV}
              count={parseStringList(typeV).length}
            />
            <AxisField
              label="Split mode"
              flag="-sm"
              hint={`One of ${SPLIT_MODES.join(', ')}.`}
              placeholder="layer, row"
              value={splitMode}
              onChange={setSplitMode}
              count={parseStringList(splitMode).length}
            />
            <AxisField
              label="Tensor split"
              flag="-ts"
              hint="Comma-separated ratio sets, e.g. one per value."
              placeholder="0.5,0.5"
              value={tensorSplit}
              onChange={setTensorSplit}
              count={parseStringList(tensorSplit).length}
            />
          </div>

          <FormField label="Flash attention" flag="-fa" hint="Test either or both.">
            {() => (
              <div className="flex items-center gap-5">
                <label className="flex items-center gap-2 text-sm text-[var(--lm-text)]">
                  <Switch checked={flashOn} onCheckedChange={setFlashOn} />
                  On
                </label>
                <label className="flex items-center gap-2 text-sm text-[var(--lm-text)]">
                  <Switch checked={flashOff} onCheckedChange={setFlashOff} />
                  Off
                </label>
              </div>
            )}
          </FormField>
        </FieldGroup>

        <FieldGroup
          title="Tests"
          description="Each row is one llama-bench invocation shape: prompt tokens, generation tokens, and an optional context depth."
        >
          <div className="space-y-2">
            {tests.map((test, index) => (
              <div key={index} className="flex items-end gap-2">
                <FormField label="Prompt (pp)" flag="-p" className="w-28">
                  {(field) => (
                    <Input
                      {...field}
                      type="number"
                      mono
                      min={0}
                      value={test.pp ?? ''}
                      onChange={(event) => updateTest(index, 'pp', event.target.value)}
                    />
                  )}
                </FormField>
                <FormField label="Generate (tg)" flag="-n" className="w-28">
                  {(field) => (
                    <Input
                      {...field}
                      type="number"
                      mono
                      min={0}
                      value={test.tg ?? ''}
                      onChange={(event) => updateTest(index, 'tg', event.target.value)}
                    />
                  )}
                </FormField>
                <FormField label="Depth" flag="-d" className="w-28">
                  {(field) => (
                    <Input
                      {...field}
                      type="number"
                      mono
                      min={0}
                      value={test.depth ?? ''}
                      onChange={(event) => updateTest(index, 'depth', event.target.value)}
                    />
                  )}
                </FormField>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Remove test"
                  onClick={() => removeTest(index)}
                  disabled={tests.length <= 1}
                >
                  <Trash2 />
                </Button>
              </div>
            ))}
            <Button
              variant="secondary"
              size="sm"
              icon={<Plus />}
              onClick={() => setTests((prev) => [...prev, { tg: 128 }])}
            >
              Add test
            </Button>
          </div>
        </FieldGroup>
      </Panel>

      <Panel className="space-y-3">
        <PanelHeader
          title="Preview"
          description={`${pointCount} point${pointCount === 1 ? '' : 's'} in this sweep${
            pointCount > SWEEP_SOFT_CAP ? ' — large sweeps take a long time and may be refused' : ''
          }.`}
        />

        {pointCount > SWEEP_SOFT_CAP ? (
          <div className="flex items-start gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)] p-3 text-xs text-[var(--lm-text)]">
            <AlertTriangle aria-hidden className="mt-0.5 size-4 shrink-0 text-[var(--lm-warn)]" />
            <span>
              {pointCount} points is a lot to run in one sweep — each point reloads the model.
              Consider narrowing an axis.
            </span>
          </div>
        ) : null}

        {!modelId ? (
          <p className="text-xs text-[var(--lm-text-muted)]">
            Choose a model to see a live estimate.
          </p>
        ) : preflight.isFetching && !preflight.data ? (
          <p className="text-xs text-[var(--lm-text-muted)]">Checking for conflicts…</p>
        ) : preflight.data ? (
          <div className="space-y-3">
            <div className="flex flex-wrap items-center gap-4 text-xs text-[var(--lm-text-muted)]">
              <span>
                Server estimate:{' '}
                <span className="lm-numeric text-[var(--lm-text)]">
                  {preflight.data.points_total} point{preflight.data.points_total === 1 ? '' : 's'}
                </span>
              </span>
              <span>
                Duration:{' '}
                <span className="text-[var(--lm-text)]">
                  {formatEstimate(preflight.data.estimated_sec / 60)}
                </span>
                {preflight.data.estimate_from_history ? '' : ' (no history yet — a rough guess)'}
              </span>
              {!preflight.data.runtime_ready ? (
                <span className="text-[var(--lm-danger)]">
                  No active llama.cpp build to benchmark with.
                </span>
              ) : null}
            </div>

            {preflight.data.ignored_flags.length > 0 ? (
              <div className="flex items-start gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-raised)] p-3 text-xs text-[var(--lm-text-muted)]">
                <Info aria-hidden className="mt-0.5 size-4 shrink-0 text-[var(--lm-text-faint)]" />
                <div className="space-y-1">
                  <p className="font-medium text-[var(--lm-text)]">
                    llama-bench ignores {preflight.data.ignored_flags.length} field
                    {preflight.data.ignored_flags.length === 1 ? '' : 's'} from this model's
                    configuration
                  </p>
                  <ul className="list-disc space-y-0.5 pl-4">
                    {preflight.data.ignored_flags.map((flag) => (
                      <li key={flag.field}>
                        <span className="lm-numeric">{flag.field}</span> — {flag.reason}
                      </li>
                    ))}
                  </ul>
                </div>
              </div>
            ) : null}

            {conflicts.length > 0 ? (
              <div className="flex items-start gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)] p-3 text-xs text-[var(--lm-text)]">
                <AlertTriangle
                  aria-hidden
                  className="mt-0.5 size-4 shrink-0 text-[var(--lm-warn)]"
                />
                <div className="space-y-1">
                  <p className="font-medium">
                    {conflicts.length} instance{conflicts.length === 1 ? '' : 's'} share a GPU this
                    benchmark needs
                  </p>
                  <ul className="list-disc space-y-0.5 pl-4">
                    {conflicts.map((conflict) => (
                      <li key={conflict.instance_id}>
                        {conflict.name} ({conflict.state}
                        {conflict.assumed ? ', assumed' : ''})
                      </li>
                    ))}
                  </ul>
                  <FormField label="If a conflict is found" className="mt-2 max-w-xs">
                    {(field) => (
                      <Select
                        {...selectFieldProps(field)}
                        value={onConflict}
                        onValueChange={setOnConflict}
                        options={[
                          {
                            value: 'stop_and_restore',
                            label: 'Stop and restart them automatically',
                          },
                          { value: 'abort', label: 'Refuse to start the benchmark' },
                        ]}
                      />
                    )}
                  </FormField>
                </div>
              </div>
            ) : null}
          </div>
        ) : preflight.isError ? (
          <p className="text-xs text-[var(--lm-danger)]">{describeError(preflight.error).title}</p>
        ) : null}
      </Panel>

      <div className="flex items-center justify-end gap-2">
        <Button
          variant="secondary"
          disabled={!modelId || busy}
          loading={create.isPending && pendingDraft === false ? false : create.isPending}
          onClick={() => void submit(true)}
        >
          Save as draft
        </Button>
        <Button variant="primary" disabled={!modelId || busy} loading={busy} onClick={onRunNow}>
          Run now
        </Button>
      </div>

      {models.data && models.data.items.length === 0 ? (
        <EmptyState
          title="No models are ready to benchmark yet"
          description="Download a model first, then come back here."
          tone="neutral"
        />
      ) : null}

      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={
          onConflict === 'abort'
            ? 'This benchmark will be refused'
            : 'This will stop running instances'
        }
        tone={onConflict === 'abort' ? 'primary' : 'danger'}
        confirmLabel={onConflict === 'abort' ? 'Try anyway' : 'Stop instances and run'}
        consequences={
          <div className="space-y-1">
            <p>
              {onConflict === 'abort'
                ? 'The following instances occupy the GPU this benchmark needs, and the benchmark will be refused until they are stopped:'
                : 'The following instances will be stopped for the duration of the benchmark and restarted automatically when it finishes:'}
            </p>
            <ul className="list-disc space-y-0.5 pl-4">
              {conflicts.map((conflict) => (
                <li key={conflict.instance_id}>
                  {conflict.name} ({conflict.state})
                </li>
              ))}
            </ul>
          </div>
        }
        busy={busy}
        onConfirm={() => {
          setConfirmOpen(false);
          void submit(pendingDraft);
        }}
      />
    </div>
  );
}
