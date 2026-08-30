/**
 * `/instances` — DESIGN section 4, screen 4.
 *
 * "Table: name, model, state, port, autostart, and the four derived badges (restart-required,
 * stale-version, inhibited, draft-unverified); the autostart toggle warns when it would leave a
 * running instance out of the next boot, or a stopped one in it (section 2.8)."
 *
 * The rows are the query the SSE `instances` topic patches, so a state change lands here without a
 * refetch and without a poll. VRAM and RAM come from `instance_status`, which the supervisor writes
 * — both are null until a process exists, and a null renders as "—" rather than as a zero that
 * would read like an answer.
 *
 * Filters, sort and `include_deleted` live in the URL, because a filtered table is a thing people
 * link to.
 */

import { useMemo } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { Plus, Server } from 'lucide-react';
import {
  Button,
  DataTable,
  EmptyState,
  Input,
  Mono,
  Panel,
  QueryError,
  Select,
  sortRows,
  StatusBadge,
  Switch,
  toast,
} from '../../components';
import type { Column, SortState } from '../../components';
import { formatBytes } from '../../format';
import type { Instance } from '../../api/types';
import { INSTANCE_STATES } from '../../api/types';
import { ApiError } from '../../api/errors';
import {
  isRouteMissing,
  useAutostart,
  useInstances,
  useModels,
} from '../../features/instances/api';
import { AutostartToggle } from '../../features/instances/components/AutostartToggle';
import { InstanceBadges } from '../../features/instances/components/InstanceBadges';

const STATE_OPTIONS = [
  { value: 'all', label: 'Every state' },
  ...INSTANCE_STATES.map((state) => ({ value: state, label: state })),
];

