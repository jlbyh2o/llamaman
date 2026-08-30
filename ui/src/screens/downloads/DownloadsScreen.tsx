/**
 * The download queue — DESIGN section 4, screen 11.
 *
 * "Queue with per-file progress, speed/ETA, pause/resume/cancel/reorder."
 *
 * This screen has no timer in it. The downloader pushes an SSE patch every second (section 7.4) and
 * `applyFrame` merges it into this query's cache by entity id, so the bars move because the daemon
 * said so — `useLiveQueryOptions()` starts an interval only after the stream has actually given up,
 * which is exactly the fallback section 4 describes and not a polling loop.
 *
 * Reorder is a `PATCH` of `downloads.priority`, which moves the `jobs` row with it: the worker pool
 * leases in `(priority, created_at)` order, so the queue the user sees and the order the pool takes
 * are the same list rather than two views that can disagree.
 */

import { useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { Download } from 'lucide-react';

import { Button, ConfirmDialog, EmptyState, Panel, Spinner, Switch, toast } from '../../components';
import type { Download as DownloadRecord } from '../../api/types';
import { formatBytes, formatBytesPerSecond, formatCount } from '../../format';
import { DownloadRow } from '../../features/models/DownloadRow';
import { LinkIcon, linkButtonClass } from '../../features/models/LinkButton';
import {
  isLive,
  useCancelDownload,
  useDownloadAction,
  useDownloads,
  useReorderDownload,
} from '../../features/models/downloads';

export function DownloadsScreen() {
  const search = useSearch({ from: '/app/downloads' });
  const navigate = useNavigate({ from: '/downloads' });

  const state = search.state ?? 'active';
  const downloads = useDownloads(state);
  const action = useDownloadAction();
  const cancel = useCancelDownload();
  const reorder = useReorderDownload();

  const [doomed, setDoomed] = useState<DownloadRecord | null>(null);
  const [keepPartial, setKeepPartial] = useState(true);

  const items = downloads.data?.items ?? [];
  const live = items.filter(isLive);
  const running = items.filter((item) => item.state === 'running');

  const totalSpeed = running.reduce((sum, item) => sum + item.speed_bps, 0);
  const remaining = live.reduce(
    (sum, item) => sum + Math.max(0, item.bytes_total - item.bytes_done),
    0,
  );

  /**
   * Queue order is `(priority, created_at)` ascending, so "up" means a smaller number. Rather than
   * renumbering the whole list, a move takes the neighbor's priority and steps one past it — which
   * is enough to swap two adjacent rows and leaves every other row untouched.
   */
  const move = (item: DownloadRecord, direction: 'up' | 'down') => {
    const queue = live;
    const index = queue.findIndex((candidate) => candidate.id === item.id);
    const neighbor = queue[direction === 'up' ? index - 1 : index + 1];
    if (!neighbor) return;
    const priority =
      neighbor.priority === item.priority
        ? item.priority + (direction === 'up' ? -1 : 1)
        : neighbor.priority;
    reorder.mutate({ id: item.id, priority }, { onError: (error) => toast.error(error) });
  };

  const busy = action.isPending || cancel.isPending || reorder.isPending;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Downloads</h1>
          <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">
            {live.length > 0
              ? `${formatCount(live.length)} in the queue · ${formatBytes(remaining)} remaining${
                  totalSpeed > 0 ? ` · ${formatBytesPerSecond(totalSpeed)}` : ''
                }`
              : 'Nothing is transferring.'}
          </p>
        </div>

        <div className="flex items-center gap-2">
          <div className="flex rounded-[var(--lm-radius)] border border-[var(--lm-border)] p-0.5">
            {(['active', 'all'] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={state === value}
                onClick={() =>
                  void navigate({
                    search: (prev) => ({
                      ...prev,
                      state: value === 'active' ? undefined : value,
                    }),
                  })
                }
                className={
                  state === value
                    ? 'rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-raised)] px-3 py-1 text-sm font-medium text-[var(--lm-text)]'
                    : 'rounded-[var(--lm-radius-sm)] px-3 py-1 text-sm text-[var(--lm-text-muted)] hover:text-[var(--lm-text)]'
                }
              >
                {value === 'active' ? 'Active' : 'Everything'}
              </button>
            ))}
          </div>
          <Link to="/models/browse" className={linkButtonClass('primary')}>
            <LinkIcon>
              <Download />
            </LinkIcon>
            Browse Hugging Face
          </Link>
        </div>
      </div>

      {downloads.isPending ? (
        <div className="flex justify-center py-16">
          <Spinner label="Reading the queue" />
        </div>
      ) : downloads.error ? (
        <EmptyState
          tone="error"
          title="The queue could not be read"
          description={(downloads.error as Error).message}
          action={<Button onClick={() => void downloads.refetch()}>Try again</Button>}
        />
      ) : items.length === 0 ? (
        <EmptyState
          icon={<Download />}
          title={state === 'active' ? 'Nothing is downloading' : 'Nothing has been downloaded'}
          description={
            state === 'active'
              ? 'Finished transfers are kept as a receipt — switch to “Everything” to see them.'
              : 'Pick a quantization on a repository page and its fit verdict will tell you whether it will run before you spend the bandwidth.'
          }
          action={
            <Link to="/models/browse" className={linkButtonClass('primary')}>
              Browse Hugging Face
            </Link>
          }
        />
      ) : (
        <ul className="space-y-3">
          {items.map((item) => {
            const index = live.findIndex((candidate) => candidate.id === item.id);
            const reorderable = index !== -1 && live.length > 1;
            return (
              <li key={item.id}>
                <DownloadRow
                  download={item}
                  busy={busy}
                  onAction={(next) =>
                    action.mutate(
                      { id: item.id, action: next },
                      { onError: (error) => toast.error(error) },
                    )
                  }
                  onCancel={() => {
                    setKeepPartial(true);
                    setDoomed(item);
                  }}
                  onReorder={reorderable ? (direction) => move(item, direction) : undefined}
                  canMoveUp={reorderable && index > 0}
                  canMoveDown={reorderable && index < live.length - 1}
                />
              </li>
            );
          })}
        </ul>
      )}

      {state === 'active' && items.length > 0 ? (
        <Panel>
          <p className="text-xs text-[var(--lm-text-muted)]">
            Transfers resume from where they stopped: a paused or canceled download keeps its
            partial files, and the whole-file SHA-256 is verified before anything is linked into the
            cache.
          </p>
        </Panel>
      ) : null}

      <ConfirmDialog
        open={doomed !== null}
        onOpenChange={(open) => {
          if (!open) setDoomed(null);
        }}
        title="Cancel this download"
        description={doomed?.repo_id}
        confirmLabel="Cancel download"
        cancelLabel="Keep downloading"
        busy={cancel.isPending}
        consequences={
          doomed ? (
            <div className="space-y-2">
              <p>
                {formatBytes(doomed.bytes_done)} of {formatBytes(doomed.bytes_total)} has already
                been transferred.
              </p>
              <label className="flex items-center justify-between gap-3">
                <span>
                  Keep the partial files
                  <span className="mt-0.5 block text-[var(--lm-text-muted)]">
                    A retry resumes from here instead of starting the transfer again.
                  </span>
                </span>
                <Switch
                  checked={keepPartial}
                  onCheckedChange={setKeepPartial}
                  aria-label="Keep the partial files"
                />
              </label>
            </div>
          ) : undefined
        }
        onConfirm={() => {
          if (!doomed) return;
          cancel.mutate(
            { id: doomed.id, keepPartial },
            {
              onSuccess: () => {
                toast.info('Download canceled', {
                  description: keepPartial
                    ? 'The partial files were kept, so a retry will resume.'
                    : 'The partial files were removed.',
                });
                setDoomed(null);
              },
              onError: (error) => toast.error(error),
            },
          );
        }}
      />
    </div>
  );
}
