import { useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Download, Hammer, PlayCircle, Rocket } from 'lucide-react';
import {
  Badge,
  Button,
  Field,
  FormField,
  LogViewer,
  PanelHeader,
  Progress,
  Select,
  StatusBadge,
  Switch,
  toast,
} from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type { Job, LlamacppPlan, LlamacppVersion } from '../../api/types';
import { EventStreamProvider, useLiveQueryOptions } from '../../events/EventStreamProvider';
import { useLogStream } from '../../events/useLogStream';
import { formatBytes, formatEstimate, formatRelative } from '../../format';
import { WizardStep } from '../../setup/WizardStep';
import { fieldProps } from './fieldProps';
import { useWizardScratch } from './scratch';
import type { Backend } from './scratch';

/**
 * Wizard step `llamacpp` (DESIGN section 11.2): "channel + version picker with the plan preview
 * (§6.3); starts the install or build and streams the log; the user may leave and come back".
 *
 * Not skippable — "there is nothing to run without it" — so the step's Continue waits on a version
 * that is actually `ready` and active, read from `GET /llamacpp/active`, never on a click. That is
 * also what makes the step resumable: a browser refreshed mid-build, or a daemon restarted mid-build,
 * finds the in-flight row in `GET /llamacpp/versions` and reattaches to its log. Nothing about where
 * this step stands is kept in the client.
 *
 * The stream is mounted here rather than in the shell because the wizard renders outside the
 * session-gated layout: by this step a session exists (the password step minted it), so the topic
 * stream can be opened for exactly as long as this screen is on-screen, and the build's progress
 * arrives as `llamacpp` and `jobs` frames instead of a polling loop.
 */
export function LlamacppStep() {
  return (
    <EventStreamProvider topics={['llamacpp', 'jobs']}>
      <LlamacppStepBody />
    </EventStreamProvider>
  );
}

/** The states of section 2.5 in which a version is still being acquired. */
const IN_FLIGHT = new Set(['pending', 'resolving', 'fetching', 'building', 'verifying']);

/** The version picker's "let the channel decide" option. Never sent as a `?tag=`. */
const LATEST = 'latest';

