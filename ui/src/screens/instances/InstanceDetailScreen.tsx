/**
 * `/instances/:id` — DESIGN section 4, screen 6.
 *
 * "Status header, live journald log pane, slots table, metrics sparklines, token access list,
 * start/stop/restart/reset-failed/safe-start, start history, and the remediation card for the last
 * exit code (section 17)."
 *
 * Where each number comes from is the whole point of this screen, so it is said out loud in the UI
 * as well as here:
 *
 *  - The **header** reads the live row out of the list query, which the `instances` SSE topic
 *    patches. No poll, no second fetch.
 *  - The **command line** is the daemon's own rendering (`GET /instances/{id}/command`, falling back
 *    to the argv the detail read carries) — never a client-side reconstruction, because
 *    `RenderArgv` is one pure function in one package and a second one could disagree with what
 *    actually launches.
 *  - `requests_served` is **null**, not zero, when `--metrics` is off (section 2.9), and the tile
 *    says "metrics disabled" rather than showing a number nobody produced.
 */

import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { KeyRound, Pencil, Trash2 } from 'lucide-react';
import {
  Badge,
  Button,
  ConfirmDialog,
  DataTable,
  EmptyState,
  Field,
  LoadingPanel,
  Mono,
  Panel,
  PanelHeader,
  StatusBadge,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  toast,
} from '../../components';
import type { Column } from '../../components';
import { formatBytes, formatCount, formatRelative, formatTimestamp } from '../../format';
import { ApiError } from '../../api/errors';
import type { ApiToken } from '../../api/types';
import { useLogStream } from '../../events/useLogStream';
import {
  instanceLogStreamUrl,
  isRouteMissing,
  useDeleteInstance,
  useDetailFreshness,
  useFitEstimate,
  useInstanceCommand,
  useInstanceControl,
  useInstanceDetail,
  useInstanceLogPage,
  useInstanceRow,
  useInstanceSlots,
  useInstanceUsage,
  useModels,
  usePinNgl,
  useTokens,
} from '../../features/instances/api';
import type { ControlAction } from '../../features/instances/api';
import { ArgvPreview } from '../../features/instances/components/ArgvPreview';
import { AutostartToggle } from '../../features/instances/components/AutostartToggle';
import { InstanceActions } from '../../features/instances/components/InstanceActions';
import { InstanceBadges } from '../../features/instances/components/InstanceBadges';
import { LogPane } from '../../features/instances/components/LogPane';
import { NglAdvisoryCard } from '../../features/instances/components/NglAdvisoryCard';
import { RemediationCard } from '../../features/instances/components/RemediationCard';
import { StartsTable } from '../../features/instances/components/StartsTable';
import { UsageSparkline } from '../../features/instances/components/UsageSparkline';
import { useAutostart } from '../../features/instances/api';
import { remediationFor } from '../../features/instances/remediation';
import type { RemediationAction } from '../../features/instances/remediation';
import type { ServerSlot } from '../../features/instances/types';

const RUNNING = new Set(['starting', 'loading', 'ready', 'degraded']);

