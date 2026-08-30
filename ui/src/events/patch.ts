/**
 * Turning a frame into a cache write.
 *
 * DESIGN section 4: "One `useEvents()` hook opens the SSE stream and maps each frame to
 * `queryClient.setQueryData`". This module is that map, and it is deliberately free of React so the
 * reducer can be tested against a bare `QueryClient`.
 *
 * The rules, in order:
 *
 *  1. A frame whose action is a lifecycle boundary — `created`, `deleted`, `purged` — changes which
 *     rows exist, which no patch can express. Its family's lists are invalidated.
 *  2. A frame carrying a `patch` and an `id` merges into the family's detail query for that id and
 *     into that id's entry in every cached list of the family, however it was filtered.
 *  3. A frame carrying no patch is a signal: invalidate the family and let the query re-read.
 *
 * The merge is a deep merge over plain objects (arrays and scalars replace), so a frame may patch a
 * nested object directly. `FRAME_PATCH_PATH` additionally scopes a frame type whose patch is
 * written against a sub-object rather than the entity root — `instance.status` is the one section
 * 3.14 names, whose patch is `InstanceStatusDTO`-shaped while `InstanceDTO` carries it under
 * `status`.
 */

import type { QueryClient, QueryKey } from '@tanstack/react-query';
import type { Family } from '../api/keys';
import { queryKeys } from '../api/keys';
import type { EventFrame } from './frames';
import { frameAction } from './frames';
import type { Topic } from './topics';

interface TopicRoute {
  /** The query-key family the topic's entities live in. */
  family: Family;
  /** The property that identifies an entity inside a list. */
  idField: string;
  /**
   * A query holding a bare array of these entities rather than a `{items:[…]}` page — the GPU
   * meters read `GET /system/gpus`, which is an array and is not in a family of its own.
   */
  arrayKey?: QueryKey;
  /** Topics whose frames are always a signal: there is no client-side entity to patch. */
  signalOnly?: boolean;
}

export const TOPIC_ROUTES: Record<Topic, TopicRoute> = {
  instances: { family: 'instances', idField: 'id' },
  downloads: { family: 'downloads', idField: 'id' },
  llamacpp: { family: 'llamacpp', idField: 'id' },
  gpu: { family: 'system', idField: 'uuid', arrayKey: queryKeys.system.gpus() },
  bench: { family: 'bench', idField: 'id' },
  jobs: { family: 'jobs', idField: 'id' },
  events: { family: 'events', idField: 'id', signalOnly: true },
  notifications: { family: 'notifications', idField: 'id', signalOnly: true },
};

/** Frame types whose patch is scoped to a sub-object of the entity. */
export const FRAME_PATCH_PATH: Readonly<Record<string, string>> = {
  'instance.status': 'status',
};

/** Actions that add or remove rows, which a patch cannot express. */
const LIFECYCLE_ACTIONS = new Set(['created', 'deleted', 'purged', 'restored']);

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Deep-merge `patch` into `base`. Plain objects merge recursively; arrays, scalars and nulls
 * replace. Returns `base` unchanged (by identity) when the patch changes nothing, so React Query
 * does not re-render a screen for a frame that said nothing new.
 */
export function deepMerge<T>(base: T, patch: Record<string, unknown>): T {
  if (!isPlainObject(base)) return patch as unknown as T;
  let changed = false;
  const next: Record<string, unknown> = { ...base };
  for (const [key, value] of Object.entries(patch)) {
    const prev = next[key];
    if (isPlainObject(value) && isPlainObject(prev)) {
      const merged = deepMerge(prev, value);
      if (merged !== prev) {
        next[key] = merged;
        changed = true;
      }
    } else if (!Object.is(prev, value)) {
      next[key] = value;
      changed = true;
    }
  }
  return changed ? (next as unknown as T) : base;
}

/** Wrap a patch in the sub-object path a frame type declares, if any. */
export function scopePatch(type: string, patch: Record<string, unknown>): Record<string, unknown> {
  const path = FRAME_PATCH_PATH[type];
  return path ? { [path]: patch } : patch;
}

function patchList(
  value: unknown,
  id: string,
  idField: string,
  patch: Record<string, unknown>,
): unknown {
  if (Array.isArray(value)) {
    let changed = false;
    const next = value.map((item) => {
      if (!isPlainObject(item) || item[idField] !== id) return item;
      const merged = deepMerge(item, patch);
      if (merged !== item) changed = true;
      return merged;
    });
    return changed ? next : value;
  }
  if (isPlainObject(value) && Array.isArray(value['items'])) {
    const items = patchList(value['items'], id, idField, patch);
    return items === value['items'] ? value : { ...value, items };
  }
  return value;
}

/**
 * Apply one frame to the cache.
 *
 * Returns what it did, which the provider uses for its "live updates" diagnostics and the tests
 * assert on: `'patched'`, `'invalidated'`, or `'ignored'` for a frame with nothing to act on.
 */
export function applyFrame(
  client: QueryClient,
  frame: EventFrame,
): 'patched' | 'invalidated' | 'ignored' {
  const route = TOPIC_ROUTES[frame.topic];
  if (!route) return 'ignored';

  const action = frameAction(frame.type);

  if (route.signalOnly || LIFECYCLE_ACTIONS.has(action) || !frame.patch || !frame.id) {
    void client.invalidateQueries({ queryKey: queryKeys.family(route.family) });
    return 'invalidated';
  }

  const patch = scopePatch(frame.type, frame.patch);

  // The detail query for this entity, matched exactly so that sub-resources of the same id
  // (`…/status`, `…/starts`) are not handed an entity-shaped patch.
  const detailKey = queryKeys.detail(route.family, frame.id);
  const detail = client.getQueryData(detailKey);
  if (detail !== undefined) {
    client.setQueryData(detailKey, (prev: unknown) =>
      isPlainObject(prev) ? deepMerge(prev, patch) : prev,
    );
  }

  // Every cached list of the family, whatever filters produced it.
  client.setQueriesData({ queryKey: [route.family, 'list'] }, (prev: unknown) =>
    patchList(prev, frame.id, route.idField, patch),
  );

  if (route.arrayKey) {
    client.setQueriesData({ queryKey: route.arrayKey }, (prev: unknown) =>
      patchList(prev, frame.id, route.idField, patch),
    );
  }

  return 'patched';
}
