import { Panel, PanelHeader, Progress, StatusBadge } from '../../components';
import type { Job } from '../../api/types';
import { formatRelative } from '../../format';

/**
 * The active-jobs strip of DESIGN section 4, screen 3.
 *
 * "Anything job-backed renders the job's progress rather than a spinner" (section 4), and this is
 * the one place that is true of *every* job at once: a build, a download, a scan, a bench sweep and
 * a self-update all land in `GET /jobs?state=active` and all arrive as `jobs` frames.
 *
 * `progress` is `object` on the wire — its shape belongs to the worker that writes it, not to the
 * API contract — so every field is checked before it is believed, and a job whose worker reports
 * nothing renders as indeterminate rather than as zero.
 */
export function ActiveJobs({ jobs }: { jobs: readonly Job[] }) {
  if (jobs.length === 0) return null;

  return (
    <Panel>
      <PanelHeader title="Running now" description="Work the daemon is doing in the background." />
      <ul className="mt-3 space-y-3">
        {jobs.map((job) => {
          const view = describeJobProgress(job);
          return (
            <li key={job.id}>
              <Progress
                value={view.percent}
                label={
                  <span className="flex items-center gap-2">
                    <StatusBadge kind="job" state={job.state} />
                    <span className="truncate">{jobKindLabel(job.kind)}</span>
                  </span>
                }
                detail={view.detail ?? formatRelative(job.started_at ?? job.created_at)}
              />
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

/** `llamacpp_install` → "Installing llama.cpp". A closed vocabulary (`model.JobKind`). */
export function jobKindLabel(kind: string): string {
  switch (kind) {
    case 'llamacpp_install':
      return 'Installing llama.cpp';
    case 'llamacpp_activate':
      return 'Activating llama.cpp';
    case 'llamacpp_delete':
      return 'Removing a llama.cpp build';
    case 'model_download':
      return 'Downloading a model';
    case 'model_verify':
      return 'Verifying a model';
    case 'model_delete':
      return 'Deleting a model';
    case 'cache_scan':
      return 'Scanning the model cache';
    case 'bench_run':
      return 'Running a benchmark';
    case 'self_update':
      return 'Updating Llama Man';
    case 'toolchain_probe':
      return 'Probing the toolchain';
    case 'maintenance':
      return 'Housekeeping';
    default:
      return kind.replace(/_/g, ' ');
  }
}

export interface JobProgressView {
  percent: number | null;
  detail: string | null;
}

export function describeJobProgress(job: Job): JobProgressView {
  const raw = (job.progress ?? {}) as Record<string, unknown>;
  const done = typeof raw['done'] === 'number' ? raw['done'] : null;
  const total = typeof raw['total'] === 'number' ? raw['total'] : null;
  const phase = typeof raw['phase'] === 'string' ? raw['phase'] : null;
  const message = typeof raw['message'] === 'string' ? raw['message'] : null;

  return {
    percent: done !== null && total !== null && total > 0 ? (done / total) * 100 : null,
    detail: message ?? phase?.replace(/_/g, ' ') ?? null,
  };
}
