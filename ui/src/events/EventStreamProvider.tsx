/**
 * The React face of the event stream.
 *
 * Mount `<EventStreamProvider>` once, inside the QueryClientProvider. It opens one EventSource for
 * the topic set it is given, maps every frame onto the cache through `applyFrame`, and publishes
 * the connection status so the shell can show the "live updates unavailable" chip and so screens
 * can fall back to interval refetch (DESIGN section 4).
 *
 * Screens never open a stream of their own. They read `useEventStatus()` and spread
 * `useLiveQueryOptions()` into the queries that would otherwise need polling.
 */

import { createContext, useContext, useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { EventFrame } from './frames';
import { applyFrame } from './patch';
import { EventStream } from './stream';
import type { EventSourceLike, StreamStatus } from './stream';
import { ALL_TOPICS } from './topics';
import type { Topic } from './topics';

export interface EventStreamContextValue {
  status: StreamStatus;
  /** The last SSE `id:` seen — the cursor the browser replays as `Last-Event-ID`. */
  lastEventId: string | null;
  /** Frames seen since mount. Cheap liveness evidence for the diagnostics panel. */
  framesSeen: number;
  /** Drop and rebuild the stream, then refetch — the "reconnect now" affordance on the chip. */
  reconnect: () => void;
}

const EventStreamContext = createContext<EventStreamContextValue | null>(null);

export interface EventStreamProviderProps {
  children: ReactNode;
  topics?: readonly Topic[];
  /** Off for the login and wizard shells, which have no session to stream with. */
  enabled?: boolean;
  /** Test seam: substitute the transport. */
  createSource?: (url: string) => EventSourceLike;
  /** Extra observer, for tests and for the events screen's raw frame log. */
  onFrame?: (frame: EventFrame) => void;
}

export function EventStreamProvider({
  children,
  topics = ALL_TOPICS,
  enabled = true,
  createSource,
  onFrame,
}: EventStreamProviderProps) {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState<StreamStatus>('idle');
  const [framesSeen, setFramesSeen] = useState(0);
  const streamRef = useRef<EventStream | null>(null);

  // Latest-callback refs: the stream is built once per topic set, and must not be rebuilt because
  // a parent re-rendered with a new inline `onFrame`.
  const onFrameRef = useRef(onFrame);
  onFrameRef.current = onFrame;

  const topicsKey = useMemo(() => [...topics].sort().join(','), [topics]);

  useEffect(() => {
    if (!enabled) {
      setStatus('idle');
      return;
    }
    const stream = new EventStream({
      topics: topicsKey.split(',').filter(Boolean) as Topic[],
      onStatus: setStatus,
      onFrame: (frame) => {
        applyFrame(queryClient, frame);
        setFramesSeen((n) => n + 1);
        onFrameRef.current?.(frame);
      },
      ...(createSource ? { createSource } : {}),
    });
    streamRef.current = stream;
    stream.start();
    return () => {
      stream.stop();
      streamRef.current = null;
    };
  }, [enabled, topicsKey, queryClient, createSource]);

  const value = useMemo<EventStreamContextValue>(
    () => ({
      status,
      lastEventId: streamRef.current?.lastEventId ?? null,
      framesSeen,
      reconnect: () => {
        streamRef.current?.restart();
        // A client-initiated restart cannot carry Last-Event-ID, so the gap is closed by re-reading
        // rather than by hoping the daemon replays it.
        void queryClient.invalidateQueries();
      },
    }),
    [status, framesSeen, queryClient],
  );

  return <EventStreamContext.Provider value={value}>{children}</EventStreamContext.Provider>;
}

/**
 * The stream's state. Safe outside the provider — the login screen and the wizard render without
 * one — where it reports `idle` and a no-op reconnect.
 */
export function useEventStatus(): EventStreamContextValue {
  return (
    useContext(EventStreamContext) ?? {
      status: 'idle',
      lastEventId: null,
      framesSeen: 0,
      reconnect: () => {},
    }
  );
}

/**
 * Query options for data that SSE normally keeps fresh.
 *
 * While the stream is live this is `{ refetchInterval: false }` — no polling loop where a topic
 * exists. When the stream has given up, it becomes the interval refetch DESIGN section 4 falls back
 * to, so a degraded connection costs freshness rather than correctness.
 */
export function useLiveQueryOptions(intervalMs = 5_000): { refetchInterval: number | false } {
  const { status } = useEventStatus();
  return { refetchInterval: status === 'degraded' ? intervalMs : false };
}
