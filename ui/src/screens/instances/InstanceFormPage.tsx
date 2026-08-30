/**
 * The shared body of `/instances/new` and `/instances/:id/edit` (DESIGN section 4, screen 5).
 *
 * Both routes are the same form; what differs is where the default values come from and which verb
 * saves them. This component owns the *data* — models, port claims, the live fit estimate, the argv
 * dry run, presets — and `<InstanceForm>` owns the fields, which is what keeps the form itself
 * mountable in a test with fixtures.
 *
 * Two things are worth naming here because they are design decisions rather than plumbing:
 *
 *  - **The device roster comes from the fit report.** `per_gpu` carries each device's uuid, index,
 *    name and free bytes (section 3.9), and the widest report seen is remembered — so selecting a
 *    subset narrows the estimate without emptying the picker.
 *  - **The management port is read from the browser's own location.** `ui.port_desired` is a
 *    setting this screen cannot see, but the SPA is served *by* the management listener, so the
 *    port in the address bar is the one section 2.8 reserves. It is a hint either way: the daemon
 *    re-runs the whole port table at save time.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { toast } from '../../components';
import { ApiError } from '../../api/errors';
import type { Instance } from '../../api/types';
import {
  isRouteMissing,
  useCreateInstance,
  useDebouncedValue,
  useFitEstimate,
  useInstances,
  useModels,
  usePatchInstance,
  usePresets,
  useSavePreset,
  useSuggestedPort,
  useValidateInstance,
} from '../../features/instances/api';
import { asDraftValidation } from '../../features/instances/types';
import { InstanceForm } from '../../features/instances/components/InstanceForm';
import type { FormPane } from '../../features/instances/components/InstanceForm';
import type { PickableDevice } from '../../features/instances/components/DevicePicker';
import { portClaims, suggestPort } from '../../features/instances/ports';
import { DEFAULT_FORM_CONTEXT } from '../../features/instances/schema';
import type { FormContext } from '../../features/instances/schema';
import {
  createBody,
  emptyFormValues,
  flagsFromValues,
  patchBody,
  valuesFromInstance,
} from '../../features/instances/values';
import type { InstanceFormValues } from '../../features/instances/values';

export interface InstanceFormPageProps {
  mode: 'create' | 'edit';
  /** The row being edited. Absent on create. */
  instance?: Instance | undefined;
  /** Prefill from the browse screen's "create an instance for this model" path. */
  initialModelId?: string | undefined;
  pane: FormPane;
  onPaneChange: (pane: FormPane) => void;
}

/** The management listener's port, which section 2.8 reserves against public ports. */
function managementPorts(): number[] {
  const ports = new Set<number>([5526]);
  if (typeof window !== 'undefined' && window.location.port) {
    const port = Number(window.location.port);
    if (Number.isInteger(port)) ports.add(port);
  }
  return [...ports];
}

