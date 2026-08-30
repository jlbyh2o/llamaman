/**
 * Wizard state, read from the server.
 *
 * "Every step is idempotent and re-enterable… A browser refresh or a daemon restart mid-build does
 * not restart the wizard" (DESIGN section 11.2). That is only true if the client never keeps its
 * own idea of where it is, so it does not: the step rows in `wizard_steps` are the truth, this hook
 * reads them, and `resumePath()` is what the `/setup` index route redirects to.
 */

import { useMemo } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import { queryKeys } from '../api/keys';
import type { WizardStep, WizardStepState } from '../api/types';
import { useSetupState } from '../auth/session';
import { WIZARD_STEP_METAS, WIZARD_STEPS_IN_RAIL } from './steps';
import type { WizardStepMeta } from './steps';

export interface WizardStepView extends WizardStepMeta {
  state: WizardStepState;
  skippable: boolean;
  blocked: boolean;
  /** Reachable now: done, skipped, or the one in progress. */
  reachable: boolean;
}

export interface WizardView {
  steps: WizardStepView[];
  /** The step the server says is active, or the first unfinished one. */
  activeId: string | null;
  /** Where `/setup` should send a returning browser. */
  resumePath: string;
  claimed: boolean;
  complete: boolean;
  tokenRequired: boolean;
  isPending: boolean;
  isError: boolean;
  error: Error | null;
}

const DEFAULT_STATE: WizardStepState = 'pending';

function toView(rows: readonly WizardStep[] | undefined): WizardStepView[] {
  const byId = new Map((rows ?? []).map((row) => [row.step, row]));
  let reachedActive = false;
  return WIZARD_STEP_METAS.map((meta) => {
    const row = byId.get(meta.id);
    const state = (row?.state as WizardStepState | undefined) ?? DEFAULT_STATE;
    const settled = state === 'complete' || state === 'skipped';
    // Everything up to and including the first unsettled step is reachable; nothing past it is,
    // because a later step's inputs are the earlier steps' outputs.
    const reachable = settled || !reachedActive;
    if (!settled) reachedActive = true;
    return {
      ...meta,
      state,
      skippable: row?.skippable ?? false,
      blocked: row?.blocked ?? false,
      reachable,
    };
  });
}

export function useWizard(): WizardView {
  const query = useSetupState();

  return useMemo(() => {
    const steps = toView(query.data?.steps);
    const active =
      query.data?.active_step ??
      steps.find((s) => s.state !== 'complete' && s.state !== 'skipped')?.id ??
      null;
    const resume = steps.find((s) => s.id === active) ?? WIZARD_STEP_METAS[0];
    return {
      steps,
      activeId: active,
      resumePath: query.data?.complete ? '/setup/done' : (resume?.path ?? '/setup/password'),
      claimed: query.data?.claimed ?? false,
      complete: query.data?.complete ?? false,
      tokenRequired: query.data?.token_required ?? false,
      isPending: query.isPending,
      isError: query.isError,
      error: query.error,
    };
  }, [query.data, query.isPending, query.isError, query.error]);
}

/** The next step after `id`, for the frame's Continue button. */
export function nextStepPath(id: string): string {
  const index = WIZARD_STEP_METAS.findIndex((step) => step.id === id);
  return WIZARD_STEP_METAS[index + 1]?.path ?? '/setup/done';
}

/** The previous step, for Back. Null on the first. */
export function previousStepPath(id: string): string | null {
  const index = WIZARD_STEPS_IN_RAIL.findIndex((step) => step.id === id);
  if (index <= 0) return null;
  return WIZARD_STEPS_IN_RAIL[index - 1]?.path ?? null;
}

/** `POST /api/v1/setup/skip` — the wizard's own record that a step was passed over deliberately. */
export function useSkipStep() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (step: string) => api.post('/api/v1/setup/skip', { body: { step } }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.setup.state() }),
  });
}

/** `POST /api/v1/setup/complete` — the last click of the wizard. */
export function useCompleteSetup() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => api.post('/api/v1/setup/complete'),
    onSuccess: async () => {
      await queryClient.invalidateQueries();
    },
  });
}
