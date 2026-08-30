/**
 * The queue's reading of a download, and the list envelope the generator cannot type.
 *
 * Two things are asserted here because both are places where a wrong answer is invisible rather
 * than loud. A shard rollup that mis-grouped would show five transfers where DESIGN section 7.3
 * says there is one model; and `asPage` exists because the generated schema types every list
 * response as `any` (see api.ts), so it is the only thing standing between a malformed body and a
 * screen that renders `undefined.map`.
 */

import { describe, expect, it } from 'vitest';

import type { Download, DownloadFile } from '../../api/types';
import { asPage, compact, modelSort, oneOf } from './api';
import {
  downloadControls,
  downloadKey,
  downloadPercent,
  isLive,
  rollupShards,
  shardStem,
} from './downloads';

function file(overrides: Partial<DownloadFile> = {}): DownloadFile {
  return {
    id: 'f1',
    model_id: 'm1',
    model_file_id: 'mf1',
    filename: 'Qwen3-8B-Q4_K_M.gguf',
    shard_index: 1,
    shard_total: 1,
    bytes_done: 0,
    bytes_total: 1000,
    attempts: 1,
    state: 'queued',
    ...overrides,
  };
}

function shard(index: number, total: number, overrides: Partial<DownloadFile> = {}): DownloadFile {
  const padded = String(index).padStart(5, '0');
  const totalPadded = String(total).padStart(5, '0');
  return file({
    id: `shard-${index}`,
    filename: `Qwen3-8B-Q4_K_M-${padded}-of-${totalPadded}.gguf`,
    shard_index: index,
    shard_total: total,
    ...overrides,
  });
}

describe('shardStem', () => {
  it('strips the shard suffix so a set can be named once', () => {
    expect(shardStem('Qwen3-8B-Q4_K_M-00002-of-00005.gguf')).toBe('Qwen3-8B-Q4_K_M');
  });

  it('leaves an unsharded filename alone', () => {
    expect(shardStem('mmproj-F16.gguf')).toBe('mmproj-F16.gguf');
  });
});

describe('rollupShards', () => {
  it('folds a shard set into one row with a fraction', () => {
    const rollups = rollupShards([
      shard(1, 3, { state: 'succeeded', bytes_done: 1000 }),
      shard(2, 3, { state: 'running', bytes_done: 400 }),
      shard(3, 3),
    ]);

    expect(rollups).toHaveLength(1);
    expect(rollups[0]!.key).toBe('Qwen3-8B-Q4_K_M');
    expect(rollups[0]!.shardTotal).toBe(3);
    expect(rollups[0]!.done).toBe(1);
    expect(rollups[0]!.bytesDone).toBe(1400);
    expect(rollups[0]!.bytesTotal).toBe(3000);
  });

  it('orders the shards within a set, however the API returned them', () => {
    const rollups = rollupShards([shard(3, 3), shard(1, 3), shard(2, 3)]);
    expect(rollups[0]!.files.map((f) => f.shard_index)).toEqual([1, 2, 3]);
  });

  it('keeps a projector separate from the quantization it was downloaded with', () => {
    const rollups = rollupShards([
      shard(1, 2),
      shard(2, 2),
      file({ id: 'proj', filename: 'mmproj-F16.gguf' }),
    ]);
    expect(rollups.map((r) => r.key)).toEqual(['Qwen3-8B-Q4_K_M', 'mmproj-F16.gguf']);
  });
});

describe('downloadControls', () => {
  it('offers pause while a transfer is going somewhere, and resume only when paused', () => {
    expect(downloadControls('running')).toMatchObject({ pause: true, resume: false, cancel: true });
    expect(downloadControls('paused')).toMatchObject({ pause: false, resume: true, cancel: true });
  });

  it('offers retry only for the two terminal failures a retry can act on', () => {
    expect(downloadControls('failed').retry).toBe(true);
    expect(downloadControls('canceled').retry).toBe(true);
    expect(downloadControls('succeeded').retry).toBe(false);
  });

  it('offers nothing on a finished transfer but leaves it in the ledger', () => {
    expect(downloadControls('succeeded')).toEqual({
      pause: false,
      resume: false,
      retry: false,
      cancel: false,
    });
  });
});

