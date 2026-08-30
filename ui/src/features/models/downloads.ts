/**
 * The download queue — DESIGN section 3.8, section 7.4.
 *
 * A download is three rows written in one transaction (section 2.7): a `jobs` row that carries
 * scheduling, a `downloads` row that carries domain state, and one `download_tasks` row per file
 * folded upward into it. The UI only ever reads the middle one plus its files, because that fold is
 * the daemon's single writer and re-deriving it here would be a second, disagreeing opinion.
 *
 * Freshness comes from the `downloads` SSE topic: the worker pushes a patch every second, and
 * `applyFrame` merges it into both the detail query and every cached list. So there is no polling
 * here — `useLiveQueryOptions()` only starts an interval once the stream has actually given up.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type { Download, DownloadFile, ListPage } from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { asPage } from './api';

/** `GET /api/v1/downloads` — `active` is everything unfinished, `all` is the whole ledger. */
export function useDownloads(
  state: 'active' | 'all' = 'active',
): UseQueryResult<ListPage<Download>, Error> {
  const live = useLiveQueryOptions(2_000);
  return useQuery({
    queryKey: queryKeys.downloads.list({ state }),
    queryFn: async () => asPage<Download>(await api.get('/api/v1/downloads', { query: { state } })),
    ...live,
  });
}

export interface CreateDownloadVars {
  repo_id: string;
  files: string[];
  revision?: string;
  include_mmproj: boolean;
  mmproj_file?: string;
  kind?: string;
  priority?: number;
}

/**
 * `POST /api/v1/downloads` — `202 {job_id, subject, model_id, download_id, bytes_total}`.
 *
 * The `Idempotency-Key` is not optional politeness here. This is the one button in the models area
 * that a double click would otherwise turn into two transfers of the same twenty gigabytes; with a
 * key, the replay inside the ten-minute window returns the original receipt with `200` (D65).
 */
export function useCreateDownload() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (vars: CreateDownloadVars) => {
      const { repo_id, files, revision, include_mmproj, mmproj_file, kind, priority } = vars;
      const body = {
        repo_id,
        files: [...files].sort(),
        include_mmproj,
        ...(revision === undefined ? {} : { revision }),
        ...(mmproj_file === undefined ? {} : { mmproj_file }),
        ...(kind === undefined ? {} : { kind }),
        ...(priority === undefined ? {} : { priority }),
      };
      return api.post('/api/v1/downloads', { body, idempotencyKey: downloadKey(body) });
    },
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('downloads') });
      await client.invalidateQueries({ queryKey: queryKeys.family('models') });
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

/**
 * A stable key for one request.
 *
 * It is derived from the **whole body**, not from a hand-picked subset, and that is the point:
 * section 3's rule is that the same key with a *different* body inside the ten-minute window is
 * `422 idempotency_key_reused`. Keying on "repository plus file set" alone would mean that starting
 * a download, canceling it, and restarting it at a different queue priority sent the same key with
 * a changed body — and the retry would be rejected as a replay of something it is not. Two clicks
 * on an unchanged dialog still produce one key, which is the collapse this exists for.
 */
export function downloadKey(body: Record<string, unknown>): string {
  // Key order must not affect the hash: the body is built with conditional spreads, so a projector
  // choice can move `mmproj_file` ahead of `kind` without the request having changed at all.
  const canonical = Object.keys(body)
    .sort()
    .map((key) => `${key}=${JSON.stringify(body[key])}`)
    .join('&');
  // The header must be a short printable ASCII token (section 3, `400` otherwise), so the body is
  // hashed rather than sent verbatim — a repo id and a file list are neither short nor ASCII-safe.
  return `dl-${hash32(canonical)}`;
}

/** FNV-1a, rendered hex. Not a security primitive: this only has to be stable and short. */
function hash32(input: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < input.length; i += 1) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, '0');
}

type DownloadAction = 'pause' | 'resume' | 'retry';

/** `POST /api/v1/downloads/{id}/{pause|resume|retry}` — each answers `202` with the fresh row. */
export function useDownloadAction() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (vars: { id: string; action: DownloadAction }) => {
      const { id, action } = vars;
      if (action === 'pause') return api.post('/api/v1/downloads/{id}/pause', { path: { id } });
      if (action === 'resume') return api.post('/api/v1/downloads/{id}/resume', { path: { id } });
      return api.post('/api/v1/downloads/{id}/retry', { path: { id } });
    },
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('downloads') });
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

/**
 * `POST /api/v1/downloads/{id}/cancel?keep_partial=` — the default keeps the `.incomplete` files,
 * so a later retry resumes from where this stopped rather than starting the transfer again.
 */
