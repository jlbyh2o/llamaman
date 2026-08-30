/**
 * Frame parsing.
 *
 * DESIGN section 3.14 fixes the wire shape:
 *
 *     event: instances
 *     id: 01J8Z…
 *     data: {"type":"instance.status","id":"01J8…","patch":{"state":"ready"}}
 *
 * `type` names what happened, `id` is the entity the patch belongs to, and `patch` is a partial of
 * that entity — "patches keyed by entity id, so the client merges into its query cache without
 * refetching". The `id:` line is the ULID cursor the browser replays as `Last-Event-ID`.
 */

import type { Topic } from './topics';

export interface EventFrame {
  /** The SSE `event:` name. */
  topic: Topic;
  /** The SSE `id:` line — the ULID cursor. Null when the daemon sent an unidentified frame. */
  cursor: string | null;
  /** `data.type`, e.g. `instance.status`, `download.progress`, `job.finished`. */
  type: string;
  /** `data.id` — the entity this frame is about. Empty for frames that name no entity. */
  id: string;
  /** `data.patch` — a partial of the entity, or undefined for a frame that only signals. */
  patch: Record<string, unknown> | undefined;
  /** Anything else the daemon put in `data`, kept so screens can read frame-specific fields. */
  data: Record<string, unknown>;
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Parse one frame. Returns null for anything malformed — a frame this build does not understand is
 * dropped rather than allowed to corrupt the cache, and the caller treats the miss as a reason to
 * refetch, not as an error.
 */
export function parseFrame(topic: Topic, cursor: string | null, raw: string): EventFrame | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isPlainObject(parsed)) return null;

  const type = parsed['type'];
  if (typeof type !== 'string' || type === '') return null;

  const id = parsed['id'];
  const patch = parsed['patch'];

  return {
    topic,
    cursor,
    type,
    id: typeof id === 'string' ? id : '',
    patch: isPlainObject(patch) ? patch : undefined,
    data: parsed,
  };
}

/** `instance.status` -> `instance`. The half of a frame type that names the entity family. */
export function frameSubject(type: string): string {
  const dot = type.indexOf('.');
  return dot === -1 ? type : type.slice(0, dot);
}

/** `instance.status` -> `status`. The half that names what happened. */
export function frameAction(type: string): string {
  const dot = type.indexOf('.');
  return dot === -1 ? '' : type.slice(dot + 1);
}
