/**
 * The EventSource wrapper.
 *
 * One EventSource per topic set (DESIGN section 4). Resume is the browser's own: we record every
 * `id:` line, and `EventSource` replays it as `Last-Event-ID` on each automatic reconnect, which is
 * exactly what section 3.14 says the daemon honors — `Last-Event-ID` replays from `events`. When
 * the *client* tears a stream down and builds a new one (a topic-set change, or `restart()`), that
 * cursor cannot travel in a header the browser owns, so the caller invalidates instead of
 * pretending nothing was missed; `lastEventId` is exposed for that decision.
 *
 * Connection health follows DESIGN section 4's rule — "if the stream drops twice it falls back to
 * interval refetch and shows a 'live updates unavailable' chip":
 *
 *   - `open` puts the stream in `live` and starts a stability timer.
 *   - Surviving `stableAfterMs` clears the drop count: a connection that stays up is healthy again.
 *   - Each `error` increments the count and cancels that timer. At `dropsBeforeDegraded` the status
 *     becomes `degraded` and stays there while the browser keeps retrying underneath.
 *
 * There is no React in this file: the whole thing is driveable from a test with a fake EventSource.
 */

import { API_BASE } from '../api/client';
import type { EventFrame } from './frames';
import { parseFrame } from './frames';
import type { Topic } from './topics';
import { isTopic, topicsParam } from './topics';

export type StreamStatus = 'idle' | 'connecting' | 'live' | 'degraded';

/** The slice of the DOM EventSource this module uses — the seam the tests substitute. */
export interface EventSourceLike {
  addEventListener(type: string, listener: (event: MessageEvent) => void): void;
  close(): void;
  onopen: ((event: Event) => void) | null;
  onerror: ((event: Event) => void) | null;
}

export interface EventStreamOptions {
  topics: readonly Topic[];
  onFrame: (frame: EventFrame) => void;
  onStatus?: (status: StreamStatus) => void;
  /** Consecutive failures before the UI stops claiming to be live. DESIGN section 4 says two. */
  dropsBeforeDegraded?: number;
  /** How long a connection must survive before its drops are forgiven. */
  stableAfterMs?: number;
  /** Injectable transport, for tests and for a future fetch-based stream. */
  createSource?: (url: string) => EventSourceLike;
}

function defaultCreateSource(url: string): EventSourceLike {
  return new EventSource(url, { withCredentials: true }) as unknown as EventSourceLike;
}

export class EventStream {
  private source: EventSourceLike | null = null;
  private stableTimer: ReturnType<typeof setTimeout> | null = null;
  private drops = 0;
  private statusValue: StreamStatus = 'idle';

  readonly topics: readonly Topic[];
  private readonly onFrame: (frame: EventFrame) => void;
  private readonly onStatus: (status: StreamStatus) => void;
  private readonly dropsBeforeDegraded: number;
  private readonly stableAfterMs: number;
  private readonly createSource: (url: string) => EventSourceLike;

  /** The most recent `id:` line seen. The browser replays it as `Last-Event-ID`. */
  lastEventId: string | null = null;

  constructor(options: EventStreamOptions) {
    this.topics = [...options.topics];
    this.onFrame = options.onFrame;
    this.onStatus = options.onStatus ?? (() => {});
    this.dropsBeforeDegraded = options.dropsBeforeDegraded ?? 2;
    this.stableAfterMs = options.stableAfterMs ?? 10_000;
    this.createSource = options.createSource ?? defaultCreateSource;
  }

  get status(): StreamStatus {
    return this.statusValue;
  }

  /** `/api/v1/events?topics=…`. Sorted, so one topic set is always one URL. */
  get url(): string {
    const topics = topicsParam(this.topics);
    return topics
      ? `${API_BASE}/events?topics=${encodeURIComponent(topics)}`
      : `${API_BASE}/events`;
  }

  start(): void {
    if (this.source) return;
    this.setStatus('connecting');
    const source = this.createSource(this.url);
    this.source = source;

    source.onopen = () => {
      this.setStatus('live');
      this.armStabilityTimer();
    };

    source.onerror = () => {
      this.clearStabilityTimer();
      this.drops += 1;
      if (this.drops >= this.dropsBeforeDegraded) this.setStatus('degraded');
      else this.setStatus('connecting');
    };

    // A named `event:` per topic, so one listener per topic rather than one switch over `type`.
    for (const topic of this.topics) {
      source.addEventListener(topic, (event) => this.handle(topic, event));
    }
    // Frames the daemon sends without an `event:` line arrive as `message`; the topic then has to
    // come from the payload, and a payload that names no known topic is dropped.
    source.addEventListener('message', (event) => this.handleUntyped(event));
  }

  stop(): void {
    this.clearStabilityTimer();
    this.source?.close();
    this.source = null;
    this.setStatus('idle');
  }

  /** Tear down and reconnect. The caller is responsible for refetching what the gap may have held. */
  restart(): void {
    const wasRunning = this.source !== null;
    this.stop();
    this.drops = 0;
    if (wasRunning) this.start();
  }

  private handle(topic: Topic, event: MessageEvent): void {
    const cursor = event.lastEventId || null;
    if (cursor) this.lastEventId = cursor;
    const frame = parseFrame(topic, cursor, typeof event.data === 'string' ? event.data : '');
    if (frame) this.onFrame(frame);
  }

  private handleUntyped(event: MessageEvent): void {
    let topic: string | undefined;
    try {
      const parsed: unknown = JSON.parse(typeof event.data === 'string' ? event.data : '');
      if (typeof parsed === 'object' && parsed !== null) {
        const value = (parsed as Record<string, unknown>)['topic'];
        if (typeof value === 'string') topic = value;
      }
    } catch {
      return;
    }
    if (!topic || !isTopic(topic)) return;
    this.handle(topic, event);
  }

  private armStabilityTimer(): void {
    this.clearStabilityTimer();
    this.stableTimer = setTimeout(() => {
      this.drops = 0;
      this.stableTimer = null;
    }, this.stableAfterMs);
  }

  private clearStabilityTimer(): void {
    if (this.stableTimer !== null) {
      clearTimeout(this.stableTimer);
      this.stableTimer = null;
    }
  }

  private setStatus(next: StreamStatus): void {
    // Once degraded, a bare reconnect attempt does not get to claim progress; only `open` does.
    if (this.statusValue === 'degraded' && next === 'connecting') return;
    if (this.statusValue === next) return;
    this.statusValue = next;
    this.onStatus(next);
  }
}