function LlamacppStepBody() {
  const queryClient = useQueryClient();
  const live = useLiveQueryOptions();
  const scratchBackend = useWizardScratch((state) => state.backend);

  const [channel, setChannel] = useState<'stable' | 'nightly'>('stable');
  // `LATEST` rather than an empty string: a Radix Select item may not carry `""`, and "whatever the
  // channel resolves to" is a real choice rather than the absence of one. It is translated back to
  // an omitted `?tag=` at the two places that ask the daemon.
  const [tag, setTag] = useState<string>(LATEST);
  const [backendOverride, setBackendOverride] = useState<Backend | null>(null);
  const [forceSource, setForceSource] = useState(false);

  const backend: Backend = backendOverride ?? scratchBackend ?? 'cpu';
  /** The tag to actually pin, or empty for "whatever the channel resolves to". */
  const pinned = tag === LATEST ? '' : tag;

  const active = useQuery({
    queryKey: queryKeys.llamacpp.active(),
    // 404 is the honest answer on a host with nothing installed, not a failure.
    queryFn: () =>
      api.get('/api/v1/llamacpp/active').catch((error: unknown) => {
        if (error instanceof ApiError && error.status === 404) return null;
        throw error;
      }),
    retry: false,
    ...live,
  });

  const versions = useQuery({
    queryKey: queryKeys.llamacpp.versions(),
    queryFn: () => api.get('/api/v1/llamacpp/versions'),
    ...live,
  });

  const releases = useQuery({
    queryKey: queryKeys.llamacpp.releases(channel),
    queryFn: () => api.get('/api/v1/llamacpp/releases', { query: { channel } }),
    retry: false,
  });

  const plan = useQuery({
    queryKey: queryKeys.llamacpp.plan({ channel, tag, backend, force_source: forceSource }),
    queryFn: () =>
      api.get('/api/v1/llamacpp/plan', {
        query: { channel, backend, force_source: forceSource, ...(pinned ? { tag: pinned } : {}) },
      }),
    retry: false,
  });

  const jobs = useQuery({
    queryKey: queryKeys.jobs.list({ state: 'active', kind: 'llamacpp_install' }),
    queryFn: () =>
      api.get('/api/v1/jobs', { query: { state: 'active', kind: 'llamacpp_install' } }),
    ...live,
  });

  const install = useMutation({
    mutationFn: () =>
      api.post('/api/v1/llamacpp/versions', {
        body: { channel, backend, force_source: forceSource, ...(pinned ? { tag: pinned } : {}) },
      }),
    onSuccess: async (result) => {
      if (result?.reused) toast.info('That build already exists on this host.');
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('llamacpp') });
    },
    onError: (error) => toast.error(error),
  });

  const activate = useMutation({
    mutationFn: (id: string) =>
      api.post('/api/v1/llamacpp/versions/{id}/activate', {
        path: { id },
        body: { restart_instances: 'none' },
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('llamacpp') });
    },
    onError: (error) => toast.error(error),
  });

  const cancel = useMutation({
    mutationFn: (id: string) => api.post('/api/v1/llamacpp/versions/{id}/cancel', { path: { id } }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('llamacpp') });
    },
    onError: (error) => toast.error(error),
  });

  // The generated list envelope (`List[github.com/…]`) collapses to `any` in schema.d.ts, because
  // openapi-typescript splits that schema name on its slashes. Annotating the destination is what
  // restores the element type without a cast.
  const rows: readonly LlamacppVersion[] = versions.data?.items ?? [];
  const jobActive: readonly Job[] = jobs.data?.items ?? [];
  const building = rows.find((row) => IN_FLIGHT.has(row.state));
  const readyNotActive = rows.find((row) => row.state === 'ready' && !row.is_active);
  const activeVersion = active.data?.version ?? null;
  const installed = activeVersion?.state === 'ready';

  const releaseOptions = useMemo(() => {
    const list = releases.data?.releases ?? [];
    return [
      {
        value: LATEST,
        label: `Latest ${channel}`,
        description: 'Whatever the channel resolves to now',
      },
      ...list.map((release) => ({
        value: release.tag,
        label: release.tag,
        ...(release.installed ? { description: 'Already on this host' } : {}),
      })),
    ];
  }, [releases.data, channel]);

  return (
    <WizardStep
      step="llamacpp"
      canContinue={installed}
      continueLabel={installed ? 'Continue' : 'Waiting for a build'}
      banner={
        installed && activeVersion ? (
          <div className="flex flex-wrap items-center gap-2 rounded-[var(--lm-radius-lg)] border border-[var(--lm-ok)]/40 bg-[var(--lm-ok-soft)] px-3 py-2">
            <Rocket aria-hidden className="size-4 shrink-0 text-[var(--lm-ok)]" />
            <span className="text-sm text-[var(--lm-text)]">
              <span className="lm-numeric">{activeVersion.build_tag ?? activeVersion.tag}</span> is
              active
            </span>
            <Badge tone="neutral">{activeVersion.backend}</Badge>
            <Badge tone="neutral">{activeVersion.acquisition}</Badge>
            {activeVersion.size_bytes ? (
              <Badge tone="neutral">{formatBytes(activeVersion.size_bytes)}</Badge>
            ) : null}
          </div>
        ) : null
      }
    >
      <div className="space-y-5">
        {building ? (
          <BuildProgress
            version={building}
            jobs={jobActive}
            onCancel={() => cancel.mutate(building.id)}
            canceling={cancel.isPending}
          />
        ) : (
          <>
            <PanelHeader
              title="Pick a version"
              description="The plan below says whether this will be a download or a source build before anything starts."
            />

            <div className="grid gap-4 sm:grid-cols-2">
              <FormField label="Channel">
                {(field) => (
                  <Select<'stable' | 'nightly'>
                    {...fieldProps(field)}
                    value={channel}
                    onValueChange={(next) => {
                      setChannel(next);
                      setTag(LATEST);
                    }}
                    options={[
                      {
                        value: 'stable',
                        label: 'Stable',
                        description: 'Tagged llama.cpp releases',
                      },
                      {
                        value: 'nightly',
                        label: 'Nightly',
                        description: 'The latest master build',
                      },
                    ]}
                  />
                )}
              </FormField>

              <FormField label="Version">
                {(field) => (
                  <Select
                    {...fieldProps(field)}
                    value={tag}
                    onValueChange={setTag}
                    mono
                    disabled={releases.isPending}
                    options={releaseOptions}
                  />
                )}
              </FormField>

              <FormField
                label="Backend"
                hint="CUDA offloads layers to NVIDIA GPUs; CPU runs anywhere."
              >
                {(field) => (
                  <Select<Backend>
                    {...fieldProps(field)}
                    value={backend}
                    onValueChange={setBackendOverride}
                    options={[
                      { value: 'cpu', label: 'CPU' },
                      { value: 'cuda', label: 'CUDA' },
                    ]}
                  />
                )}
              </FormField>

              <FormField
                label="Build from source"
                hint="Off by default: a prebuilt asset for this host is faster and identical."
              >
                {(field) => (
                  <div className="flex h-8 items-center gap-2">
                    <Switch
                      id={field.id}
                      aria-describedby={field['aria-describedby']}
                      checked={forceSource}
                      onCheckedChange={setForceSource}
                    />
                    <span className="text-sm text-[var(--lm-text-muted)]">
                      Ignore prebuilt assets
                    </span>
                  </div>
                )}
              </FormField>
            </div>

            <PlanPreview plan={plan.data} error={plan.error} loading={plan.isFetching} />

            {releases.isError ? (
              <p className="text-xs text-[var(--lm-warn)]">
                The release list could not be fetched. Installing the latest of the channel still
                works if the daemon can reach GitHub when the job runs.
              </p>
            ) : null}

            <div className="flex flex-wrap items-center gap-2">
              <Button
                variant="primary"
                icon={plan.data?.acquisition === 'source' ? <Hammer /> : <Download />}
                loading={install.isPending}
                disabled={!plan.data?.can_proceed}
                onClick={() => install.mutate()}
              >
                {plan.data?.acquisition === 'source'
                  ? 'Build this version'
                  : 'Download this version'}
              </Button>
              {readyNotActive ? (
                <Button
                  icon={<PlayCircle />}
                  loading={activate.isPending}
                  onClick={() => activate.mutate(readyNotActive.id)}
                >
                  Activate {readyNotActive.build_tag ?? readyNotActive.tag}
                </Button>
              ) : null}
            </div>
          </>
        )}

        {rows.length ? <VersionList rows={rows} /> : null}
      </div>
    </WizardStep>
  );
}

