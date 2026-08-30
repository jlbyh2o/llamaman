/**
 * The SSE topics of DESIGN section 3.14.
 *
 * `GET /api/v1/events?topics=…` filters the stream; frames arrive as
 * `event: <topic>` / `id: <ulid>` / `data: {"type":"instance.status","id":"…","patch":{…}}`,
 * with a `retry: 3000` directive and a 20 s `:keepalive` comment.
 */

export const TOPICS = [
  'instances',
  'downloads',
  'llamacpp',
  'gpu',
  'bench',
  'jobs',
  'events',
  'notifications',
] as const;

export type Topic = (typeof TOPICS)[number];

const TOPIC_SET: ReadonlySet<string> = new Set(TOPICS);

export function isTopic(value: string): value is Topic {
  return TOPIC_SET.has(value);
}

/**
 * Everything the shell subscribes to. A screen that needs a narrower set passes its own; the
 * provider keeps one EventSource per distinct set (DESIGN section 4: one stream, patched cache).
 */
export const ALL_TOPICS: readonly Topic[] = TOPICS;

/** Canonical `?topics=` value: sorted and de-duplicated, so one set is always one URL. */
export function topicsParam(topics: readonly Topic[]): string {
  return [...new Set(topics)].sort().join(',');
}