export function InstanceFormPage({
  mode,
  instance,
  initialModelId,
  pane,
  onPaneChange,
}: InstanceFormPageProps) {
  const navigate = useNavigate();
  const models = useModels();
  const instances = useInstances();
  const presets = usePresets();
  const savePreset = useSavePreset();
  const create = useCreateInstance();
  const patch = usePatchInstance(instance?.id ?? '');

  const defaultValues = useMemo<InstanceFormValues>(() => {
    if (instance) return valuesFromInstance(instance);
    const base = emptyFormValues();
    return initialModelId ? { ...base, model_id: initialModelId } : base;
    // A saved row only reseeds the form when the identity of the row changes; a live SSE patch to
    // its status must not throw away what the user has typed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [instance?.id, initialModelId]);

  const [values, setValues] = useState<InstanceFormValues>(defaultValues);
  const debounced = useDebouncedValue(values, 400);

  const formContext = useMemo<FormContext>(
    () => ({
      ...DEFAULT_FORM_CONTEXT,
      claims: portClaims(instances.data ?? []),
      managementPorts: managementPorts(),
      ...(instance ? { excludeInstanceId: instance.id } : {}),
    }),
    [instances.data, instance],
  );

  /* -- the live estimate ---------------------------------------------------- */

  const fitInput = useMemo(
    () =>
      debounced.model_id === ''
        ? null
        : {
            model_id: debounced.model_id,
            flags: flagsFromValues(debounced) as Record<string, unknown>,
            ...(debounced.device_uuids.length > 0 ? { gpus: debounced.device_uuids } : {}),
          },
    [debounced],
  );
  const fit = useFitEstimate(fitInput);

  // The widest device list any report has shown. Narrowing the selection narrows `per_gpu`, and a
  // picker that lost the device you just deselected would be a trap.
  const [devices, setDevices] = useState<PickableDevice[]>([]);
  const reportedDevices = fit.data?.per_gpu;
  useEffect(() => {
    if (!reportedDevices) return;
    setDevices((known) => {
      const merged = new Map(known.map((device) => [device.uuid, device]));
      for (const gpu of reportedDevices) {
        merged.set(gpu.uuid, {
          uuid: gpu.uuid,
          index: gpu.index,
          name: gpu.name,
          freeBytes: gpu.free_bytes ?? null,
          totalBytes: gpu.total_bytes ?? null,
        });
      }
      const next = [...merged.values()].sort((a, b) => a.index - b.index);
      // Identity is what stops this from re-rendering the form on every estimate.
      return next.length === known.length &&
        next.every(
          (device, i) => device.uuid === known[i]?.uuid && device.freeBytes === known[i]?.freeBytes,
        )
        ? known
        : next;
    });
  }, [reportedDevices]);

  /* -- the argv dry run ------------------------------------------------------ */

  // `POST /instances/validate` is a dry run of a FlagSet (section 3.10a): it renders argv, checks
  // conflicts and answers the three-valued draft check, and never a 422. The create shape is the
  // one it takes in both modes — an edit is the same configuration, not a different document.
  const validateBody = useMemo(
    () => (debounced.model_id === '' ? null : createBody(debounced)),
    [debounced],
  );
  const validation = useValidateInstance(validateBody);

  /* -- port suggestions ------------------------------------------------------ */

  const wantsPublic = mode === 'create' && values.public_port.trim() === '';
  const wantsInternal = mode === 'create' && values.internal_port.trim() === '';
  const publicSuggestion = useSuggestedPort('public', wantsPublic);
  const internalSuggestion = useSuggestedPort('internal', wantsInternal);

  const suggestion = (
    kind: 'public' | 'internal',
    served: number | undefined,
    wanted: boolean,
  ): number | undefined => {
    if (!wanted) return undefined;
    if (served !== undefined) return served;
    return suggestPort(kind, formContext) ?? undefined;
  };

  /* -- saving ---------------------------------------------------------------- */

  const submit = useCallback(
    (submitted: InstanceFormValues) => {
      const done = (created: Instance, warnings: { code: string; message: string }[]) => {
        for (const warning of warnings) {
          toast.warn(warning.message, { duration: null });
        }
        toast.success(mode === 'create' ? `${created.name} created.` : 'Configuration saved.');
        void navigate({ to: '/instances/$id', params: { id: created.id } });
      };

      if (mode === 'create') {
        create.mutate(createBody(submitted), {
          onSuccess: (result) => done(result.instance, result.warnings ?? []),
          onError: (error) => {
            if (!(error instanceof ApiError)) toast.error(error);
          },
        });
        return;
      }
      if (!instance) return;
      patch.mutate(patchBody(submitted, instance.generation), {
        onSuccess: (result) => done(result.instance, result.warnings ?? []),
        onError: (error) => {
          if (!(error instanceof ApiError)) toast.error(error);
        },
      });
    },
    [create, patch, instance, mode, navigate],
  );

  const saving = create.isPending || patch.isPending;
  const submitError = mode === 'create' ? create.error : patch.error;

  const argvUnavailable =
    validation.error && isRouteMissing(validation.error)
      ? 'This daemon does not serve /instances/validate yet, so the command line appears once the instance is saved.'
      : validation.error instanceof ApiError
        ? validation.error.message
        : undefined;

  return (
    <InstanceForm
      mode={mode}
      defaultValues={defaultValues}
      models={models.data ?? []}
      devices={devices}
      formContext={formContext}
      pane={pane}
      onPaneChange={onPaneChange}
      onValuesChange={setValues}
      onSubmit={submit}
      onCancel={() =>
        void navigate(
          instance ? { to: '/instances/$id', params: { id: instance.id } } : { to: '/instances' },
        )
      }
      submitting={saving}
      submitError={submitError}
      fitReport={fit.data}
      fitLoading={fit.isFetching}
      fitError={fit.error}
      argv={validation.data?.argv ?? undefined}
      argvLoading={validation.isFetching}
      argvUnavailable={argvUnavailable}
      unknownFlags={validation.data?.unknown_flags ?? []}
      draftValidation={asDraftValidation(validation.data?.draft_validation)}
      presets={presets.data ?? []}
      presetsUnavailable={Boolean(presets.error)}
      savingPreset={savePreset.isPending}
      onSavePreset={({ name, description }) =>
        savePreset.mutate(
          {
            name,
            ...(description ? { description } : {}),
            flags: flagsFromValues(values),
            extra_flags: values.extra_flags,
          },
          {
            onSuccess: () => toast.success(`Preset “${name}” saved.`),
            onError: (error) =>
              isRouteMissing(error)
                ? toast.warn('This daemon does not serve /presets yet.')
                : toast.error(error),
          },
        )
      }
      portSuggestions={{
        public: suggestion('public', publicSuggestion.data?.port, wantsPublic),
        internal: suggestion('internal', internalSuggestion.data?.port, wantsInternal),
      }}
    />
  );
}
