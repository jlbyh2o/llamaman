/**
 * One row of the download queue — DESIGN section 4, screen 11.
 *
 * "The queue with per-file progress, speed and ETA, and pause / resume / cancel / reorder."
 *
 * Two things this row refuses to invent. First, the ETA and the speed are the daemon's:
 * section 7.4 computes an EWMA over a 1 Hz ticker and records `bytes_at_start` precisely so that a
 * resumed transfer's ETA is honest, and recomputing either from two frames here would produce a
 * worse number that disagreed with the one in the journal. Second, a sharded GGUF is *one* model
 * (section 7.3) — five files named `-00003-of-00005` are rolled up into a fraction, because a queue
 * that listed them as five transfers would misrepresent what is being downloaded.
 */

import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { ChevronDown, ChevronUp, Pause, Play, RotateCcw, X } from 'lucide-react';

import { Badge, Button, Panel, Progress, StatusBadge } from '../../components';
import { cn } from '../../components/cn';
import type { Download } from '../../api/types';
import {
  formatByteProgress,
  formatBytes,
  formatBytesPerSecond,
  formatCount,
  formatSeconds,
} from '../../format';
import { downloadControls, downloadPercent, rollupShards } from './downloads';

export interface DownloadRowProps {
  download: Download;
  onAction: (action: 'pause' | 'resume' | 'retry') => void;
  onCancel: () => void;
  /** Queue order. Undefined when the row is not reorderable — a finished transfer, say. */
  onReorder?: ((direction: 'up' | 'down') => void) | undefined;
  canMoveUp?: boolean;
  canMoveDown?: boolean;
  busy?: boolean;
}

export function DownloadRow({
  download,
  onAction,
  onCancel,
  onReorder,
  canMoveUp = false,
  canMoveDown = false,
  busy = false,
}: DownloadRowProps) {
  const [open, setOpen] = useState(false);
  const controls = downloadControls(download.state);
  const percent = downloadPercent(download);
  const rollups = rollupShards(download.files);
  const running = download.state === 'running';

  return (
    <Panel className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <Link
              to="/models/$id"
              params={{ id: download.model_id }}
              className="lm-numeric truncate text-[13px] font-medium text-[var(--lm-text)] underline-offset-4 hover:underline"
            >
              {download.repo_id}
            </Link>
            <StatusBadge kind="download" state={download.state} />
            {download.include_mmproj ? <Badge tone="neutral">+ projector</Badge> : null}
            {download.attempts > 1 ? (
              <Badge tone="neutral" title="Transfers that were retried after a network failure.">
                attempt {formatCount(download.attempts)}
              </Badge>
            ) : null}
          </div>
          <p className="lm-numeric mt-0.5 truncate text-xs text-[var(--lm-text-muted)]">
            {download.primary_file}
          </p>
        </div>

        <div className="flex shrink-0 items-center gap-1.5">
          {onReorder ? (
            <>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Move up the queue"
                disabled={busy || !canMoveUp}
                onClick={() => onReorder('up')}
              >
                <ChevronUp aria-hidden />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                aria-label="Move down the queue"
                disabled={busy || !canMoveDown}
                onClick={() => onReorder('down')}
              >
                <ChevronDown aria-hidden />
              </Button>
            </>
          ) : null}
          {controls.pause ? (
            <Button size="sm" icon={<Pause />} disabled={busy} onClick={() => onAction('pause')}>
              Pause
            </Button>
          ) : null}
          {controls.resume ? (
            <Button size="sm" icon={<Play />} disabled={busy} onClick={() => onAction('resume')}>
              Resume
            </Button>
          ) : null}
          {controls.retry ? (
            <Button
              size="sm"
              icon={<RotateCcw />}
              disabled={busy}
              onClick={() => onAction('retry')}
            >
              Retry
            </Button>
          ) : null}
          {controls.cancel ? (
            <Button size="sm" variant="danger" icon={<X />} disabled={busy} onClick={onCancel}>
              Cancel
            </Button>
          ) : null}
        </div>
      </div>

      <Progress
        value={percent}
        tone={
          download.state === 'failed'
            ? 'danger'
            : download.state === 'paused'
              ? 'warn'
              : download.state === 'succeeded'
                ? 'ok'
                : 'accent'
        }
        label={formatByteProgress(download.bytes_done, download.bytes_total)}
        detail={
          running
            ? `${formatBytesPerSecond(download.speed_bps)} · ${formatSeconds(download.eta_sec, { placeholder: 'estimating' })} left`
            : download.state === 'succeeded'
              ? 'complete'
              : download.state
        }
        aria-label={`${download.repo_id} download progress`}
      />

      {download.error_message ? (
        <p
          role="alert"
          className="rounded-[var(--lm-radius)] border border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)] px-3 py-2 text-xs text-[var(--lm-text)]"
        >
          <span className="lm-numeric text-[var(--lm-danger)]">
            {download.error_code ?? 'failed'}
          </span>{' '}
          — {download.error_message}
        </p>
      ) : null}

      <div>
        <button
          type="button"
          onClick={() => setOpen((value) => !value)}
          aria-expanded={open}
          className="text-xs text-[var(--lm-text-muted)] underline-offset-4 hover:text-[var(--lm-text)] hover:underline"
        >
          {open ? 'Hide' : 'Show'} {formatCount(download.files.length)}{' '}
          {download.files.length === 1 ? 'file' : 'files'}
        </button>

        {open ? (
          <ul className="mt-2 space-y-2">
            {rollups.map((rollup) => (
              <li key={rollup.key} className="space-y-1">
                <div className="flex items-baseline justify-between gap-3 text-xs">
                  <span className="lm-numeric truncate text-[var(--lm-text-muted)]">
                    {rollup.key}
                  </span>
                  <span className="lm-numeric shrink-0 text-[var(--lm-text-faint)]">
                    {rollup.shardTotal > 1
                      ? `${formatCount(rollup.done)} of ${formatCount(rollup.shardTotal)} shards · `
                      : ''}
                    {formatBytes(rollup.bytesDone)} / {formatBytes(rollup.bytesTotal)}
                  </span>
                </div>

                <ul className="space-y-1 pl-3">
                  {rollup.files.map((file) => {
                    const filePercent =
                      file.bytes_total > 0 ? (file.bytes_done / file.bytes_total) * 100 : null;
                    return (
                      <li key={file.id} className="flex items-center gap-2">
                        <span
                          className={cn(
                            'lm-numeric w-16 shrink-0 text-[11px]',
                            file.state === 'failed'
                              ? 'text-[var(--lm-danger)]'
                              : 'text-[var(--lm-text-faint)]',
                          )}
                        >
                          {rollup.shardTotal > 1
                            ? `${file.shard_index}/${file.shard_total}`
                            : file.state}
                        </span>
                        <Progress
                          value={filePercent}
                          size="sm"
                          tone={file.state === 'failed' ? 'danger' : 'accent'}
                          aria-label={`${file.filename} progress`}
                        />
                        <span className="lm-numeric w-20 shrink-0 text-right text-[11px] text-[var(--lm-text-faint)]">
                          {formatBytes(file.bytes_total)}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              </li>
            ))}
          </ul>
        ) : null}
      </div>
    </Panel>
  );
}