export function InstanceDetailScreen() {
  const { id } = useParams({ from: '/app/instances/$id' });
  const search = useSearch({ from: '/app/instances/$id' });
  const navigate = useNavigate();
  const tab = search.tab ?? 'overview';

  const row = useInstanceRow(id, true);
  const detail = useInstanceDetail(id);
  const models = useModels();
  const command = useInstanceCommand(id);
  const usage = useInstanceUsage(id);
  const tokens = useTokens();
  const control = useInstanceControl(id);
  const autostart = useAutostart();
  const remove = useDeleteInstance(id);
  useDetailFreshness(id, row);

  const [confirmDelete, setConfirmDelete] = useState<'soft' | 'purge' | null>(null);

  const instance = row ?? detail.data?.instance;
  const state = instance?.status.state ?? 'unknown';
  const running = RUNNING.has(state);

  const slots = useInstanceSlots(
    id,
    running && tab === 'slots',
    `${instance?.status.slots_busy ?? ''}:${instance?.status.last_change_at ?? ''}`,
  );
  const logPage = useInstanceLogPage(id, tab === 'logs');
  const log = useLogStream({ url: tab === 'logs' ? instanceLogStreamUrl(id) : null });
  // The paged read and the tail are concatenated rather than seeded into each other: the page can
  // land after the stream opens, and a seed only applies at mount.
  const logLines = useMemo(
    () => [...(logPage.data ?? []), ...log.lines],
    [logPage.data, log.lines],
  );

  const model = models.data?.find((entry) => entry.id === instance?.model_id);

  // D51's advisory: `auto` means llama.cpp decides, so the only honest number to show beside it is
  // the calculator's — and `pin-ngl` is the one click that turns it into a saved configuration.
  const nglMode = instance?.flags.n_gpu_layers?.mode ?? 'auto';
  const fit = useFitEstimate(
    instance && instance.model_id && nglMode === 'auto'
      ? {
          model_id: instance.model_id,
          flags: instance.flags as Record<string, unknown>,
          ...(instance.flags.device_uuids?.length ? { gpus: instance.flags.device_uuids } : {}),
        }
      : null,
  );
  const pinNgl = usePinNgl(id);

  const remediation = useMemo(() => {
    if (!instance) return null;
    const lastStart = detail.data?.starts?.[0];
    return remediationFor({
      errorCode: lastStart?.error_code ?? null,
      exitCode: instance.status.last_exit_code ?? null,
      inhibited: instance.inhibited,
      inhibitReason: instance.inhibit_reason ?? null,
      lastError: instance.status.last_error ?? null,
    });
  }, [instance, detail.data?.starts]);

  if (!instance) {
    return detail.isLoading ? (
      <LoadingPanel>Loading instance…</LoadingPanel>
    ) : (
      <EmptyState
        tone="error"
        title="No instance with that id"
        description="It may have been purged. Deleted instances keep their history and are listed with “Show deleted”."
        action={
          <Button onClick={() => void navigate({ to: '/instances' })}>Back to instances</Button>
        }
      />
    );
  }

  const controlsAvailable = !instance.deleted_at;

  const runAction = (action: ControlAction, drainSec?: number) => {
    control.mutate(
      { action, ...(drainSec === undefined ? {} : { drainSec }) },
      {
        onSuccess: (result) => {
          if (result.hint === 'will_start_at_boot') {
            toast.info('Stopped. Autostart is on, so it will come back at the next boot.');
          } else {
            toast.success(`${action.replace('-', ' ')} requested.`);
          }
        },
        onError: (error) => {
          if (isRouteMissing(error)) {
            toast.warn(`This daemon does not serve /instances/{id}/${action} yet.`, {
              description: `systemctl start ${instance.unit_name}`,
            });
            return;
          }
          const hints = error instanceof ApiError ? error.hints : [];
          toast.error(error, hints[0] ? { description: hints[0] } : {});
        },
      },
    );
  };

  const onRemediation = (action: RemediationAction) => {
    if (action === 'edit') return void navigate({ to: '/instances/$id/edit', params: { id } });
    if (action === 'models') return void navigate({ to: '/models' });
    if (action === 'llamacpp') return void navigate({ to: '/llamacpp' });
    runAction(action);
  };

  const usagePoints = (usage.data?.items ?? []).map((day) => ({
    label: day.day,
    value: day.requests,
  }));

  const scopedTokens = (tokens.data ?? []).filter(
    (token) => token.scope === 'all' || token.instance_ids.includes(id),
  );

  const tokenColumns: Column<ApiToken>[] = [
    { id: 'name', header: 'Token', cell: (token) => token.name },
    { id: 'prefix', header: 'Prefix', cell: (token) => <Mono>{token.prefix}…</Mono>, mono: true },
    {
      id: 'scope',
      header: 'Scope',
      cell: (token) =>
        token.scope === 'all' ? (
          <Badge tone="warn">every instance</Badge>
        ) : (
          <Badge>this instance</Badge>
        ),
    },
    {
      id: 'state',
      header: 'State',
      cell: (token) => <StatusBadge kind="token" state={token.state} />,
    },
    {
      id: 'used',
      header: 'Last used',
      cell: (token) => formatRelative(token.last_used_at),
      secondary: true,
    },
  ];

  const slotColumns: Column<ServerSlot>[] = [
    { id: 'id', header: 'Slot', cell: (slot) => slot.id, mono: true },
    {
      id: 'busy',
      header: 'State',
      cell: (slot) =>
        slot.is_processing ? (
          <Badge tone="info" dot pulse>
            Processing
          </Badge>
        ) : (
          <Badge>Idle</Badge>
        ),
    },
    {
      id: 'ctx',
      header: 'Context',
      cell: (slot) => (slot.n_ctx ? formatCount(slot.n_ctx) : '—'),
      mono: true,
      align: 'right',
    },
  ];

  return (
    <div className="space-y-4">
      {/* ------------------------------------------------------------ header */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="truncate text-lg font-semibold tracking-tight text-[var(--lm-text)]">
              {instance.display_name || instance.name}
            </h1>
            <StatusBadge kind="instance" state={state} />
            <InstanceBadges instance={instance} />
            {instance.deleted_at ? <Badge tone="neutral">deleted</Badge> : null}
          </div>
          <p className="mt-1 text-xs text-[var(--lm-text-muted)]">
            <Mono>{instance.unit_name}</Mono>
            {instance.description ? ` · ${instance.description}` : ''}
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <InstanceActions
            state={state}
            desiredState={instance.desired_state}
            autostart={instance.autostart}
            inhibited={instance.inhibited}
            busy={control.isPending}
            onAction={controlsAvailable ? runAction : null}
          />
          <Button
            icon={<Pencil />}
            onClick={() => void navigate({ to: '/instances/$id/edit', params: { id } })}
          >
            Edit
          </Button>
          <Button variant="danger" icon={<Trash2 />} onClick={() => setConfirmDelete('soft')}>
            Delete
          </Button>
        </div>
      </div>

      {/* ------------------------------------------------------------- facts */}
      <Panel>
        <dl className="grid gap-4 sm:grid-cols-3 lg:grid-cols-6">
          <Field label="Public port" mono>
            {instance.public_port}
          </Field>
          <Field label="Internal port" mono>
            {instance.internal_port}
          </Field>
          <Field label="Model">
            {model ? (
              <Link
                to="/models/$id"
                params={{ id: model.id }}
                className="hover:text-[var(--lm-accent)]"
              >
                {model.repo_id.split('/').pop() ?? model.repo_id}
              </Link>
            ) : (
              '—'
            )}
          </Field>
          <Field label="Slots" mono>
            {instance.status.slots_total
              ? `${instance.status.slots_busy ?? 0} / ${instance.status.slots_total}`
              : '—'}
          </Field>
          <Field label="VRAM" mono>
            {formatBytes(instance.status.vram_bytes)}
            {instance.status.gpu_attribution !== 'measured' ? (
              <span className="ml-1 text-[11px] text-[var(--lm-text-faint)]">
                ({instance.status.gpu_attribution})
              </span>
            ) : null}
          </Field>
          <Field label="RSS" mono>
            {formatBytes(instance.status.rss_bytes)}
          </Field>
          <Field label="Ready since">
            {instance.status.ready_at ? formatRelative(instance.status.ready_at) : '—'}
          </Field>
          <Field label="Requests served" mono>
            {instance.status.requests_served === null ||
            instance.status.requests_served === undefined ? (
              <span className="text-[var(--lm-text-faint)]">metrics disabled</span>
            ) : (
              formatCount(instance.status.requests_served)
            )}
          </Field>
          <Field label="Config hash" mono>
            <span title={instance.config_hash}>{instance.config_hash.slice(0, 12)}</span>
          </Field>
          <Field label="Applied hash" mono>
            <span title={instance.status.applied_config_hash ?? ''}>
              {instance.status.applied_config_hash?.slice(0, 12) ?? '—'}
            </span>
          </Field>
          <Field label="Main PID" mono>
            {instance.status.main_pid ?? '—'}
          </Field>
          <Field label="At boot">
            <AutostartToggle
              enabled={instance.autostart}
              state={state}
              pending={autostart.isPending}
              disabled={Boolean(instance.deleted_at)}
              aria-label={`Start ${instance.name} at boot`}
              onChange={(enabled) =>
                autostart.mutate(
                  { id, enabled },
                  {
                    onError: (error) => {
                      if (isRouteMissing(error)) {
                        toast.warn('This daemon does not serve the autostart endpoint yet.');
                        return;
                      }
                      const hints = error instanceof ApiError ? error.hints : [];
                      toast.error(error, hints[0] ? { description: hints[0] } : {});
                    },
                  },
                )
              }
            />
          </Field>
        </dl>
      </Panel>

      {remediation ? <RemediationCard remediation={remediation} onAction={onRemediation} /> : null}

      {/* -------------------------------------------------------------- tabs */}
      <Tabs
        value={tab}
        onValueChange={(next) =>
          void navigate({
            to: '/instances/$id',
            params: { id },
            search: { tab: next as typeof tab },
            replace: true,
          })
        }
      >
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="slots">Slots</TabsTrigger>
          <TabsTrigger value="tokens">Token access</TabsTrigger>
          <TabsTrigger value="starts">Start history</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="space-y-4">
          <NglAdvisoryCard
            ngl={instance.flags.n_gpu_layers}
            report={fit.data}
            loading={fit.isFetching}
            pinning={pinNgl.isPending}
            onPin={
              instance.deleted_at
                ? undefined
                : () =>
                    pinNgl.mutate(undefined, {
                      onSuccess: () =>
                        toast.success('Pinned. Restart to run with the pinned offload.'),
                      onError: (error) =>
                        isRouteMissing(error)
                          ? toast.warn('This daemon does not serve pin-ngl yet.', {
                              description:
                                'Set the offload to a fixed count in the editor instead.',
                            })
                          : toast.error(error),
                    })
            }
          />

          <ArgvPreview
            argv={command.data?.argv ?? detail.data?.argv}
            env={command.data?.env}
            unit={command.data?.unit ?? instance.unit_name}
            unknownFlags={command.data?.unknown_flags ?? detail.data?.unknown_flags ?? []}
            loading={detail.isLoading}
            unavailable={
              detail.data && detail.data.argv.length === 0
                ? 'No command line yet — a model and an active llama.cpp build have to resolve first.'
                : undefined
            }
          />

          <Panel>
            <PanelHeader
              title="Traffic"
              description="Daily requests through the gateway, counted for every proxied request including open instances."
            />
            <div className="mt-3">
              {usage.error && isRouteMissing(usage.error) ? (
                <p className="text-xs text-[var(--lm-text-faint)]">
                  This daemon does not serve <Mono>/instances/&#123;id&#125;/usage</Mono> yet.
                </p>
              ) : (
                <UsageSparkline points={usagePoints} unit="requests" />
              )}
            </div>
          </Panel>
        </TabsContent>

        <TabsContent value="logs">
          <LogPane lines={logLines} status={log.status} error={logPage.error ?? undefined} />
        </TabsContent>

        <TabsContent value="slots">
          <Panel flush>
            {!running ? (
              <EmptyState
                dense
                title="Not running"
                description="Slots come from llama-server’s own /slots endpoint, which only exists while the process does."
              />
            ) : slots.error ? (
              <EmptyState
                dense
                title="Slots unavailable"
                description={
                  isRouteMissing(slots.error)
                    ? 'This daemon does not proxy /slots yet.'
                    : 'llama-server did not answer. The --slots endpoint may be turned off for this instance.'
                }
              />
            ) : (
              <DataTable
                columns={slotColumns}
                rows={slots.data ?? []}
                rowKey={(slot) => String(slot.id)}
                loading={slots.isLoading}
                caption="Server slots"
              />
            )}
          </Panel>
        </TabsContent>

        <TabsContent value="tokens">
          <Panel flush>
            <div className="flex items-center justify-between gap-2 border-b border-[var(--lm-border)] px-3 py-2">
              <p className="text-xs text-[var(--lm-text-muted)]">
                {instance.auth_mode === 'none'
                  ? 'This instance is open: the gateway proxies without checking a token, and still counts the traffic.'
                  : 'Tokens whose scope reaches this instance.'}
              </p>
              <Button
                size="sm"
                icon={<KeyRound />}
                onClick={() => void navigate({ to: '/tokens' })}
              >
                Manage tokens
              </Button>
            </div>
            <DataTable
              columns={tokenColumns}
              rows={scopedTokens}
              rowKey={(token) => token.id}
              loading={tokens.isLoading}
              caption="Tokens scoped to this instance"
              empty={
                <EmptyState
                  dense
                  title="No token reaches this instance"
                  description="Create one on the Tokens screen and scope it here."
                />
              }
            />
          </Panel>
        </TabsContent>

        <TabsContent value="starts">
          <Panel flush>
            <StartsTable starts={detail.data?.starts ?? []} />
          </Panel>
        </TabsContent>
      </Tabs>

      {/* ------------------------------------------------------------ delete */}
      <ConfirmDialog
        open={confirmDelete === 'soft'}
        onOpenChange={(open) => setConfirmDelete(open ? 'soft' : null)}
        title={`Delete ${instance.name}?`}
        description="The unit is stopped and disabled, the gateway listener closes, and the name and both ports become free again."
        consequences="Every row is kept: the start history and the accounting stay reachable, and the instance appears under “Show deleted”."
        confirmLabel="Delete"
        busy={remove.isPending}
        onConfirm={() =>
          remove.mutate(
            {},
            {
              onSuccess: (result) => {
                setConfirmDelete(null);
                const hint = result?.hints?.[0];
                toast.success(
                  `${instance.name} deleted.`,
                  hint ? { description: hint, duration: null } : {},
                );
                void navigate({ to: '/instances' });
              },
              onError: (error) => toast.error(error),
            },
          )
        }
      />

      <ConfirmDialog
        open={confirmDelete === 'purge'}
        onOpenChange={(open) => setConfirmDelete(open ? 'purge' : null)}
        title={`Purge ${instance.name} and its history?`}
        description="This is the hard delete."
        consequences={
          <>
            The row and everything that hangs off it are removed: {detail.data?.starts.length ?? 0}{' '}
            start records, the daily usage and token accounting, and this instance’s token scopes.
            That history is the one thing in this system that cannot be recomputed.
          </>
        }
        confirmPhrase={instance.name}
        confirmLabel="Purge permanently"
        busy={remove.isPending}
        onConfirm={() =>
          remove.mutate(
            { purge: true },
            {
              onSuccess: () => {
                setConfirmDelete(null);
                toast.success(`${instance.name} purged.`);
                void navigate({ to: '/instances' });
              },
              onError: (error) => toast.error(error),
            },
          )
        }
      />

      {instance.deleted_at ? (
        <p className="text-xs text-[var(--lm-text-faint)]">
          Deleted {formatTimestamp(instance.deleted_at)}. Its history is retained indefinitely —{' '}
          <button
            type="button"
            className="underline underline-offset-4"
            onClick={() => setConfirmDelete('purge')}
          >
            purge it permanently
          </button>
          .
        </p>
      ) : null}
    </div>
  );
}
