/**
 * The connection state machine.
 *
 * DESIGN section 4: "if the stream drops twice it falls back to interval refetch and shows a 'live
 * updates unavailable' chip". That sentence is a rule with edges — what counts as a drop, what
 * clears the count, what a reconnect attempt is allowed to claim — and this is where they are
 * pinned down.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { EventFrame } from './frames';
import { EventStream } from './stream';
import type { EventSourceLike, StreamStatus } from './stream';

class FakeSource implements EventSourceLike {
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closed = false;
  readonly listeners = new Map<string, ((event: MessageEvent) => void)[]>();

  constructor(readonly url: string) {}

  addEventListener(type: string, listener: (event: MessageEvent) => void): void {
    const existing = this.listeners.get(type) ?? [];
    existing.push(listener);
    this.listeners.set(type, existing);
  }

  close(): void {
    this.closed = true;
  }

  open(): void {
    this.onopen?.(new Event('open'));
  }

  fail(): void {
    this.onerror?.(new Event('error'));
  }

  emit(type: string, data: unknown, lastEventId = ''): void {
    const event = { data: JSON.stringify(data), lastEventId } as MessageEvent;
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function build(overrides: { dropsBeforeDegraded?: number; stableAfterMs?: number } = {}) {
  const sources: FakeSource[] = [];
  const statuses: StreamStatus[] = [];
  const frames: EventFrame[] = [];
  const stream = new EventStream({
    topics: ['instances', 'jobs'],
    onFrame: (f) => frames.push(f),
    onStatus: (s) => statuses.push(s),
    createSource: (url) => {
      const source = new FakeSource(url);
      sources.push(source);
      return source;
    },
    ...overrides,
  });
  return { stream, sources, statuses, frames };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('EventStream', () => {
  it('subscribes to a canonical, sorted topic URL', () => {
    const { stream, sources } = build();
    stream.start();
    expect(sources[0]!.url).toBe('/api/v1/events?topics=instances%2Cjobs');
  });

  it('reports connecting, then live, on open', () => {
    const { stream, sources, statuses } = build();
    stream.start();
    expect(statuses).toEqual(['connecting']);
    sources[0]!.open();
    expect(statuses).toEqual(['connecting', 'live']);
    expect(stream.status).toBe('live');
  });

  it('degrades on the second drop, exactly as section 4 says', () => {
    const { stream, sources, statuses } = build();
    stream.start();
    sources[0]!.open();
    sources[0]!.fail();
    expect(stream.status).toBe('connecting');
    sources[0]!.fail();
    expect(stream.status).toBe('degraded');
    expect(statuses).toEqual(['connecting', 'live', 'connecting', 'degraded']);
  });

  it('will not let a bare reconnect attempt claim progress once degraded', () => {
    const { stream, sources } = build();
    stream.start();
    sources[0]!.fail();
    sources[0]!.fail();
    expect(stream.status).toBe('degraded');
    sources[0]!.fail();
    expect(stream.status).toBe('degraded');
  });

  it('forgives the drop count once a connection has survived the stability window', () => {
    const { stream, sources } = build({ stableAfterMs: 10_000 });
    stream.start();
    sources[0]!.open();
    sources[0]!.fail(); // one drop
    sources[0]!.open();
    vi.advanceTimersByTime(10_000); // this connection held
    sources[0]!.fail();
    // The forgiven drop means this is the first again, not the second.
    expect(stream.status).toBe('connecting');
  });

  it('does not forgive a connection that dropped before the window elapsed', () => {
    const { stream, sources } = build({ stableAfterMs: 10_000 });
    stream.start();
    sources[0]!.open();
    sources[0]!.fail();
    sources[0]!.open();
    vi.advanceTimersByTime(5_000);
    sources[0]!.fail();
    expect(stream.status).toBe('degraded');
  });

  it('records the cursor the browser will replay as Last-Event-ID', () => {
    const { stream, sources, frames } = build();
    stream.start();
    sources[0]!.open();
    sources[0]!.emit(
      'instances',
      { type: 'instance.status', id: 'inst-1', patch: { state: 'ready' } },
      '01J8ZQ4K7T9WX2',
    );
    expect(stream.lastEventId).toBe('01J8ZQ4K7T9WX2');
    expect(frames).toHaveLength(1);
    expect(frames[0]!.topic).toBe('instances');
    expect(frames[0]!.patch).toEqual({ state: 'ready' });
  });

  it('accepts an untyped frame only when its payload names a known topic', () => {
    const { stream, sources, frames } = build();
    stream.start();
    sources[0]!.emit('message', { topic: 'jobs', type: 'job.progress', id: 'job-1' });
    sources[0]!.emit('message', { topic: 'not-a-topic', type: 'x.y', id: 'z' });
    sources[0]!.emit('message', { type: 'no.topic', id: 'z' });
    expect(frames.map((f) => f.topic)).toEqual(['jobs']);
  });

  it('closes the source on stop and opens a fresh one on restart', () => {
    const { stream, sources } = build();
    stream.start();
    const first = sources[0]!;
    stream.restart();
    expect(first.closed).toBe(true);
    expect(sources).toHaveLength(2);
    expect(stream.status).toBe('connecting');
  });

  it('is idle before start and after stop', () => {
    const { stream } = build();
    expect(stream.status).toBe('idle');
    stream.start();
    stream.stop();
    expect(stream.status).toBe('idle');
  });
});
