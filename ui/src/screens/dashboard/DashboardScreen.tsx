import { useMemo } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { ArrowUpCircle, Cpu, Server } from 'lucide-react';
import {
  Badge,
  Button,
  EmptyState,
  Field,
  LoadingPanel,
  Panel,
  PanelHeader,
  QueryError,
} from '../../components';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { AuditEvent, CacheRoot, Instance, Job, LlamacppVersion, Model } from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { formatBytes, formatRelative } from '../../format';
import { ActiveJobs } from './ActiveJobs';
import { GpuPanel } from './GpuPanel';
import { InstanceCard } from './InstanceCard';
import { Notifications } from './Notifications';
import { QuickActions } from './QuickActions';
import { RecentEvents } from './RecentEvents';
import { StoragePanel } from './StoragePanel';

/**
 * The dashboard (DESIGN section 4, screen 3): "instance cards (state, port, model, slots busy,
 * tokens/sec when metrics are on), GPU VRAM meters, active-jobs strip, disk usage, notifications,
 * recent events, update banner".
 *
 * It is the landing view, so its job is to answer three questions before anything is clicked: is
 * everything running, is anything happening, and is anything wrong. Nothing on it is a control
 * surface — every action is a link to the screen that owns the work — because a landing page that
 * can stop a serving instance in one click is a landing page nobody trusts.
 *
 * **No polling.** Five of the six queries below are kept fresh by the SSE topics the shell already
 * subscribes to — `instances`, `jobs`, `events`, `llamacpp`, `downloads` — and `useLiveQueryOptions`
 * turns interval refetch *on* only when the stream has given up, which is section 4's stated
 * fallback rather than a default. The two families with no topic (`models`, `cache`) are read once
 * and change only as a consequence of something that does have one.
 */