export function useCancelDownload() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; keepPartial: boolean }) =>
      api.post('/api/v1/downloads/{id}/cancel', {
        path: { id: vars.id },
        query: { keep_partial: vars.keepPartial },
      }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('downloads') });
      await client.invalidateQueries({ queryKey: queryKeys.family('models') });
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

/**
 * `PATCH /api/v1/downloads/{id}` — queue order.
 *
 * Priority is ascending: the worker pool leases in `(priority, created_at)` order, so a *lower*
 * number runs sooner. The screen says "move up" and does the arithmetic, because nobody should have
 * to know that.
 */
export function useReorderDownload() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; priority: number }) =>
      api.patch('/api/v1/downloads/{id}', {
        path: { id: vars.id },
        body: { priority: vars.priority },
      }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('downloads') });
    },
  });
}

/* -- reading a download ---------------------------------------------------- */

/** States in which a transfer is still going somewhere. */
export const LIVE_DOWNLOAD_STATES: ReadonlySet<string> = new Set([
  'queued',
  'resolving',
  'running',
  'paused',
  'verifying',
]);

export function isLive(download: Download): boolean {
  return LIVE_DOWNLOAD_STATES.has(download.state);
}

/** Which of pause / resume / retry / cancel this state actually admits. */
export function downloadControls(state: string): {
  pause: boolean;
  resume: boolean;
  retry: boolean;
  cancel: boolean;
} {
  return {
    pause: state === 'running' || state === 'queued' || state === 'resolving',
    resume: state === 'paused',
    retry: state === 'failed' || state === 'canceled',
    cancel: LIVE_DOWNLOAD_STATES.has(state),
  };
}

export interface ShardRollup {
  /** The shard set's stem, or the filename itself when the file stands alone. */
  key: string;
  files: DownloadFile[];
  shardTotal: number;
  /** Files in `succeeded`, which is what "3 of 5 shards" counts. */
  done: number;
  bytesDone: number;
  bytesTotal: number;
}

/**
 * Group a download's files into shard sets.
 *
 * A sharded GGUF is one logical model (section 7.3) and the queue must read like one: five rows
 * named `…-00001-of-00005.gguf` are a rollup with a fraction, not five unrelated transfers. The
 * grouping is driven by `shard_total` from the DB rather than by re-parsing the filename, and the
 * stem is only used to *name* the group.
 */
export function rollupShards(files: readonly DownloadFile[]): ShardRollup[] {
  const groups = new Map<string, ShardRollup>();
  for (const file of files) {
    const key = file.shard_total > 1 ? shardStem(file.filename) : file.filename;
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        files: [],
        shardTotal: file.shard_total,
        done: 0,
        bytesDone: 0,
        bytesTotal: 0,
      };
      groups.set(key, group);
    }
    group.files.push(file);
    group.shardTotal = Math.max(group.shardTotal, file.shard_total);
    if (file.state === 'succeeded') group.done += 1;
    group.bytesDone += file.bytes_done;
    group.bytesTotal += file.bytes_total;
  }
  for (const group of groups.values()) {
    group.files.sort((a, b) => a.shard_index - b.shard_index);
  }
  return [...groups.values()];
}

/** `Qwen3-8B-Q4_K_M-00002-of-00005.gguf` → `Qwen3-8B-Q4_K_M`. */
export function shardStem(filename: string): string {
  const match = /^(.*?)-\d{5}-of-\d{5}\.gguf$/i.exec(filename);
  return match?.[1] ?? filename;
}

/** Percent complete, or null when the total is not known yet (`resolving`). */
export function downloadPercent(download: Download): number | null {
  if (download.bytes_total <= 0) return null;
  return Math.min(100, (download.bytes_done / download.bytes_total) * 100);
}

/**
 * The two `409`s of section 3.8, read off the error envelope.
 *
 * `download_exists` names the transfer already running, which the screen turns into a link rather
 * than an apology; `insufficient_disk` carries the numbers, which is the only form of that message
 * worth showing.
 */
export function downloadConflict(
  error: unknown,
): { kind: 'exists'; downloadId: string } | { kind: 'disk'; needed: number; free: number } | null {
  if (!(error instanceof ApiError)) return null;
  if (error.code === 'download_exists') {
    const id = error.details['download_id'];
    return { kind: 'exists', downloadId: typeof id === 'string' ? id : '' };
  }
  if (error.code === 'insufficient_disk') {
    const needed = error.details['needed_bytes'] ?? error.details['required_bytes'];
    const free = error.details['free_bytes'] ?? error.details['available_bytes'];
    return {
      kind: 'disk',
      needed: typeof needed === 'number' ? needed : 0,
      free: typeof free === 'number' ? free : 0,
    };
  }
  return null;
}
