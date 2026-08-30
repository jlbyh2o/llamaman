/**
 * Activate a build, or roll back to the retained previous one (DESIGN section 3.5, section 6.6).
 *
 * Two steps, not one dialog that fires and forgets:
 *
 *  1. **Confirm.** "None" flips the symlink and leaves every instance `restart_required`; "Rolling"
 *     additionally restarts running instances one at a time, canary first, gated on `/health` —
 *     SPEC section 3.1's "rolling restart with confirmation" is this choice, made before the request
 *     leaves the browser.
 *  2. **Watch.** The activation is a job (section 2.3); a canary failure is an *automatic* revert
 *     (D24) — `is_active`/`previous_active` restored, every instance's `restart_required` cleared —
 *     so a `failed` job here already means the fleet is back where it started, and the dialog says
 *     that rather than leaving it to be inferred from a status code.
 */

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { CheckCircle2, RotateCcw, Undo2, XCircle } from 'lucide-react';
import {
  Badge,
  Button,
  Dialog,
  DialogContent,
  FormField,
  Progress,
  Select,
} from '../../components';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import { asPage, selectFieldProps } from '../../features/system/api';
import { isTerminalJobState, summarizeProgress, useJob } from '../../features/system/jobs';
import type { Instance } from '../../api/types';
import { useActivateLlamacpp, useRollbackLlamacpp } from './queries';

export interface ActivateDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Activating a specific `ready` version, or rolling back to `previous_active`. */
  mode: { kind: 'activate'; id: string; tag: string } | { kind: 'rollback' };
}

export function ActivateDialog({ open, onOpenChange, mode }: ActivateDialogProps) {
  const [restartInstances, setRestartInstances] = useState<'none' | 'rolling'>('none');
  const [canaryId, setCanaryId] = useState('');
  const [jobId, setJobId] = useState<string | null>(null);

  const activate = useActivateLlamacpp();
  const rollback = useRollbackLlamacpp();
  const pending = activate.isPending || rollback.isPending;

  const instances = useQuery({
    queryKey: queryKeys.instances.list({ kind: 'canary-picker' }),
    queryFn: async () => asPage<Instance>(await api.get('/api/v1/instances')),
    enabled: open && restartInstances === 'rolling',
  });
  const running = (instances.data?.items ?? []).filter((i) => i.status.state === 'ready');

  const job = useJob(jobId);
  const terminal = job.data ? isTerminalJobState(job.data.state) : false;
  const progress = summarizeProgress(job.data?.progress);

  const close = () => {
    setJobId(null);
    setRestartInstances('none');
    setCanaryId('');
    onOpenChange(false);
  };

  const onConfirm = () => {
    const body = {
      restart_instances: restartInstances,
      ...(canaryId ? { canary_instance_id: canaryId } : {}),
    };
    const onSuccess = (receipt: { job_id?: string | null }) => setJobId(receipt.job_id ?? null);
    if (mode.kind === 'activate') {
      activate.mutate({ id: mode.id, ...body }, { onSuccess });
    } else {
      rollback.mutate(body, { onSuccess });
    }
  };

  const title =
    mode.kind === 'activate' ? `Activate ${mode.tag}` : 'Roll back to the previous build';

  return (
    <Dialog open={open} onOpenChange={(next) => (next ? onOpenChange(next) : close())}>
      <DialogContent
        title={title}
        description={
          jobId
            ? undefined
            : 'Every running instance is flagged "restart required" the moment this commits, whether or not it restarts anything itself.'
        }
        footer={
          jobId ? (
            <Button variant={terminal ? 'primary' : 'ghost'} onClick={close}>
              {terminal ? 'Done' : 'Run in background'}
            </Button>
          ) : (
            <>
              <Button variant="ghost" onClick={close} disabled={pending}>
                Cancel
              </Button>
              <Button
                variant="primary"
                icon={mode.kind === 'rollback' ? <Undo2 /> : <RotateCcw />}
                loading={pending}
                onClick={onConfirm}
              >
                {mode.kind === 'rollback' ? 'Roll back' : 'Activate'}
              </Button>
            </>
          )
        }
      >
        {jobId ? (
          <div className="space-y-3">
            <div className="flex items-center gap-2">
              {job.data?.state === 'succeeded' ? (
                <CheckCircle2 className="size-4 text-[var(--lm-ok)]" aria-hidden />
              ) : job.data?.state === 'failed' ? (
                <XCircle className="size-4 text-[var(--lm-danger)]" aria-hidden />
              ) : null}
              <Badge
                tone={
                  job.data?.state === 'failed'
                    ? 'danger'
                    : job.data?.state === 'succeeded'
                      ? 'ok'
                      : 'info'
                }
                dot
                pulse={!terminal}
              >
                {job.data?.state ?? 'queued'}
              </Badge>
            </div>
            <Progress
              value={progress.percent}
              label={
                progress.label ??
                (restartInstances === 'rolling' ? 'Rolling restart' : 'Activating')
              }
            />
            {job.data?.state === 'failed' ? (
              <p className="text-xs text-[var(--lm-text-muted)]">
                {restartInstances === 'rolling'
                  ? "The canary didn't come back healthy, so the activation was reverted automatically — every instance is back on its previous build and restart_required has cleared."
                  : (job.data.error_message ?? 'The activation failed.')}
              </p>
            ) : null}
          </div>
        ) : (
          <div className="space-y-4">
            <FormField label="Restart running instances">
              {(field) => (
                <Select
                  {...selectFieldProps(field)}
                  value={restartInstances}
                  onValueChange={(v) => setRestartInstances(v as 'none' | 'rolling')}
                  options={[
                    {
                      value: 'none',
                      label: 'Not now',
                      description: 'Flip the active build; restart later, per instance.',
                    },
                    {
                      value: 'rolling',
                      label: 'Rolling restart',
                      description: 'Restart each running instance, canary first, gated on health.',
                    },
                  ]}
                />
              )}
            </FormField>
            {restartInstances === 'rolling' ? (
              <FormField
                label="Canary instance"
                hint="Restarted first; a failed health check reverts the whole activation."
              >
                {(field) => (
                  <Select
                    {...selectFieldProps(field)}
                    value={canaryId || undefined}
                    onValueChange={setCanaryId}
                    placeholder="Creation order (default)"
                    options={running.map((i) => ({ value: i.id, label: i.display_name || i.name }))}
                  />
                )}
              </FormField>
            ) : null}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
