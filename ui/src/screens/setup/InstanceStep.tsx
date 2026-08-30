import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { AlertTriangle, Plus, Sparkles } from 'lucide-react';
import {
  Badge,
  Button,
  EmptyState,
  Field,
  FormField,
  Input,
  PanelHeader,
  Select,
  StatusBadge,
  Switch,
  toast,
} from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type { FitReport, FlagSet, Instance, Model } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { WizardStep } from '../../setup/WizardStep';
import { fieldProps } from './fieldProps';
import {
  INSTANCE_NAME_HELP,
  isValidInstanceName,
  suggestInstanceName,
  uniqueInstanceName,
} from './instanceName';
import { useWizardScratch } from './scratch';

/**
 * Wizard step `instance` (DESIGN section 11.2): "prefilled from the chosen model with flags
 * recommended by the fit calculator, a port suggestion, and an autostart toggle".
 *
 * Two prefills, and they come from different places on purpose:
 *
 *  - **The name** is derived from the model here, because there is no server-side answer to "what
 *    should this be called" and D11's grammar is narrow enough that a free-text field alone is a
 *    trap. It is editable, and validated against the same pattern the unit name has to satisfy.
 *  - **The flags** are the fit calculator's, not this screen's. `POST /fit/estimate` returns a
 *    `recommendation` (section 3.9) computed from this host's real VRAM and this model's real
 *    header; guessing `-ngl 99` locally would be a worse answer arrived at more confidently.
 *
 * The ports are deliberately *not* prefilled: `POST /instances` allocates both when they are
 * omitted (section 3.10), which is a live check against every other holder on the host rather than
 * a guess this form could make. What the response assigns is shown once it exists.
 *
 * The step is skippable — "an instance can be created from the dashboard afterwards" — so nothing
 * here is a gate; it is an offer with the work already done.
 */
