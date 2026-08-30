/**
 * A generic job watcher.
 *
 * `jobs` is a real SSE topic (section 3.14) and `GET /api/v1/jobs/{id}` is a real, generated path
 * (`api/schema.d.ts`), so this is the one hook in this module that needs no stand-in typing — it is
 * shared here because three areas this agent owns are all "start a `202` job, then watch it end":
 * a llama.cpp build or activation, and a self-update. `progress` is `Record<string, unknown>`
 * (section 3.14 does not pin its shape per job kind), so `summarizeProgress` reads the handful of
 * key spellings a job worker plausibly writes rather than assuming one.
 */

import { useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { Job, JobState } from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';

export const TERMINAL_JOB_STATES: readonly JobState[] = ['succeeded', 'failed', 'canceled'];

export function isTerminalJobState(state: JobState | string): boolean {
  return (TERMINAL_JOB_STATES as readonly string[]).includes(state);
}

/** Watches one job until it reaches a terminal state, over the `jobs` SSE topic. */
export function useJob(id: string | null | undefined): UseQueryResult<Job, Error> {
  const live = useLiveQueryOptions(1500);
  return useQuery({
    queryKey: queryKeys.jobs.detail(id ?? 'none'),
    queryFn: ({ signal }) => api.get('/api/v1/jobs/{id}', { path: { id: id as string }, signal }),
    enabled: Boolean(id),
    ...live,
  });
}

export interface ProgressSummary {
  /** 0-100, or null when the job has not reported enough to compute one. */
  percent: number | null;
  /** "4 of 7 instances", "Compiling ggml-cuda.cu" — whatever the worker chose to say. */
  label: string | null;
}

function firstNumber(progress: Record<string, unknown>, ...keys: string[]): number | undefined {
  for (const key of keys) {
    const value = progress[key];
    if (typeof value === 'number' && Number.isFinite(value)) return value;
  }
  return undefined;
}

function firstString(progress: Record<string, unknown>, ...keys: string[]): string | undefined {
  for (const key of keys) {
    const value = progress[key];
    if (typeof value === 'string' && value !== '') return value;
  }
  return undefined;
}

/** Best-effort reading of a job's `progress` blob — see the module docstring. */
export function summarizeProgress(
  progress: Record<string, unknown> | null | undefined,
): ProgressSummary {
  if (!progress || Object.keys(progress).length === 0) return { percent: null, label: null };

  const percent = firstNumber(progress, 'percent', 'pct');
  const done = firstNumber(progress, 'done', 'current', 'migrated', 'completed');
  const total = firstNumber(progress, 'total', 'count');
  const label = firstString(progress, 'message', 'step', 'phase', 'status', 'detail') ?? null;

  if (percent !== undefined) return { percent, label };
  if (done !== undefined && total !== undefined && total > 0) {
    return { percent: (done / total) * 100, label: label ?? `${done} of ${total}` };
  }
  return { percent: null, label };
}
