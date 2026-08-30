/**
 * The instance form — DESIGN section 4, screen 5.
 *
 * "Three-pane form (**Model & context** / **Performance** / **Advanced**) with a live **fit panel**
 * that re-estimates on every change and a rendered-argv preview underneath."
 *
 * The panes are a URL search param, not local state, because a form this size is something people
 * send each other a link into. Everything else about the shape follows from two design facts:
 *
 *  - **Every flag is optional and null ≠ zero.** So no field is pre-filled with a llama.cpp default
 *    it would then start passing explicitly; blank means "do not pass it", and the tri-states say
 *    that out loud for the booleans.
 *  - **The daemon is the authority.** Validation mirrors the server's rules (`schema.ts`) so the
 *    answer arrives early, the argv preview is the *server's* rendering rather than a second
 *    implementation of `RenderArgv`, and a refusal comes back to the field that caused it.
 *
 * The component is presentational on purpose — data in, values out — so the screens own the
 * queries and the tests can mount it with fixtures.
 */

import { useEffect, useMemo } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import type { Resolver } from 'react-hook-form';
import { AlertTriangle, Save, X } from 'lucide-react';
import {
  Badge,
  Button,
  FieldGroup,
  FormField,
  Panel,
  Switch,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from '../../../components';
import type { FitRecommendation, FitReport, Model } from '../../../api/types';
import { formAdvisories } from '../advisories';
import { draftChoices, findModel, mmprojChoices, primaryModelChoices } from '../modelChoices';
import { createInstanceFormSchema } from '../schema';
import type { FormContext } from '../schema';
import { refusalReport } from '../serverErrors';
import type { FlagPreset } from '../types';
import { applyFlagsToValues, CACHE_TYPES, resolveDeviceFilter } from '../values';
import type { InstanceFormValues } from '../values';
import { AdvisoryList } from './AdvisoryList';
import { ArgvPreview } from './ArgvPreview';
import { DevicePicker } from './DevicePicker';
import type { PickableDevice } from './DevicePicker';
import { ExtraFlagsField } from './ExtraFlagsField';
import { SelectField, TextAreaField, TextField, TriField } from './fields';
import { FitPanel } from './FitPanel';
import { NglField } from './NglField';
import { PortField } from './PortField';
import { PresetBar } from './PresetBar';

export type FormPane = 'model' | 'performance' | 'advanced';

export interface InstanceFormProps {
  mode: 'create' | 'edit';
  defaultValues: InstanceFormValues;
  models: readonly Model[];
  devices: readonly PickableDevice[];
  formContext: FormContext;
  pane: FormPane;
  onPaneChange: (pane: FormPane) => void;
  onSubmit: (values: InstanceFormValues) => void;
  onCancel: () => void;
  onValuesChange?: (values: InstanceFormValues) => void;
  submitting?: boolean;
  submitError?: unknown;
  /** The live estimate beside the form. */
  fitReport?: FitReport | undefined;
  fitLoading?: boolean;
  fitError?: unknown;
  /** `llamacpp_versions.supports_fit` for the active build (section 5.7's `-ngl auto` rule). */
  supportsFit?: boolean | undefined;
  argv?: readonly string[] | undefined;
  argvLoading?: boolean;
  argvUnavailable?: string | undefined;
  unknownFlags?: readonly string[];
  /** Section 3.10a's three-valued answer for the current pairing. */
  draftValidation?: 'ok' | 'deferred' | 'mismatch' | undefined;
  presets?: readonly FlagPreset[];
  presetsUnavailable?: boolean;
  onSavePreset?: (input: { name: string; description: string }) => void;
  savingPreset?: boolean;
  portSuggestions?: { public?: number | undefined; internal?: number | undefined };
}

const PANES: { id: FormPane; label: string }[] = [
  { id: 'model', label: 'Model & context' },
  { id: 'performance', label: 'Performance' },
  { id: 'advanced', label: 'Advanced' },
];

const cacheTypeOptions = [
  { value: '', label: 'Leave to llama.cpp' },
  ...CACHE_TYPES.map((type) => ({ value: type, label: type })),
];

export function InstanceForm({
  mode,
  defaultValues,
  models,
  devices,
  formContext,
  pane,
  onPaneChange,
  onSubmit,
  onCancel,
  onValuesChange,
  submitting = false,
  submitError,
  fitReport,
  fitLoading = false,
  fitError,
  supportsFit,
  argv,
  argvLoading = false,
  argvUnavailable,
  unknownFlags = [],
  draftValidation,
  presets = [],
  presetsUnavailable = false,
  onSavePreset,
  savingPreset = false,
  portSuggestions,
}: InstanceFormProps) {
  const schema = useMemo(() => createInstanceFormSchema(formContext), [formContext]);
  const form = useForm<InstanceFormValues>({
    defaultValues,
    // One documented cast: the schema validates the string-shaped value model in place, so its
    // input and output are the same type — which the resolver's four overloads cannot infer through
    // a `superRefine` chain.
    resolver: zodResolver(schema) as unknown as Resolver<InstanceFormValues>,
    mode: 'onBlur',
  });

  const values = form.watch();

  // The screen owns the fit estimate and the argv dry run; it needs the values to ask for them.
  useEffect(() => {
    onValuesChange?.(values);
    // `values` is a fresh object each render; the JSON is what actually changed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(values)]);

  // A refusal from the daemon lands on the field that caused it (section 3.10's error tables).
  const refusal = useMemo(
    () =>
      refusalReport(submitError, {
        min: formContext.internalPortMin,
        max: formContext.internalPortMax,
      }),
    [submitError, formContext.internalPortMin, formContext.internalPortMax],
  );
  useEffect(() => {
    for (const { field, message } of refusal.fields) {
      form.setError(field, { type: 'server', message });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refusal]);

  const primary = findModel(models, values.model_id);
  const advisories = formAdvisories(values, {
    deviceCount: values.device_uuids.length,
    hasDraftModel: values.draft_model_id !== '',
    ...(supportsFit === undefined ? {} : { supportsFit }),
    ...(primary === undefined ? {} : { modelHasVision: primary.has_vision }),
    unknownFlags,
  });

  const applyRecommendation = (recommendation: FitRecommendation) => {
    form.setValue('ngl_mode', 'count', { shouldDirty: true });
    form.setValue('ngl_count', String(recommendation.n_gpu_layers), { shouldDirty: true });
    form.setValue('flash_attn', recommendation.flash_attn ? 'on' : 'off', { shouldDirty: true });
    form.setValue('cache_type_k', recommendation.type_k, { shouldDirty: true });
    form.setValue('cache_type_v', recommendation.type_v, { shouldDirty: true });
  };

  const applyPreset = (preset: FlagPreset) => {
    form.reset(applyFlagsToValues(form.getValues(), preset.flags, preset.extra_flags), {
      keepDefaultValues: true,
    });
  };

  const setDevices = (uuids: string[]) => {
    form.setValue('device_uuids', uuids, { shouldDirty: true });
    form.setValue(
      'device_filter',
      resolveDeviceFilter(
        uuids,
        devices.map((device) => ({ uuid: device.uuid, index: device.index })),
      ),
      { shouldDirty: true },
    );
  };

  return (
    <form
      className="space-y-4"
      onSubmit={form.handleSubmit((submitted) => onSubmit(submitted))}
      noValidate
    >
      {refusal.banner ? (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-danger)]/40 bg-[var(--lm-danger-soft)] px-3 py-2 text-xs text-[var(--lm-text)]"
        >
          <AlertTriangle aria-hidden className="mt-0.5 size-3.5 shrink-0 text-[var(--lm-danger)]" />
          <span>{refusal.banner}</span>
        </div>
      ) : null}

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_22rem]">
        <div className="min-w-0 space-y-4">
          <Panel>
            <Tabs value={pane} onValueChange={(next) => onPaneChange(next as FormPane)}>
              <div className="flex flex-wrap items-center justify-between gap-3">
                <TabsList>
                  {PANES.map((entry) => (
                    <TabsTrigger key={entry.id} value={entry.id}>
                      {entry.label}
                    </TabsTrigger>
                  ))}
                </TabsList>
                {onSavePreset ? (
                  <PresetBar
                    presets={presets}
                    unavailable={presetsUnavailable}
                    onApply={applyPreset}
                    onSave={onSavePreset}
                    saving={savingPreset}
                  />
                ) : null}
              </div>

              {/* ---------------------------------------------- Model & context */}
              <TabsContent value="model" className="space-y-6">
                <FieldGroup
                  title="Identity"
                  description="The name becomes this instance’s systemd unit id, so it is lowercase, digits and hyphens only."
                >
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="name"
                      label="Name"
                      required
                      mono
                      placeholder="qwen3-8b"
                      hint="llamaman-instance@<name>.service"
                    />
                    <TextField
                      form={form}
                      name="display_name"
                      label="Display name"
                      placeholder="Qwen3 8B, long context"
                    />
                  </div>
                  <TextAreaField
                    form={form}
                    name="description"
                    label="Description"
                    rows={2}
                    placeholder="What this instance is for."
                  />
                </FieldGroup>

                <FieldGroup title="Models">
                  <SelectField
                    form={form}
                    name="model_id"
                    label="Model"
                    required
                    flag="-m"
                    options={primaryModelChoices(models)}
                    placeholder={models.length === 0 ? 'No local models yet' : 'Choose a model…'}
                    hint="A model that is still downloading can be chosen — the instance is configured while it lands."
                  />
                  <SelectField
                    form={form}
                    name="mmproj_model_id"
                    label="Vision projector"
                    flag="--mmproj"
                    options={mmprojChoices(models, primary)}
                    placeholder="None"
                  />
                  <div className="space-y-2">
                    <SelectField
                      form={form}
                      name="draft_model_id"
                      label="Draft model"
                      flag="-md"
                      options={draftChoices(models, primary)}
                      placeholder="None"
                      hint="Speculative decoding. The draft model must share the primary model’s tokenizer."
                    />
                    {draftValidation === 'deferred' && values.draft_model_id !== '' ? (
                      <Badge tone="warn">
                        Vocabulary check deferred until both GGUF headers are read
                      </Badge>
                    ) : null}
                    {draftValidation === 'mismatch' ? (
                      <Badge tone="danger">
                        Vocabularies differ — this instance would refuse to start
                      </Badge>
                    ) : null}
                  </div>
                </FieldGroup>

                <FieldGroup
                  title="Context"
                  description="-c is the TOTAL context, shared across the parallel slots."
                >
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="ctx_size"
                      label="Context size"
                      flag="-c"
                      mono
                      numeric
                      placeholder="8192"
                      suffix="tokens"
                    />
                    <TextField
                      form={form}
                      name="parallel"
                      label="Parallel slots"
                      flag="-np"
                      mono
                      numeric
                      placeholder="4"
                    />
                  </div>
                  <TextField
                    form={form}
                    name="alias"
                    label="Served model name"
                    flag="--alias"
                    placeholder="qwen3-8b"
                    hint="What the OpenAI-compatible API reports as the model id."
                  />
                </FieldGroup>

                <FieldGroup
                  title="Network and lifecycle"
                  description="The public port is the gateway's; the internal one is llama-server's, on loopback."
                >
                  <div className="grid gap-4 sm:grid-cols-2">
                    <PortField
                      form={form}
                      name="public_port"
                      kind="public"
                      label="Public port"
                      ctx={formContext}
                      suggestion={portSuggestions?.public}
                    />
                    <PortField
                      form={form}
                      name="internal_port"
                      kind="internal"
                      label="Internal port"
                      ctx={formContext}
                      suggestion={portSuggestions?.internal}
                    />
                  </div>

                  <div className="grid gap-4 sm:grid-cols-2">
                    <SelectField
                      form={form}
                      name="auth_mode"
                      label="Authentication"
                      options={[
                        {
                          value: 'token',
                          label: 'API token required',
                          description: 'The gateway checks a token’s scope before proxying.',
                        },
                        {
                          value: 'none',
                          label: 'Open',
                          description: 'Anyone who can reach the port can use it. Still counted.',
                        },
                      ]}
                    />
                    <SelectField
                      form={form}
                      name="restart_policy"
                      label="Restart policy"
                      options={[
                        { value: 'always', label: 'Always' },
                        {
                          value: 'on-failure',
                          label: 'On failure',
                          description: 'A clean exit is not a failure, and we say so.',
                        },
                        { value: 'never', label: 'Never' },
                      ]}
                    />
                  </div>

                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="restart_max"
                      label="Restart budget"
                      mono
                      numeric
                      placeholder="5"
                      hint="Failed starts allowed inside the window before the state latches to crash-looping."
                    />
                    <TextField
                      form={form}
                      name="restart_window_sec"
                      label="Restart window"
                      mono
                      numeric
                      placeholder="600"
                      suffix="sec"
                    />
                  </div>

                  {mode === 'create' ? (
                    <FormField
                      label="Start at boot"
                      hint="Enables the unit. It does not start the instance now."
                      inline
                    >
                      {(field) => (
                        <Switch
                          id={field.id}
                          checked={values.autostart}
                          onCheckedChange={(checked) =>
                            form.setValue('autostart', checked, { shouldDirty: true })
                          }
                        />
                      )}
                    </FormField>
                  ) : (
                    <p className="text-xs text-[var(--lm-text-faint)]">
                      Autostart is toggled from the instance itself — it enables or disables the
                      unit and never starts or stops anything.
                    </p>
                  )}
                </FieldGroup>
              </TabsContent>

              {/* ------------------------------------------------- Performance */}
              <TabsContent value="performance" className="space-y-6">
                <FieldGroup title="Offload">
                  <NglField
                    form={form}
                    report={fitReport}
                    supportsFit={supportsFit}
                    onPin={(count) => {
                      form.setValue('ngl_mode', 'count', { shouldDirty: true });
                      form.setValue('ngl_count', String(count), { shouldDirty: true });
                    }}
                  />
                  <DevicePicker
                    devices={devices}
                    selected={values.device_uuids}
                    onChange={setDevices}
                    resolved={values.device_filter}
                  />
                  <div className="grid gap-4 sm:grid-cols-2">
                    <SelectField
                      form={form}
                      name="split_mode"
                      label="Split mode"
                      flag="-sm"
                      options={[
                        { value: '', label: 'Leave to llama.cpp' },
                        { value: 'none', label: 'None — one device' },
                        { value: 'layer', label: 'By layer' },
                        { value: 'row', label: 'By row' },
                      ]}
                    />
                    <TextField
                      form={form}
                      name="main_gpu"
                      label="Main device"
                      flag="-mg"
                      mono
                      numeric
                      placeholder="0"
                      hint="An index into the --device list, not into nvidia-smi's."
                    />
                  </div>
                  <TextField
                    form={form}
                    name="tensor_split"
                    label="Tensor split"
                    flag="-ts"
                    mono
                    placeholder="0.5, 0.5"
                    hint="Proportions per device, in --device order. Pinning it disables llama.cpp's --fit."
                  />
                </FieldGroup>

                <FieldGroup title="Batching and threads">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="batch_size"
                      label="Logical batch"
                      flag="-b"
                      mono
                      numeric
                      placeholder="2048"
                    />
                    <TextField
                      form={form}
                      name="ubatch_size"
                      label="Physical batch"
                      flag="-ub"
                      mono
                      numeric
                      placeholder="512"
                    />
                    <TextField
                      form={form}
                      name="threads"
                      label="Threads"
                      flag="-t"
                      mono
                      numeric
                      placeholder="auto"
                    />
                    <TextField
                      form={form}
                      name="threads_batch"
                      label="Threads for batch"
                      flag="-tb"
                      mono
                      numeric
                      placeholder="auto"
                    />
                  </div>
                  <TriField
                    form={form}
                    name="cont_batching"
                    label="Continuous batching"
                    flag="-cb / -nocb"
                    labels={{ on: 'On (-cb)', off: 'Off (-nocb)' }}
                    hint="llama-server defaults this on, so turning it off has to be said out loud."
                  />
                </FieldGroup>

                <FieldGroup title="Attention and KV cache">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <SelectField
                      form={form}
                      name="flash_attn"
                      label="Flash attention"
                      flag="-fa"
                      options={[
                        { value: '', label: 'Leave to llama.cpp' },
                        { value: 'on', label: 'On' },
                        { value: 'off', label: 'Off' },
                        { value: 'auto', label: 'Auto' },
                      ]}
                    />
                    <div />
                    <SelectField
                      form={form}
                      name="cache_type_k"
                      label="K cache type"
                      flag="-ctk"
                      options={cacheTypeOptions}
                      mono
                    />
                    <SelectField
                      form={form}
                      name="cache_type_v"
                      label="V cache type"
                      flag="-ctv"
                      options={cacheTypeOptions}
                      mono
                      hint="Quantizing the V cache needs flash attention on most builds."
                    />
                  </div>
                </FieldGroup>

                <FieldGroup title="Memory">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TriField
                      form={form}
                      name="mlock"
                      label="Lock weights in RAM"
                      flag="--mlock"
                      labels={{ on: 'Locked', off: 'Pageable' }}
                    />
                    <TriField
                      form={form}
                      name="no_mmap"
                      label="Memory mapping"
                      flag="--no-mmap"
                      labels={{ on: 'Disabled (--no-mmap)', off: 'Enabled' }}
                    />
                  </div>
                </FieldGroup>
              </TabsContent>

              {/* ---------------------------------------------------- Advanced */}
              <TabsContent value="advanced" className="space-y-6">
                <FieldGroup title="Server mode">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TriField form={form} name="embedding" label="Embeddings" flag="--embedding" />
                    <TriField form={form} name="rerank" label="Reranking" flag="--reranking" />
                  </div>
                  <TextField
                    form={form}
                    name="pooling"
                    label="Pooling"
                    flag="--pooling"
                    placeholder="mean"
                    hint="Vocabulary belongs to llama.cpp; anything this build accepts is accepted here."
                  />
                </FieldGroup>

                <FieldGroup title="Chat template">
                  <TriField form={form} name="jinja" label="Jinja templating" flag="--jinja" />
                  <TextField
                    form={form}
                    name="chat_template"
                    label="Template name"
                    flag="--chat-template"
                    placeholder="chatml"
                  />
                  <TextField
                    form={form}
                    name="chat_template_file"
                    label="Template file"
                    flag="--chat-template-file"
                    mono
                    placeholder="/var/lib/llamaman/templates/qwen.jinja"
                  />
                </FieldGroup>

                <FieldGroup title="Context handling">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="n_keep"
                      label="Tokens kept"
                      flag="--keep"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="n_predict"
                      label="Prediction limit"
                      flag="--predict"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="defrag_thold"
                      label="Defrag threshold"
                      flag="--defrag-thold"
                      mono
                    />
                    <TextField
                      form={form}
                      name="cache_reuse"
                      label="Cache reuse"
                      flag="--cache-reuse"
                      mono
                      numeric
                    />
                  </div>
                </FieldGroup>

                <FieldGroup title="RoPE and YaRN">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="rope_scaling"
                      label="RoPE scaling"
                      flag="--rope-scaling"
                      placeholder="yarn"
                    />
                    <TextField
                      form={form}
                      name="rope_freq_base"
                      label="RoPE base frequency"
                      flag="--rope-freq-base"
                      mono
                    />
                    <TextField
                      form={form}
                      name="rope_freq_scale"
                      label="RoPE frequency scale"
                      flag="--rope-freq-scale"
                      mono
                    />
                    <TextField
                      form={form}
                      name="yarn_ext_factor"
                      label="YaRN extrapolation"
                      flag="--yarn-ext-factor"
                      mono
                    />
                    <TextField
                      form={form}
                      name="yarn_attn_factor"
                      label="YaRN attention factor"
                      flag="--yarn-attn-factor"
                      mono
                    />
                  </div>
                </FieldGroup>

                <FieldGroup title="Speculative decoding">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField
                      form={form}
                      name="draft_n_max"
                      label="Draft tokens, max"
                      flag="--draft-max"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="draft_n_min"
                      label="Draft tokens, min"
                      flag="--draft-min"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="draft_p_min"
                      label="Acceptance threshold"
                      flag="--draft-p-min"
                      mono
                      placeholder="0.75"
                    />
                    <TextField
                      form={form}
                      name="draft_ctx_size"
                      label="Draft context"
                      flag="-cd"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="draft_n_gpu_layers"
                      label="Draft offload"
                      flag="-ngld"
                      mono
                      numeric
                    />
                  </div>
                </FieldGroup>

                <FieldGroup title="Process and NUMA">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <TextField form={form} name="numa" label="NUMA policy" flag="--numa" />
                    <TextField form={form} name="cpu_mask" label="CPU mask" flag="-C" mono />
                    <TextField
                      form={form}
                      name="prio"
                      label="Scheduling priority"
                      flag="--prio"
                      mono
                      numeric
                    />
                    <TextField
                      form={form}
                      name="log_verbosity"
                      label="Log verbosity"
                      flag="--verbosity"
                      mono
                      numeric
                    />
                  </div>
                  <TextField
                    form={form}
                    name="slot_save_path"
                    label="Slot save path"
                    flag="--slot-save-path"
                    mono
                  />
                </FieldGroup>

                <FieldGroup
                  title="Server endpoints"
                  description="The supervisor and the gateway read these, which is why they are flags rather than constants."
                >
                  <div className="grid gap-4 sm:grid-cols-3">
                    <TriField form={form} name="props_endpoint" label="/props" flag="--props" />
                    <TriField form={form} name="slots_endpoint" label="/slots" flag="--slots" />
                    <TriField
                      form={form}
                      name="metrics_endpoint"
                      label="/metrics"
                      flag="--metrics"
                    />
                  </div>
                </FieldGroup>

                <FieldGroup title="Escape hatch">
                  <ExtraFlagsField form={form} unknownFlags={unknownFlags} />
                </FieldGroup>
              </TabsContent>
            </Tabs>
          </Panel>

          <ArgvPreview
            argv={argv}
            loading={argvLoading}
            unknownFlags={unknownFlags}
            unavailable={argvUnavailable}
            title="Rendered command line"
          />
        </div>

        <aside className="space-y-4">
          <FitPanel
            report={fitReport}
            loading={fitLoading}
            error={fitError}
            {...(fitReport ? { onApplyRecommendation: applyRecommendation } : {})}
          />
          <AdvisoryList advisories={advisories} />
        </aside>
      </div>

      <div className="sticky bottom-0 flex items-center justify-end gap-2 border-t border-[var(--lm-border)] bg-[var(--lm-bg)]/95 py-3">
        <Button variant="ghost" icon={<X />} onClick={onCancel} disabled={submitting}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" icon={<Save />} loading={submitting}>
          {mode === 'create' ? 'Create instance' : 'Save changes'}
        </Button>
      </div>
    </form>
  );
}
