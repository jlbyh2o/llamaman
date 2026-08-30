/**
 * The SSE reducer.
 *
 * DESIGN section 3.14 promises "patches keyed by entity id, so the client merges into its query
 * cache without refetching", and section 4 promises the cache is patched rather than polled. These
 * tests are what make those two sentences enforceable: a frame lands in the detail query and in
 * every cached list, a frame that cannot be expressed as a patch invalidates instead, and a frame
 * that says nothing new does not produce a new object (which is what stops a re-render storm).
 */

import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { queryKeys } from '../api/keys';
import type { EventFrame } from './frames';
import { frameAction, frameSubject, parseFrame } from './frames';
import { applyFrame, deepMerge, scopePatch } from './patch';

function frame(partial: Partial<EventFrame> & Pick<EventFrame, 'topic' | 'type'>): EventFrame {
  return {
    cursor: '01J8ZQ4K7T9WX2',
    id: 'inst-1',
    patch: undefined,
    data: {},
    ...partial,
  };
}

describe('frame parsing', () => {
  it('reads the wire shape of section 3.14', () => {
    const parsed = parseFrame(
      'instances',
      '01J8ZQ',
      '{"type":"instance.status","id":"inst-1","patch":{"state":"ready"}}',
    );
    expect(parsed).toEqual({
      topic: 'instances',
      cursor: '01J8ZQ',
      type: 'instance.status',
      id: 'inst-1',
      patch: { state: 'ready' },
      data: { type: 'instance.status', id: 'inst-1', patch: { state: 'ready' } },
    });
  });

  it('drops a frame this build cannot understand rather than corrupting the cache', () => {
    expect(parseFrame('jobs', null, 'not json')).toBeNull();
    expect(parseFrame('jobs', null, '{"id":"x"}')).toBeNull();
    expect(parseFrame('jobs', null, '[1,2,3]')).toBeNull();
  });

  it('splits a frame type into subject and action', () => {
    expect(frameSubject('instance.status')).toBe('instance');
    expect(frameAction('instance.status')).toBe('status');
    expect(frameAction('heartbeat')).toBe('');
  });
});

describe('deepMerge', () => {
  it('merges nested objects and replaces arrays', () => {
    const base = { a: 1, nested: { x: 1, y: 2 }, list: [1, 2] };
    expect(deepMerge(base, { nested: { y: 9 }, list: [3] })).toEqual({
      a: 1,
      nested: { x: 1, y: 9 },
      list: [3],
    });
  });

  it('returns the same object when nothing changed', () => {
    const base = { a: 1, nested: { x: 1 } };
    expect(deepMerge(base, { a: 1, nested: { x: 1 } })).toBe(base);
  });

  it('scopes a patch to the sub-object its frame type declares', () => {
    expect(scopePatch('instance.status', { state: 'ready' })).toEqual({
      status: { state: 'ready' },
    });
    expect(scopePatch('instance.updated', { autostart: true })).toEqual({ autostart: true });
  });
});

describe('applyFrame', () => {
  function client() {
    return new QueryClient({ defaultOptions: { queries: { retry: false } } });
  }

  it('patches the detail query and every cached list of the family', () => {
    const qc = client();
    qc.setQueryData(queryKeys.instances.detail('inst-1'), {
      id: 'inst-1',
      name: 'qwen',
      status: { state: 'loading', slots_busy: 0 },
    });
    qc.setQueryData(queryKeys.instances.list(), {
      items: [
        { id: 'inst-1', name: 'qwen', status: { state: 'loading' } },
        { id: 'inst-2', name: 'gemma', status: { state: 'ready' } },
      ],
      total: 2,
    });
    qc.setQueryData(queryKeys.instances.list({ state: 'loading' }), {
      items: [{ id: 'inst-1', name: 'qwen', status: { state: 'loading' } }],
      total: 1,
    });

    const result = applyFrame(
      qc,
      frame({ topic: 'instances', type: 'instance.status', patch: { state: 'ready' } }),
    );

    expect(result).toBe('patched');
    expect(qc.getQueryData(queryKeys.instances.detail('inst-1'))).toEqual({
      id: 'inst-1',
      name: 'qwen',
      status: { state: 'ready', slots_busy: 0 },
    });

    const list = qc.getQueryData(queryKeys.instances.list()) as { items: { status: unknown }[] };
    expect(list.items[0]!.status).toEqual({ state: 'ready' });
    // Other rows are untouched.
    expect(list.items[1]!.status).toEqual({ state: 'ready' });

    const filtered = qc.getQueryData(queryKeys.instances.list({ state: 'loading' })) as {
      items: { status: unknown }[];
    };
    expect(filtered.items[0]!.status).toEqual({ state: 'ready' });
  });

  it('does not invent a detail entry for an entity nothing has loaded', () => {
    const qc = client();
    applyFrame(
      qc,
      frame({ topic: 'instances', type: 'instance.status', patch: { state: 'ready' } }),
    );
    expect(qc.getQueryData(queryKeys.instances.detail('inst-1'))).toBeUndefined();
  });

  it('leaves sub-resource queries of the same id alone', () => {
    const qc = client();
    const starts = [{ id: 'start-1', outcome: 'ok' }];
    qc.setQueryData(queryKeys.instances.starts('inst-1'), starts);
    applyFrame(
      qc,
      frame({ topic: 'instances', type: 'instance.status', patch: { state: 'ready' } }),
    );
    expect(qc.getQueryData(queryKeys.instances.starts('inst-1'))).toBe(starts);
  });

  it('invalidates rather than patches when rows appear or disappear', () => {
    const qc = client();
    expect(applyFrame(qc, frame({ topic: 'instances', type: 'instance.created' }))).toBe(
      'invalidated',
    );
    expect(applyFrame(qc, frame({ topic: 'instances', type: 'instance.deleted' }))).toBe(
      'invalidated',
    );
  });

  it('treats the events and notifications topics as signals', () => {
    const qc = client();
    expect(
      applyFrame(qc, frame({ topic: 'events', type: 'event.appended', patch: { level: 'warn' } })),
    ).toBe('invalidated');
    expect(applyFrame(qc, frame({ topic: 'notifications', type: 'notification.raised' }))).toBe(
      'invalidated',
    );
  });

  it('patches the GPU array by uuid, which is its identity', () => {
    const qc = client();
    qc.setQueryData(queryKeys.system.gpus(), [
      { uuid: 'GPU-a', vram_free: 1, name: 'A' },
      { uuid: 'GPU-b', vram_free: 2, name: 'B' },
    ]);
    applyFrame(
      qc,
      frame({ topic: 'gpu', type: 'gpu.telemetry', id: 'GPU-b', patch: { vram_free: 99 } }),
    );
    expect(qc.getQueryData(queryKeys.system.gpus())).toEqual([
      { uuid: 'GPU-a', vram_free: 1, name: 'A' },
      { uuid: 'GPU-b', vram_free: 99, name: 'B' },
    ]);
  });

  it('patches a download in a bare-array list as well as a paged one', () => {
    const qc = client();
    qc.setQueryData(queryKeys.downloads.list({ state: 'active' }), [
      { id: 'dl-1', bytes_done: 10 },
    ]);
    applyFrame(
      qc,
      frame({
        topic: 'downloads',
        type: 'download.progress',
        id: 'dl-1',
        patch: { bytes_done: 20 },
      }),
    );
    expect(qc.getQueryData(queryKeys.downloads.list({ state: 'active' }))).toEqual([
      { id: 'dl-1', bytes_done: 20 },
    ]);
  });
});