export function InstancesScreen() {
  const search = useSearch({ from: '/app/instances' });
  const navigate = useNavigate({ from: '/instances' });
  const instances = useInstances(search.include_deleted ?? false);
  const models = useModels();
  const autostart = useAutostart();

  const setSearch = (patch: Record<string, unknown>) => {
    void navigate({ search: (prev) => ({ ...prev, ...patch }), replace: true });
  };

  const modelName = (id: string | null | undefined): string => {
    if (!id) return '—';
    const model = models.data?.find((row) => row.id === id);
    return model ? `${model.repo_id.split('/').pop() ?? model.repo_id}` : id.slice(0, 8);
  };

  const columns: Column<Instance>[] = useMemo(
    () => [
      {
        id: 'name',
        header: 'Name',
        sortValue: (row) => row.name,
        cell: (row) => (
          <span className="flex min-w-0 flex-col">
            <span className="flex items-center gap-2">
              <Link
                to="/instances/$id"
                params={{ id: row.id }}
                className="truncate font-medium text-[var(--lm-text)] hover:text-[var(--lm-accent)]"
              >
                {row.name}
              </Link>
              {row.deleted_at ? <Mono>deleted</Mono> : null}
            </span>
            {row.display_name ? (
              <span className="truncate text-xs text-[var(--lm-text-muted)]">
                {row.display_name}
              </span>
            ) : null}
          </span>
        ),
      },
      {
        id: 'model',
        header: 'Model',
        sortValue: (row) => row.model_id ?? '',
        cell: (row) => <span className="truncate">{modelName(row.model_id)}</span>,
        secondary: true,
      },
      {
        id: 'state',
        header: 'State',
        sortValue: (row) => row.status.state,
        cell: (row) => (
          <span className="flex flex-wrap items-center gap-1.5">
            <StatusBadge
              kind="instance"
              state={row.status.state}
              {...(row.status.slots_total
                ? {
                    label: `${row.status.state === 'ready' ? 'Ready' : row.status.state} · ${
                      row.status.slots_busy ?? 0
                    }/${row.status.slots_total}`,
                  }
                : {})}
            />
            <InstanceBadges instance={row} />
          </span>
        ),
      },
      {
        id: 'ports',
        header: 'Ports',
        mono: true,
        sortValue: (row) => row.public_port,
        cell: (row) => (
          <span title={`public ${row.public_port} → internal ${row.internal_port}`}>
            {row.public_port} <span className="text-[var(--lm-text-faint)]">→</span>{' '}
            {row.internal_port}
          </span>
        ),
      },
      {
        id: 'vram',
        header: 'VRAM',
        align: 'right',
        mono: true,
        secondary: true,
        sortValue: (row) => row.status.vram_bytes ?? null,
        cell: (row) => formatBytes(row.status.vram_bytes),
      },
      {
        id: 'ram',
        header: 'RAM',
        align: 'right',
        mono: true,
        secondary: true,
        sortValue: (row) => row.status.rss_bytes ?? null,
        cell: (row) => formatBytes(row.status.rss_bytes),
      },
      {
        id: 'autostart',
        header: 'At boot',
        align: 'center',
        sortValue: (row) => row.autostart,
        cell: (row) => (
          <AutostartToggle
            enabled={row.autostart}
            state={row.status.state}
            pending={autostart.isPending && autostart.variables?.id === row.id}
            disabled={Boolean(row.deleted_at)}
            aria-label={`Start ${row.name} at boot`}
            onChange={(enabled) =>
              autostart.mutate(
                { id: row.id, enabled },
                {
                  onSuccess: (result) => {
                    if (result.hint === 'start_now') {
                      toast.info(`${row.name} is enabled for boot but is not running now.`);
                    }
                  },
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
        ),
      },
    ],
    // `modelName` closes over the model list; nothing else here is unstable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [models.data, autostart.isPending, autostart.variables?.id],
  );

  const rows = useMemo(() => {
    const all = instances.data ?? [];
    const q = search.q?.toLowerCase() ?? '';
    const filtered = all.filter((row) => {
      if (search.state && search.state !== 'all' && row.status.state !== search.state) return false;
      if (q === '') return true;
      return (
        row.name.toLowerCase().includes(q) ||
        (row.display_name ?? '').toLowerCase().includes(q) ||
        String(row.public_port).includes(q)
      );
    });
    const sort: SortState | null = search.sort
      ? { id: search.sort, desc: search.desc ?? false }
      : null;
    return sortRows(filtered, columns, sort);
  }, [instances.data, search.q, search.state, search.sort, search.desc, columns]);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Instances</h1>
        <Button
          variant="primary"
          icon={<Plus />}
          onClick={() => void navigate({ to: '/instances/new' })}
        >
          New instance
        </Button>
      </div>

      <Panel flush>
        <div className="flex flex-wrap items-center gap-2 border-b border-[var(--lm-border)] px-3 py-2">
          <Input
            value={search.q ?? ''}
            onChange={(event) => setSearch({ q: event.target.value || undefined })}
            placeholder="Filter by name or port"
            aria-label="Filter instances"
            className="w-56"
          />
          <Select
            value={search.state ?? 'all'}
            onValueChange={(value) => setSearch({ state: value === 'all' ? undefined : value })}
            options={STATE_OPTIONS}
            aria-label="Filter by state"
            className="w-44"
          />
          <div className="flex-1" />
          <label className="flex items-center gap-2 text-xs text-[var(--lm-text-muted)]">
            <Switch
              checked={search.include_deleted ?? false}
              onCheckedChange={(checked) => setSearch({ include_deleted: checked || undefined })}
              aria-label="Show deleted instances"
            />
            Show deleted
          </label>
        </div>

        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.id}
          loading={instances.isPending}
          sort={search.sort ? { id: search.sort, desc: search.desc ?? false } : null}
          onSortChange={(sort) =>
            setSearch({ sort: sort?.id, desc: sort?.desc ? true : undefined })
          }
          caption="Every instance this host manages"
          empty={
            instances.isError ? (
              // Neither "no instances yet" nor "nothing matches those filters" is true when the
              // read failed, and both send the user looking for a filter to clear.
              <QueryError
                title="The instance list could not be read"
                error={instances.error}
                onRetry={() => void instances.refetch()}
                dense
              />
            ) : instances.data?.length === 0 ? (
              <EmptyState
                icon={<Server />}
                title="No instances yet"
                description="An instance is one llama-server process: a model, a port, and the flags it runs with."
                action={
                  <Button
                    variant="primary"
                    icon={<Plus />}
                    onClick={() => void navigate({ to: '/instances/new' })}
                  >
                    New instance
                  </Button>
                }
              />
            ) : (
              <EmptyState
                dense
                title="Nothing matches those filters"
                description="Clear the search or pick a different state."
              />
            )
          }
        />
      </Panel>
    </div>
  );
}