export function DashboardScreen() {
  const live = useLiveQueryOptions();

  const instances = useQuery({
    queryKey: queryKeys.instances.list(),
    queryFn: () => api.get('/api/v1/instances'),
    ...live,
  });

  const models = useQuery({
    queryKey: queryKeys.models.list(),
    queryFn: () => api.get('/api/v1/models'),
  });

  const jobs = useQuery({
    queryKey: queryKeys.jobs.list({ state: 'active' }),
    queryFn: () => api.get('/api/v1/jobs', { query: { state: 'active' } }),
    ...live,
  });

  const events = useQuery({
    queryKey: queryKeys.events.log({ limit: 12 }),
    queryFn: () => api.get('/api/v1/events/log', { query: { limit: '12' } }),
    ...live,
  });

  const roots = useQuery({
    queryKey: queryKeys.cache.roots(),
    queryFn: () => api.get('/api/v1/cache/roots'),
  });

  const versions = useQuery({
    queryKey: queryKeys.llamacpp.versions(),
    queryFn: () => api.get('/api/v1/llamacpp/versions'),
    ...live,
  });

  const update = useQuery({
    queryKey: queryKeys.update.status(),
    queryFn: () => api.get('/api/v1/update/status'),
    retry: false,
  });

  // The generated list envelope (`List[github.com/…]`) collapses to `any` in schema.d.ts, because
  // openapi-typescript splits that schema name on its slashes. Annotating each destination is what
  // restores the element type without a cast.
  const instanceRows: readonly Instance[] = instances.data?.items ?? [];
  const modelRows: readonly Model[] = models.data?.items ?? [];
  const jobRows: readonly Job[] = jobs.data?.items ?? [];
  const eventRows: readonly AuditEvent[] = events.data?.items ?? [];
  const rootRows: readonly CacheRoot[] = roots.data?.items ?? [];
  const versionRows: readonly LlamacppVersion[] = versions.data?.items ?? [];

  const modelsById = useMemo(
    () => new Map(modelRows.map((model) => [model.id, model])),
    [modelRows],
  );

  // The active build comes out of the versions list rather than from a second request to
  // `GET /llamacpp/active`.
  //
  // Two reasons, and the second is the one that shows. Every field this panel renders is on the
  // list row already, so the extra request bought nothing; and that endpoint answers `404` on a
  // host with nothing installed — a documented state the screen handled fine, but one the BROWSER
  // logs as a failed request on the landing page of every fresh install, which is noise a user
  // sees in devtools and the end-to-end suite counts as a defect. Reading `is_active` from a list
  // the screen already has makes the question answerable without asking it.
  const version = versionRows.find((row) => row.is_active) ?? null;

  return (
    <div className="space-y-4 p-4 lg:p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Dashboard</h1>
          <p className="mt-0.5 text-sm text-[var(--lm-text-muted)]">
            What is running on this host, and what it is doing.
          </p>
        </div>
      </div>

      {update.data?.update_available ? (
        <div className="flex flex-wrap items-center gap-3 rounded-[var(--lm-radius-lg)] border border-[var(--lm-accent)]/40 bg-[var(--lm-accent-soft)] px-3 py-2">
          <ArrowUpCircle aria-hidden className="size-4 shrink-0 text-[var(--lm-accent)]" />
          <span className="min-w-0 flex-1 text-sm text-[var(--lm-text)]">
            Llama Man {update.data.latest_version} is available — this host runs{' '}
            <span className="lm-numeric">{update.data.current_version}</span>.
          </span>
          <Link to="/settings">
            <Button size="sm">Review the update</Button>
          </Link>
        </div>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,2fr)_minmax(18rem,1fr)]">
        <div className="space-y-4">
          <section className="space-y-3">
            <PanelHeader
              title="Instances"
              description={
                instanceRows.length
                  ? `${instanceRows.filter((row) => row.status.state === 'ready').length} of ${instanceRows.length} ready`
                  : 'Each one is a systemd unit of its own.'
              }
              actions={
                <Link to="/instances">
                  <Button size="sm" variant="ghost">
                    All instances
                  </Button>
                </Link>
              }
            />

            {instances.isPending ? (
              <LoadingPanel>Reading instances…</LoadingPanel>
            ) : instances.isError ? (
              // "No instances yet" on a host that has three is the worst thing this screen can
              // say: it is the landing view, and it is the answer a user acts on first.
              <QueryError
                title="The instance list could not be read"
                error={instances.error}
                onRetry={() => void instances.refetch()}
              />
            ) : instanceRows.length === 0 ? (
              <EmptyState
                icon={<Server />}
                title="No instances yet"
                description="An instance is a llama-server process with its own port, model and flags — and its own systemd unit, so it survives a Llama Man restart."
                action={
                  <Link to="/instances/new">
                    <Button variant="primary">Create an instance</Button>
                  </Link>
                }
              />
            ) : (
              <div className="grid gap-3 sm:grid-cols-2">
                {instanceRows.map((instance) => {
                  const model = instance.model_id ? modelsById.get(instance.model_id) : undefined;
                  return (
                    <InstanceCard
                      key={instance.id}
                      instance={instance}
                      {...(model ? { model } : {})}
                    />
                  );
                })}
              </div>
            )}
          </section>

          <ActiveJobs jobs={jobRows} />
          <RecentEvents events={eventRows} />
        </div>

        <div className="space-y-4">
          {/* Above llama.cpp deliberately: a card here is something that needs a person, and the
              answer to "is anything wrong" belongs before the answer to "what is installed". */}
          <Notifications />
          <GpuPanel />

          <Panel>
            <PanelHeader
              title="llama.cpp"
              description={
                version ? 'The build every instance executes out of.' : 'Nothing installed yet.'
              }
              actions={
                <Link
                  to="/llamacpp"
                  className="text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
                >
                  Versions
                </Link>
              }
            />
            {versions.isError ? (
              <p className="mt-3 text-xs text-[var(--lm-danger)]">
                The installed builds could not be read, so this panel is not showing what is active.
              </p>
            ) : version ? (
              <dl className="mt-3 grid grid-cols-2 gap-3">
                <Field label="Active" mono>
                  {version.build_tag ?? version.tag}
                </Field>
                <Field label="Backend" mono>
                  {version.backend}
                </Field>
                <Field label="Acquired" mono>
                  {version.acquisition}
                </Field>
                <Field label="Size" mono>
                  {formatBytes(version.size_bytes)}
                </Field>
                <Field label="Activated" mono>
                  {formatRelative(version.activated_at)}
                </Field>
                <Field label="In use by" mono>
                  {version.in_use_by?.length ?? 0}
                </Field>
              </dl>
            ) : (
              <div className="mt-3 flex items-center gap-2">
                <Cpu aria-hidden className="size-4 shrink-0 text-[var(--lm-text-faint)]" />
                <span className="text-xs text-[var(--lm-text-muted)]">
                  No active version. Instances cannot start until one is installed.
                </span>
              </div>
            )}
            {versionRows.length > 1 ? (
              <p className="mt-2 text-xs text-[var(--lm-text-faint)]">
                {versionRows.length} builds on disk.{' '}
                <Badge tone="neutral">
                  {formatBytes(versionRows.reduce((sum, row) => sum + (row.size_bytes ?? 0), 0))}
                </Badge>
              </p>
            ) : null}
          </Panel>

          <StoragePanel roots={rootRows} versions={versionRows} />
          <QuickActions hasInstances={instanceRows.length > 0} />
        </div>
      </div>
    </div>
  );
}
