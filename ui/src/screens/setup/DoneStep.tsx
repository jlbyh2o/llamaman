import type { ReactNode } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { Boxes, CheckCircle2, Cpu, Server } from 'lucide-react';
import { Badge, Button, Panel, toast } from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type { Instance, Model } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { useCompleteSetup, useWizard } from '../../setup/useWizard';

/**
 * The last screen of the wizard.
 *
 * `POST /api/v1/setup/complete` is the only thing it does, and it is deliberately explicit: until
 * that call lands, the wizard is resumable, and after it the same work is reachable from Settings.
 * "Every step is idempotent and re-enterable from /settings later; there is no wizard-only
 * capability" (DESIGN section 11.2), which is what this copy promises.
 *
 * The summary above the button is read from the rows themselves rather than from anything this
 * session remembers, so a wizard finished in a second browser — or resumed a day later — describes
 * the host as it actually is. `409 wizard_step_locked` is the server refusing to call a host
 * finished while a non-skippable step is not: it names the step, and the answer is to go back to it
 * rather than to try again.
 */
export function DoneStep() {
  const navigate = useNavigate();
  const wizard = useWizard();
  const complete = useCompleteSetup();

  const active = useQuery({
    queryKey: queryKeys.llamacpp.active(),
    queryFn: () =>
      api.get('/api/v1/llamacpp/active').catch((error: unknown) => {
        if (error instanceof ApiError && error.status === 404) return null;
        throw error;
      }),
    retry: false,
  });

  const models = useQuery({
    queryKey: queryKeys.models.list({ state: 'ready' }),
    queryFn: () => api.get('/api/v1/models', { query: { state: 'ready' } }),
  });

  const instances = useQuery({
    queryKey: queryKeys.instances.list(),
    queryFn: () => api.get('/api/v1/instances'),
  });

  const version = active.data?.version ?? null;
  const modelRows: readonly Model[] = models.data?.items ?? [];
  const instanceRows: readonly Instance[] = instances.data?.items ?? [];
  const modelBytes = modelRows.reduce((sum, row) => sum + row.total_bytes, 0);

  const blocked =
    complete.error instanceof ApiError && complete.error.code === 'wizard_step_locked'
      ? complete.error
      : null;
  const unfinished = wizard.steps.find(
    (step) => step.id !== 'done' && !step.skippable && step.state !== 'complete',
  );

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Ready</h1>
        <p className="mt-1 max-w-2xl text-sm text-[var(--lm-text-muted)]">
          Everything the wizard set up is editable later in Settings. Nothing here was one-shot.
        </p>
      </div>

      <Panel>
        <div className="flex items-start gap-3">
          <CheckCircle2 aria-hidden className="mt-0.5 size-5 shrink-0 text-[var(--lm-ok)]" />
          <div className="min-w-0 space-y-3">
            <ul className="space-y-2">
              <SummaryLine
                icon={<Cpu />}
                title="llama.cpp"
                detail={
                  version ? (
                    <>
                      <span className="lm-numeric">{version.build_tag ?? version.tag}</span>{' '}
                      <Badge tone="neutral">{version.backend}</Badge>{' '}
                      <Badge tone="neutral">{version.acquisition}</Badge>
                    </>
                  ) : (
                    'not installed yet'
                  )
                }
              />
              <SummaryLine
                icon={<Boxes />}
                title="Models"
                detail={
                  modelRows.length
                    ? `${formatCount(modelRows.length)} ready · ${formatBytes(modelBytes)}`
                    : 'none yet — the Models screen is where they come from'
                }
              />
              <SummaryLine
                icon={<Server />}
                title="Instances"
                detail={
                  instanceRows.length
                    ? instanceRows.map((row) => row.name).join(', ')
                    : 'none yet — one can be created from the dashboard'
                }
              />
            </ul>

            <p className="text-sm text-[var(--lm-text-muted)]">
              Instances run as their own systemd units, so restarting or updating Llama Man never
              interrupts a loaded model. Anything skipped — a Hugging Face token, a first model, a
              first instance — is waiting where it belongs rather than gone.
            </p>
          </div>
        </div>
      </Panel>

      {blocked ? (
        <div className="flex items-center justify-between gap-3 rounded-[var(--lm-radius-lg)] border border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)] p-3">
          <p className="text-sm text-[var(--lm-text-muted)]">{blocked.message}</p>
          {unfinished ? (
            <Button onClick={() => void navigate({ to: unfinished.path })}>
              Back to {unfinished.railLabel}
            </Button>
          ) : null}
        </div>
      ) : null}

      <div className="flex justify-end">
        <Button
          variant="primary"
          loading={complete.isPending}
          onClick={() =>
            complete.mutate(undefined, {
              onSuccess: () => void navigate({ to: '/', replace: true }),
              onError: (error) => {
                if (error instanceof ApiError && error.code === 'wizard_step_locked') return;
                toast.error(error);
              },
            })
          }
        >
          Finish setup
        </Button>
      </div>
    </div>
  );
}

function SummaryLine({
  icon,
  title,
  detail,
}: {
  icon: ReactNode;
  title: string;
  detail: ReactNode;
}) {
  return (
    <li className="flex items-baseline gap-2 text-sm">
      <span aria-hidden className="text-[var(--lm-text-faint)] [&>svg]:size-3.5">
        {icon}
      </span>
      <span className="w-24 shrink-0 text-[var(--lm-text-muted)]">{title}</span>
      <span className="min-w-0 flex-1 text-[var(--lm-text)]">{detail}</span>
    </li>
  );
}