describe('downloadPercent', () => {
  const base: Download = {
    id: 'd1',
    model_id: 'm1',
    repo_id: 'bartowski/Qwen3-8B-GGUF',
    revision: 'main',
    primary_file: 'Qwen3-8B-Q4_K_M.gguf',
    state: 'running',
    priority: 100,
    include_mmproj: true,
    bytes_total: 1000,
    bytes_done: 250,
    bytes_at_start: 0,
    speed_bps: 100,
    attempts: 1,
    created_at: '2026-08-30T00:00:00Z',
    files: [],
  };

  it('is a fraction of the declared total', () => {
    expect(downloadPercent(base)).toBe(25);
  });

  it('is null — not zero — while the total is still being resolved', () => {
    // `resolving` has no byte total yet, and a 0% bar would claim progress information the daemon
    // has not produced. The Progress component renders null as indeterminate.
    expect(downloadPercent({ ...base, state: 'resolving', bytes_total: 0 })).toBeNull();
  });

  it('counts paused transfers as still in the queue', () => {
    expect(isLive({ ...base, state: 'paused' })).toBe(true);
    expect(isLive({ ...base, state: 'succeeded' })).toBe(false);
  });
});

describe('downloadKey', () => {
  const body = {
    repo_id: 'bartowski/Qwen3-8B-GGUF',
    files: ['Qwen3-8B-Q4_K_M.gguf'],
    include_mmproj: true,
    priority: 100,
  };

  it('collapses two clicks on an unchanged dialog into one request', () => {
    expect(downloadKey(body)).toBe(downloadKey({ ...body }));
  });

  it('ignores key order, which the conditional spreads in the body can change', () => {
    expect(downloadKey(body)).toBe(
      downloadKey({
        priority: 100,
        include_mmproj: true,
        files: ['Qwen3-8B-Q4_K_M.gguf'],
        repo_id: 'bartowski/Qwen3-8B-GGUF',
      }),
    );
  });

  it('changes when the body changes, or the reuse would be a 422 rather than a replay', () => {
    // Section 3: the same key with a different body inside the window is idempotency_key_reused.
    expect(downloadKey({ ...body, priority: 10 })).not.toBe(downloadKey(body));
    expect(downloadKey({ ...body, include_mmproj: false })).not.toBe(downloadKey(body));
    expect(downloadKey({ ...body, files: ['Qwen3-8B-Q8_0.gguf'] })).not.toBe(downloadKey(body));
  });

  it('is a short printable ASCII token, which the header requires', () => {
    expect(downloadKey(body)).toMatch(/^dl-[0-9a-f]{8}$/);
  });
});

describe('asPage', () => {
  it('passes a well-formed page through', () => {
    expect(asPage<number>({ items: [1, 2], total: 7, next_cursor: '01J' })).toEqual({
      items: [1, 2],
      total: 7,
      next_cursor: '01J',
    });
  });

  it('accepts a bare array, which is what the array-valued endpoints return', () => {
    expect(asPage<number>([1, 2, 3])).toEqual({ items: [1, 2, 3], total: 3, next_cursor: null });
  });

  it('degrades to an empty page rather than throwing halfway down a render', () => {
    expect(asPage<number>(null)).toEqual({ items: [], total: 0 });
    expect(asPage<number>({ error: 'nope' })).toEqual({ items: [], total: 0 });
  });
});

describe('search-param vocabularies', () => {
  it('accepts a value the endpoint declares and drops anything else', () => {
    expect(modelSort('size')).toBe('size');
    expect(modelSort('rm -rf')).toBeUndefined();
    expect(modelSort(undefined)).toBeUndefined();
  });

  it('is a general guard, not a one-off', () => {
    const state = oneOf(['active', 'all'] as const);
    expect(state('all')).toBe('all');
    expect(state('everything')).toBeUndefined();
  });
});

describe('compact', () => {
  it('drops undefined values, which exactOptionalPropertyTypes forbids writing', () => {
    expect(compact({ q: 'qwen', sort: undefined, limit: 30 })).toEqual({ q: 'qwen', limit: 30 });
  });

  it('keeps falsy values that are real answers', () => {
    expect(compact({ gated: false, priority: 0 })).toEqual({ gated: false, priority: 0 });
  });
});
