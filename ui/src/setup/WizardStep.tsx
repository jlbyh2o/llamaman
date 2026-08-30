import { useNavigate } from '@tanstack/react-router';
import type { ReactNode } from 'react';
import { Button, Panel } from '../components';
import { nextStepPath, previousStepPath, useSkipStep, useWizard } from './useWizard';
import { stepMeta } from './steps';

/**
 * The frame every wizard step body renders inside.
 *
 * It owns the title, the description and the three buttons — Back, Skip, Continue — so the step
 * bodies contain only their own work. Skip is shown only when the *server* says this step is
 * skippable right now (section 11.2: models is skippable only when the cache scan already found
 * GGUFs), and it records the skip through `POST /setup/skip` before moving on, because a skipped
 * step is a row state, not a client-side jump.
 */

export interface WizardStepProps {
  step: string;
  children: ReactNode;
  /** Replaces the default Continue button — a step that submits a form supplies its own. */
  primaryAction?: ReactNode;
  /** Disable the default Continue until the step's work is done. */
  canContinue?: boolean;
  continueLabel?: string;
  /** Extra content between the description and the body: a warning, a status strip. */
  banner?: ReactNode;
}

export function WizardStep({
  step,
  children,
  primaryAction,
  canContinue = true,
  continueLabel = 'Continue',
  banner,
}: WizardStepProps) {
  const navigate = useNavigate();
  const wizard = useWizard();
  const skip = useSkipStep();

  const meta = stepMeta(step);
  const view = wizard.steps.find((s) => s.id === step);
  const back = previousStepPath(step);

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">
          {meta?.title ?? step}
        </h1>
        {meta?.description ? (
          <p className="mt-1 max-w-2xl text-sm text-[var(--lm-text-muted)]">{meta.description}</p>
        ) : null}
      </div>

      {banner}

      <Panel>{children}</Panel>

      <div className="flex items-center justify-between gap-2">
        <div>
          {back ? (
            <Button variant="ghost" onClick={() => void navigate({ to: back })}>
              Back
            </Button>
          ) : null}
        </div>
        <div className="flex items-center gap-2">
          {view?.skippable ? (
            <Button
              variant="ghost"
              loading={skip.isPending}
              onClick={() =>
                skip.mutate(step, { onSuccess: () => void navigate({ to: nextStepPath(step) }) })
              }
            >
              Skip this step
            </Button>
          ) : null}
          {primaryAction ?? (
            <Button
              variant="primary"
              disabled={!canContinue}
              onClick={() => void navigate({ to: nextStepPath(step) })}
            >
              {continueLabel}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
