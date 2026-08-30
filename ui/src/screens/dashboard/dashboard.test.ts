/**
 * The dashboard's one piece of interpretation.
 *
 * A job's `progress` is `object` on the wire: its shape belongs to the worker that writes it, not
 * to the API contract, so the strip that renders every kind of job at once has to read it
 * defensively. These are the cases that reach it — the build worker's `{phase, done, total,
 * message}`, a worker that has written nothing yet, and a worker whose frame this build does not
 * understand — and none of them may produce a bar that lies.
 */

import { describe, expect, it } from 'vitest';

import type { Job } from '../../api/types';
import { describeJobProgress, jobKindLabel } from './ActiveJobs';

function job(progress: unknown): Job {
  return {
    id: '01J8Z',
    kind: 'llamacpp_install',
    state: 'running',
    subject: { type: 'llamacpp_version', id: '01J8Y' },
    priority: 100,
    attempts: 1,
    max_attempts: 3,
    cancel_requested: false,
    created_at: '2026-08-30T10:00:00Z',
    run_after: '2026-08-30T10:00:00Z',
    progress,
  } as unknown as Job;
}

describe('describeJobProgress', () => {
  it('reads the install worker’s frame', () => {
    const view = describeJobProgress(
      job({ phase: 'build', done: 40, total: 80, message: 'ninja' }),
    );
    expect(view.percent).toBe(50);
    expect(view.detail).toBe('ninja');
  });

  it('is indeterminate rather than zero when nothing has been reported', () => {
    expect(describeJobProgress(job(undefined)).percent).toBeNull();
    expect(describeJobProgress(job({})).percent).toBeNull();
    expect(describeJobProgress(job({ phase: 'preflight' })).percent).toBeNull();
  });

  it('refuses to divide by a zero or missing total', () => {
    expect(describeJobProgress(job({ done: 5, total: 0 })).percent).toBeNull();
    expect(describeJobProgress(job({ done: 5 })).percent).toBeNull();
  });

  it('ignores fields of the wrong type instead of rendering them', () => {
    const view = describeJobProgress(job({ done: '40', total: '80', message: 12, phase: null }));
    expect(view.percent).toBeNull();
    expect(view.detail).toBeNull();
  });

  it('falls back to the phase when there is no message', () => {
    expect(describeJobProgress(job({ phase: 'verify_signature' })).detail).toBe('verify signature');
  });
});

describe('jobKindLabel', () => {
  it('names every kind of model.JobKind', () => {
    const kinds = [
      'llamacpp_install',
      'llamacpp_activate',
      'llamacpp_delete',
      'model_download',
      'model_verify',
      'model_delete',
      'cache_scan',
      'bench_run',
      'self_update',
      'toolchain_probe',
      'maintenance',
    ];
    for (const kind of kinds) {
      const label = jobKindLabel(kind);
      expect(label).not.toBe(kind);
      expect(label.length).toBeGreaterThan(0);
    }
  });

  it('renders a kind this build does not know rather than hiding the job', () => {
    expect(jobKindLabel('future_kind')).toBe('future kind');
  });
});