export function InstanceStep() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const scratch = useWizardScratch();

  const [name, setName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);
  const [ctxSize, setCtxSize] = useState(8192);
  const [parallel, setParallel] = useState(1);
  const [autostart, setAutostart] = useState(true);
  const [useRecommendation, setUseRecommendation] = useState(true);
  const [created, setCreated] = useState<Instance | null>(null);

  const models = useQuery({
    queryKey: queryKeys.models.list({ state: 'ready', kind: 'text' }),
    queryFn: () => api.get('/api/v1/models', { query: { state: 'ready', kind: 'text' } }),
  });

  const instances = useQuery({
    queryKey: queryKeys.instances.list(),
    queryFn: () => api.get('/api/v1/instances'),
  });

  const available: readonly Model[] = models.data?.items ?? [];
  const existing: readonly Instance[] = instances.data?.items ?? [];

  const modelId = scratch.modelId ?? available[0]?.id ?? '';
  const model = available.find((row) => row.id === modelId);

  // The fit report for exactly the shape being asked for. It re-runs when the context or the slot
  // count changes, which is the "live" part of section 11.2's promise.
  const fit = useQuery({
    queryKey: queryKeys.list('fit', { model_id: modelId, ctx: ctxSize, parallel }),
    queryFn: () =>
      api.post('/api/v1/fit/estimate', {
        body: {
          source: { model_id: modelId },
          flags: { ctx_size: ctxSize, parallel },
        },
      }),
    enabled: modelId !== '',
    retry: false,
  });

  const report: FitReport | undefined = fit.data;

  // Keep the suggestion in step with the model until the user takes the field over.
  useEffect(() => {
    if (nameTouched || !model) return;
    setName(
      uniqueInstanceName(
        suggestInstanceName(model.repo_id, model.quant_label),
        existing.map((row) => row.name),
      ),
    );
  }, [model, nameTouched, existing]);

  const modelOptions = useMemo(
    () =>
      available.map((row) => ({
        value: row.id,
        label: row.repo_id,
        description: [row.quant_label, formatBytes(row.total_bytes)].filter(Boolean).join(' · '),
      })),
    [available],
  );

  const flags: FlagSet = useMemo(() => {
    const base: FlagSet = { ctx_size: ctxSize, parallel };
    const recommendation = report?.recommendation;
    if (!useRecommendation || !recommendation) return base;
    return {
      ...base,
      n_gpu_layers: { mode: 'count', count: recommendation.n_gpu_layers },
      flash_attn: recommendation.flash_attn ? 'on' : 'off',
      ...(recommendation.type_k ? { cache_type_k: recommendation.type_k } : {}),
      ...(recommendation.type_v ? { cache_type_v: recommendation.type_v } : {}),
    };
  }, [ctxSize, parallel, report, useRecommendation]);

  const create = useMutation({
    mutationFn: () =>
      api.post('/api/v1/instances', {
        body: { name, model_id: modelId, flags, autostart },
      }),
    onSuccess: async (result) => {
      setCreated(result?.instance ?? null);
      for (const warning of result?.warnings ?? []) toast.warn(warning.message);
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('instances') });
    },
    onError: (error) => toast.error(error),
  });

  const nameError =
    name.length > 0 && !isValidInstanceName(name)
      ? INSTANCE_NAME_HELP
      : create.error instanceof ApiError && create.error.code === 'instance_name_taken'
        ? 'An instance already has that name.'
        : undefined;

  const ready = modelId !== '' && isValidInstanceName(name) && !created;

  if (created) {
    return (
      <WizardStep step="instance" canContinue continueLabel="Continue">
        <div className="space-y-3">
          <PanelHeader
            title={<span className="lm-numeric">{created.name}</span>}
            description="Created. It is a systemd unit of its own, so restarting or updating Llama Man never interrupts it."
            actions={<StatusBadge kind="instance" state={created.status.state} />}
          />
          <dl className="grid gap-3 sm:grid-cols-4">
            <Field label="Public port" mono>
              {created.public_port}
            </Field>
            <Field label="Internal port" mono>
              {created.internal_port}
            </Field>
            <Field label="Autostart" mono>
              {created.autostart ? 'on' : 'off'}
            </Field>
            <Field label="Unit" mono>
              {created.unit_name}
            </Field>
          </dl>
          <p className="text-sm text-[var(--lm-text-muted)]">
            Starting it, editing its forty-odd flags and watching its journal are all on the
            instance screen. Nothing about it is fixed by having been made here.
          </p>
        </div>
      </WizardStep>
    );
  }

  if (!models.isPending && available.length === 0) {
    return (
      <WizardStep step="instance">
        <EmptyState
          dense
          title="No model to serve yet"
          description="An instance needs a local model. Go back a step to download one, or skip this — instances are created from the dashboard just as well."
          action={
            <Button onClick={() => void navigate({ to: '/setup/models' })}>Back to models</Button>
          }
        />
      </WizardStep>
    );
  }

  return (
    <WizardStep
      step="instance"
      primaryAction={
        <Button
          variant="primary"
          icon={<Plus />}
          loading={create.isPending}
          disabled={!ready}
          onClick={() => create.mutate()}
        >
          Create instance
        </Button>
      }
      banner={
        report ? (
          <div className="flex flex-wrap items-center gap-2 rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)] px-3 py-2">
            <StatusBadge kind="fit" state={report.verdict} />
            <span className="text-sm text-[var(--lm-text-muted)]">
              {formatBytes(report.required_vram_bytes)} of VRAM at {formatCount(ctxSize)} context
              {report.per_slot_ctx ? ` (${formatCount(report.per_slot_ctx)} per slot)` : ''}
            </span>
            <Badge tone="neutral">{report.confidence}</Badge>
            {report.spill_to_ram_bytes > 0 ? (
              <Badge tone="warn">{formatBytes(report.spill_to_ram_bytes)} spills to RAM</Badge>
            ) : null}
          </div>
        ) : null
      }
    >
      <form
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (ready && !create.isPending) create.mutate();
        }}
      >
        <FormField label="Model" required>
          {(field) => (
            <Select
              {...fieldProps(field)}
              value={modelId}
              onValueChange={(next) => {
                scratch.setModelId(next);
                setNameTouched(false);
              }}
              options={modelOptions}
              placeholder={models.isPending ? 'Reading the library…' : 'Choose a model'}
            />
          )}
        </FormField>

        <FormField
          label="Instance name"
          required
          error={nameError}
          hint={`Suggested from the model. ${INSTANCE_NAME_HELP}`}
        >
          {(field) => (
            <Input
              {...field}
              mono
              value={name}
              onChange={(event) => {
                setNameTouched(true);
                setName(event.target.value);
              }}
            />
          )}
        </FormField>

        <div className="grid gap-4 sm:grid-cols-2">
          <FormField
            label="Context size"
            flag="--ctx-size"
            hint="Shared across slots. The fit report above re-runs as this changes."
          >
            {(field) => (
              <Input
                {...field}
                mono
                type="number"
                min={512}
                step={512}
                value={ctxSize}
                onChange={(event) => setCtxSize(Number(event.target.value) || 0)}
              />
            )}
          </FormField>

          <FormField label="Parallel slots" flag="--parallel" hint="Concurrent requests served.">
            {(field) => (
              <Input
                {...field}
                mono
                type="number"
                min={1}
                max={64}
                value={parallel}
                onChange={(event) => setParallel(Number(event.target.value) || 1)}
              />
            )}
          </FormField>
        </div>

        <FormField
          label="Use the recommended flags"
          hint={
            report?.recommendation?.reason ??
            'Offload, flash attention and KV cache types chosen by the fit calculator for this host.'
          }
        >
          {(field) => (
            <div className="flex h-8 items-center gap-2">
              <Switch
                id={field.id}
                aria-describedby={field['aria-describedby']}
                checked={useRecommendation}
                onCheckedChange={setUseRecommendation}
              />
              <span className="lm-numeric text-xs text-[var(--lm-text-muted)]">
                {report?.recommendation
                  ? `-ngl ${report.recommendation.n_gpu_layers} · -fa ${report.recommendation.flash_attn ? 'on' : 'off'} · KV ${report.recommendation.type_k}/${report.recommendation.type_v}`
                  : 'Waiting for the fit report…'}
              </span>
            </div>
          )}
        </FormField>

        <FormField
          label="Start at boot"
          hint="Enables the unit so this instance comes back after a reboot. Changed any time from the instances table."
        >
          {(field) => (
            <div className="flex h-8 items-center gap-2">
              <Switch
                id={field.id}
                aria-describedby={field['aria-describedby']}
                checked={autostart}
                onCheckedChange={setAutostart}
              />
              <span className="text-sm text-[var(--lm-text-muted)]">
                {autostart ? 'Autostart on' : 'Autostart off'}
              </span>
            </div>
          )}
        </FormField>

        {report?.notes?.length ? (
          <ul className="space-y-1">
            {report.notes.map((note: string) => (
              <li key={note} className="flex items-start gap-2 text-xs text-[var(--lm-text-muted)]">
                <AlertTriangle
                  aria-hidden
                  className="mt-0.5 size-3 shrink-0 text-[var(--lm-warn)]"
                />
                {note}
              </li>
            ))}
          </ul>
        ) : null}

        {fit.isError ? (
          <p className="flex items-center gap-2 text-xs text-[var(--lm-text-faint)]">
            <Sparkles aria-hidden className="size-3 shrink-0" />
            The fit calculator could not read this model&apos;s header. The instance can still be
            created; llama.cpp will decide the offload itself.
          </p>
        ) : null}
      </form>
    </WizardStep>
  );
}