/** Section 6.3's answer, rendered before anything is committed. */
function PlanPreview({
  plan,
  error,
  loading,
}: {
  plan: LlamacppPlan | undefined;
  error: unknown;
  loading: boolean;
}) {
  if (error) {
    return (
      <div className="rounded-[var(--lm-radius)] border border-[var(--lm-danger)]/40 bg-[var(--lm-danger-soft)] p-3 text-sm text-[var(--lm-text-muted)]">
        {error instanceof ApiError ? error.message : 'The plan could not be resolved.'}
      </div>
    );
  }
  if (!plan) {
    return (
      <div className="rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] p-3 text-sm text-[var(--lm-text-faint)]">
        {loading ? 'Working out what this would do…' : 'Pick a version to see the plan.'}
      </div>
    );
  }

  return (
    <div className="space-y-2 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={plan.acquisition === 'source' ? 'warn' : 'accent'} dot>
          {plan.acquisition === 'source' ? 'Source build' : 'Prebuilt download'}
        </Badge>
        <Badge tone="neutral">{plan.backend}</Badge>
        <Badge tone="neutral">{formatEstimate(plan.estimated_minutes)}</Badge>
        {plan.can_proceed ? null : <Badge tone="danger">Cannot proceed</Badge>}
      </div>
      <p className="text-sm text-[var(--lm-text-muted)]">{plan.reason}</p>
      <dl className="grid gap-3 sm:grid-cols-3">
        <Field label="Resolves to" mono>
          {plan.build_tag || plan.tag}
        </Field>
        <Field label="Free space" mono>
          {formatBytes(plan.free_bytes)} of {formatBytes(plan.required_bytes)} needed
        </Field>
        <Field label="Asset" mono>
          {plan.asset_name ?? '—'}
        </Field>
      </dl>
      {plan.missing_tools.length ? (
        <p className="text-xs text-[var(--lm-danger)]">
          Missing from this host:{' '}
          <span className="lm-numeric">{plan.missing_tools.join(', ')}</span>. Go back a step for
          what carries each one.
        </p>
      ) : null}
    </div>
  );
}

