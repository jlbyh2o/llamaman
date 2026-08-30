/**
 * The local model library — DESIGN section 4, screen 7.
 *
 * "Local library grouped by repo: quant chips, size, in-use badges, missing/corrupt states, delete
 * with the free-space preview."
 *
 * The organizing fact, from SPEC section 3.2, is that this is **not** a private directory: models
 * live in the standard Hugging Face cache, so this screen is a view of a shared filesystem that the
 * `hf` CLI and any other HF-aware tool also write to. That is why it leads with the cache roots and
 * their free space, why "Rescan" is a first-class action rather than a debug tool, and why a row can
 * be `missing` — a disk can be unplugged, and a scan says so instead of deleting the row.
 *
 * Grouping is by repository because that is how the cache stores it: one `models--org--name`
 * directory, several snapshots, blobs shared between them. It is also why the delete dialog has to
 * refcount rather than subtract (D28).
 */

import { useMemo, useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { Boxes, FolderSearch, HardDrive, RefreshCw, Search, Trash2 } from 'lucide-react';

import {
  Badge,
  Button,
  EmptyState,
  Input,
  Meter,
  Panel,
  PanelHeader,
  Select,
  Spinner,
  StatusBadge,
  stateStyle,
  toast,
} from '../../components';
import { cn } from '../../components/cn';
import type { Model } from '../../api/types';
import { formatBytes, formatCount, formatRelative } from '../../format';
import {
  KIND_LABELS,
  MODEL_FILTER_STATES,
  MODEL_KINDS,
  MODEL_SORTS,
  modelKind,
  modelSort,
  modelState,
} from '../../features/models/api';
import { DeleteModelDialog } from '../../features/models/DeleteModelDialog';
import { LinkIcon, linkButtonClass } from '../../features/models/LinkButton';
import {
  useCacheRoots,
  useDismissStray,
  useModels,
  useScanCache,
  useStrays,
} from '../../features/models/queries';

const SORT_LABELS: Record<(typeof MODEL_SORTS)[number], string> = {
  repo: 'Repository name',
  size: 'Largest first',
  recent: 'Recently added',
};

export function ModelsScreen() {
  const search = useSearch({ from: '/app/models' });
  const navigate = useNavigate({ from: '/models' });

  const sort = modelSort(search.sort) ?? 'repo';
  const models = useModels({
    ...(search.q === undefined ? {} : { q: search.q }),
    ...(modelKind(search.kind) === undefined ? {} : { kind: modelKind(search.kind) }),
    ...(modelState(search.state) === undefined ? {} : { state: modelState(search.state) }),
    sort,
  });
  const roots = useCacheRoots();
  const strays = useStrays();
  const scan = useScanCache();

  const [doomed, setDoomed] = useState<Model | null>(null);

  const items = models.data?.items ?? [];

  /** Repositories, in the order the server sorted their models, reversed when `desc` is set. */
  const repos = useMemo(() => {
    const byRepo = new Map<string, Model[]>();
    for (const model of items) {
      const list = byRepo.get(model.repo_id);
      if (list) list.push(model);
      else byRepo.set(model.repo_id, [model]);
    }
    const ordered = [...byRepo.entries()].map(([repo, list]) => ({
      repo,
      models: list,
      bytes: list.reduce((sum, model) => sum + model.bytes_on_disk, 0),
    }));
    return search.desc ? ordered.reverse() : ordered;
  }, [items, search.desc]);

  const totalBytes = items.reduce((sum, model) => sum + model.bytes_on_disk, 0);
  const visibleStrays = (strays.data?.items ?? []).filter((stray) => !stray.dismissed_at);

  const rescan = () => {
    scan.mutate(undefined, {
      onSuccess: () =>
        toast.info('Scanning the cache', {
          description: 'Models already on disk are reconciled against the catalog.',
        }),
      onError: (error) => toast.error(error),
    });
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">
            Model library
          </h1>
          <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">
            {formatCount(items.length)} models · {formatBytes(totalBytes)} in the Hugging Face cache
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button icon={<RefreshCw />} loading={scan.isPending} onClick={rescan}>
            Rescan cache
          </Button>
          <Link to="/models/browse" className={linkButtonClass('primary')}>
            <LinkIcon>
              <Search />
            </LinkIcon>
            Browse Hugging Face
          </Link>
        </div>
      </div>

      {/* The cache roots, because this library is a view of a directory other tools also use. */}
      {(roots.data?.items.length ?? 0) > 0 ? (
        <Panel className="space-y-3">
          <PanelHeader
            title="Cache roots"
            description="The standard Hugging Face hub layout. Downloads land in the primary root; every other root is scanned and served read-only."
          />
          <ul className="space-y-3">
            {(roots.data?.items ?? []).map((root) => (
              <li key={root.id}>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="lm-numeric flex items-center gap-2 text-[13px] text-[var(--lm-text)]">
                    <HardDrive aria-hidden className="size-3.5 text-[var(--lm-text-faint)]" />
                    {root.path}
                    {root.is_primary ? <Badge tone="accent">primary</Badge> : null}
                    {!root.writable ? <Badge tone="neutral">read-only</Badge> : null}
                    {!root.symlinks_ok ? (
                      <Badge tone="warn" title="This filesystem does not support symlinks.">
                        no symlinks
                      </Badge>
                    ) : null}
                  </span>
                  <span className="lm-numeric text-xs text-[var(--lm-text-muted)]">
                    {formatCount(root.models)} models · {formatBytes(root.bytes_on_disk)}
                    {root.last_scan_at ? ` · scanned ${formatRelative(root.last_scan_at)}` : ''}
                  </span>
                </div>
                {root.total_bytes && root.free_bytes !== null && root.free_bytes !== undefined ? (
                  <Meter
                    className="mt-1"
                    used={root.total_bytes - root.free_bytes}
                    total={root.total_bytes}
                    label={`${root.fs_type ?? 'filesystem'}`}
                    detail={`${formatBytes(root.free_bytes)} free of ${formatBytes(root.total_bytes)}`}
                  />
                ) : null}
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}

      <Panel>
        <div className="grid gap-3 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)]">
          <label className="block">
            <span className="sr-only">Filter</span>
            <Input
              value={search.q ?? ''}
              onChange={(event) =>
                void navigate({
                  search: (prev) => ({
                    ...prev,
                    q: event.target.value === '' ? undefined : event.target.value,
                  }),
                  replace: true,
                })
              }
              placeholder="Filter by repository or file…"
              aria-label="Filter models"
            />
          </label>

          <Select
            value={modelKind(search.kind) ?? 'any'}
            onValueChange={(value) =>
              void navigate({
                search: (prev) => ({ ...prev, kind: value === 'any' ? undefined : value }),
              })
            }
            aria-label="Kind"
            options={[
              { value: 'any', label: 'Any kind' },
              ...MODEL_KINDS.map((kind) => ({ value: kind, label: KIND_LABELS[kind] ?? kind })),
            ]}
          />

          <Select
            value={modelState(search.state) ?? 'any'}
            onValueChange={(value) =>
              void navigate({
                search: (prev) => ({ ...prev, state: value === 'any' ? undefined : value }),
              })
            }
            aria-label="State"
            options={[
              { value: 'any', label: 'Any state' },
              // `deleted` is deliberately not offered: `GET /models` excludes those rows unless
              // `include_deleted=true`, so the filter would be a control that always finds nothing.
              // Deleting keeps the row as history (section 7.2), and history is the events screen.
              ...MODEL_FILTER_STATES.filter((state) => state !== 'deleted').map((state) => ({
                value: state,
                // The same labels the badges use — nothing else in this app names a state.
                label: stateStyle('model', state).label,
              })),
            ]}
          />

          <Select
            value={sort}
            onValueChange={(value) =>
              void navigate({
                search: (prev) => ({ ...prev, sort: value === 'repo' ? undefined : value }),
              })
            }
            aria-label="Sort"
            options={MODEL_SORTS.map((value) => ({ value, label: SORT_LABELS[value] }))}
          />
        </div>
      </Panel>

      {models.isPending ? (
        <div className="flex justify-center py-16">
          <Spinner label="Reading the catalog" />
        </div>
      ) : models.error ? (
        <EmptyState
          tone="error"
          title="The catalog could not be read"
          description={(models.error as Error).message}
          action={<Button onClick={() => void models.refetch()}>Try again</Button>}
        />
      ) : repos.length === 0 ? (
        <EmptyState
          icon={<Boxes />}
          title={search.q || search.kind || search.state ? 'Nothing matches' : 'No models yet'}
          description={
            search.q || search.kind || search.state
              ? 'Clear the filters to see the whole library.'
              : 'Download one from the Hub, or point Llama Man at a cache directory that already holds some and rescan.'
          }
          action={
            <Link to="/models/browse" className={linkButtonClass('primary')}>
              Browse Hugging Face
            </Link>
          }
          secondaryAction={
            <Button onClick={rescan} loading={scan.isPending}>
              Rescan cache
            </Button>
          }
        />
      ) : (
        <ul className="space-y-3">
          {repos.map((group) => (
            <li key={group.repo}>
              <Panel className="space-y-2">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <Link
                    to="/models/browse/$"
                    params={{ _splat: group.repo }}
                    className="lm-numeric truncate text-[13px] font-medium text-[var(--lm-text)] underline-offset-4 hover:underline"
                  >
                    {group.repo}
                  </Link>
                  <span className="lm-numeric text-xs text-[var(--lm-text-muted)]">
                    {formatCount(group.models.length)}{' '}
                    {group.models.length === 1 ? 'quantization' : 'quantizations'} ·{' '}
                    {formatBytes(group.bytes)}
                  </span>
                </div>

                <ul className="divide-y divide-[var(--lm-border)]">
                  {group.models.map((model) => (
                    <li key={model.id}>
                      <ModelRow model={model} onDelete={() => setDoomed(model)} />
                    </li>
                  ))}
                </ul>
              </Panel>
            </li>
          ))}
        </ul>
      )}

      {visibleStrays.length > 0 ? (
        <Panel className="space-y-3">
          <PanelHeader
            title="Stray files"
            description="GGUF files in a cache root that belong to no model here. They may be another tool's — nothing is removed unless you say so."
          />
          <ul className="space-y-1">
            {visibleStrays.map((stray) => (
              <li
                key={stray.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] px-3 py-2"
              >
                <span className="lm-numeric min-w-0 flex-1 truncate text-[12px] text-[var(--lm-text-muted)]">
                  {stray.path}
                </span>
                <Badge tone="neutral">{stray.reason}</Badge>
                <span className="lm-numeric text-xs text-[var(--lm-text-faint)]">
                  {formatBytes(stray.size_bytes)}
                </span>
                <StrayActions id={stray.id} />
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}

      <DeleteModelDialog
        open={doomed !== null}
        onOpenChange={(open) => {
          if (!open) setDoomed(null);
        }}
        model={doomed}
      />
    </div>
  );
}

function ModelRow({ model, onDelete }: { model: Model; onDelete: () => void }) {
  const inUse = model.in_use_by.filter((ref) => !ref.deleted);

  return (
    <div className="flex flex-wrap items-center gap-3 py-2">
      <Link
        to="/models/$id"
        params={{ id: model.id }}
        className="lm-numeric min-w-0 flex-1 truncate text-[13px] text-[var(--lm-text)] underline-offset-4 hover:underline"
      >
        {model.quant_label ?? model.primary_file}
      </Link>

      <div className="flex flex-wrap items-center gap-1.5">
        <StatusBadge kind="model" state={model.state} />
        {model.kind !== 'text' ? (
          <Badge tone="neutral">{KIND_LABELS[model.kind] ?? model.kind}</Badge>
        ) : null}
        {model.shard_count > 1 ? (
          <Badge tone="neutral">{formatCount(model.shard_count)} shards</Badge>
        ) : null}
        {model.origin === 'scanned' ? (
          <Badge tone="neutral" title="Found by a cache scan rather than downloaded here.">
            scanned
          </Badge>
        ) : null}
        {inUse.map((ref) => (
          <Link key={ref.id} to="/instances/$id" params={{ id: ref.id }}>
            <Badge tone="accent" title={`Used as the ${ref.role} model`}>
              {ref.name}
            </Badge>
          </Link>
        ))}
      </div>

      <span className="lm-numeric w-24 shrink-0 text-right text-xs text-[var(--lm-text-muted)]">
        {formatBytes(model.bytes_on_disk)}
      </span>

      <Button
        size="icon"
        variant="ghost"
        aria-label={`Delete ${model.quant_label ?? model.primary_file}`}
        onClick={onDelete}
        className={cn(inUse.length > 0 && 'opacity-60')}
      >
        <Trash2 aria-hidden />
      </Button>
    </div>
  );
}

function StrayActions({ id }: { id: string }) {
  const dismiss = useDismissStray();
  return (
    <Button
      size="sm"
      variant="ghost"
      icon={<FolderSearch />}
      loading={dismiss.isPending}
      onClick={() => dismiss.mutate(id, { onError: (error) => toast.error(error) })}
    >
      Dismiss
    </Button>
  );
}