/**
 * The live half.
 *
 * The build's phase comes from the job's `progress_json` — `{phase, done, total, message}`, written
 * by `internal/llamacpp`'s install worker on every step — and the log from the SSE tail the log
 * endpoint answers with when the request accepts an event stream. Both arrive without this screen
 * polling anything, which is what lets a user close the tab and come back.
 */
function BuildProgress({
  version,
  jobs,
  onCancel,
  canceling,
}: {
  version: LlamacppVersion;
  jobs: readonly Job[];
  onCancel: () => void;
  canceling: boolean;
}) {
  const job = jobs.find(
    (row) => row.subject?.type === 'llamacpp_version' && row.subject.id === version.id,
  );
  const progress = readProgress(job);
  const { lines, status } = useLogStream({
    url: `/api/v1/llamacpp/versions/${version.id}/log?tail=400`,
    maxLines: 20_000,
  });

  return (
    <div className="space-y-3">
      <PanelHeader
        title={
          <span className="flex items-center gap-2">
            <span className="lm-numeric">{version.build_tag ?? version.tag}</span>
            <StatusBadge kind="llamacpp" state={version.state} />
          </span>
        }
        description={
          version.started_at
            ? `Started ${formatRelative(version.started_at)} — you may leave this page; the job keeps running.`
            : 'Queued. You may leave this page; the job keeps running.'
        }
        actions={
          <Button variant="danger" loading={canceling} onClick={onCancel}>
            Cancel
          </Button>
        }
      />

      <Progress
        value={progress.percent}
        label={progress.label}
        detail={progress.detail}
        aria-label="Build progress"
      />

      <LogViewer
        lines={lines}
        rows={18}
        aria-label="Build log"
        placeholder={status === 'live' ? 'Waiting for output…' : 'Connecting to the build log…'}
      />
    </div>
  );
}

interface ProgressView {
  percent: number | null;
  label: string;
  detail: string | null;
}

/**
 * Read the install worker's progress frame. It is `object` on the wire — the shape belongs to the
 * worker, not to the contract — so every field is checked rather than trusted.
 */
export function readProgress(job: Job | undefined): ProgressView {
  const raw = (job?.progress ?? {}) as Record<string, unknown>;
  const phase = typeof raw['phase'] === 'string' ? raw['phase'] : null;
  const message = typeof raw['message'] === 'string' ? raw['message'] : null;
  const done = typeof raw['done'] === 'number' ? raw['done'] : null;
  const total = typeof raw['total'] === 'number' ? raw['total'] : null;

  const percent = done !== null && total !== null && total > 0 ? (done / total) * 100 : null;
  return {
    percent,
    label: phase ? phase.replace(/_/g, ' ') : 'Working',
    detail: message ?? (done !== null && total !== null && total > 0 ? `${done}/${total}` : null),
  };
}

function VersionList({ rows }: { rows: readonly LlamacppVersion[] }) {
  return (
    <div>
      <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
        On this host
      </p>
      <ul className="mt-1 divide-y divide-[var(--lm-border)]">
        {rows.slice(0, 6).map((row) => (
          <li key={row.id} className="flex flex-wrap items-center gap-2 py-2">
            <span className="lm-numeric text-[13px] text-[var(--lm-text)]">
              {row.build_tag ?? row.tag}
            </span>
            <StatusBadge kind="llamacpp" state={row.state} />
            <Badge tone="neutral">{row.backend}</Badge>
            {row.is_active ? <Badge tone="ok">Active</Badge> : null}
            <span className="flex-1" />
            <span className="text-xs text-[var(--lm-text-faint)]">
              {formatRelative(row.finished_at ?? row.created_at)}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
