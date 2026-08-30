/**
 * A log tail over SSE.
 *
 * Separate from the topic stream on purpose. The topic stream carries entity patches for the whole
 * app; a log tail is a per-screen subscription to one endpoint — `GET /llamacpp/versions/{id}/log`,
 * `GET /instances/{id}/logs`, `GET /system/journal` — which section 3 says answers with SSE when the
 * request accepts an event stream, and which must be torn down when the screen unmounts.
 *
 * The daemon may send a frame as a bare line or as `{"line":"…"}`; both are accepted, because the
 * shape is not pinned by openapi.json (the response is typed `text/event-stream`) and guessing
 * wrong should not blank a build log.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import type { EventSourceLike, StreamStatus } from './stream';

export interface LogStreamOptions {
  /** The endpoint, already built. Null suspends the subscription. */
  url: string | null;
  /** Ring-buffer bound: a CUDA build prints tens of thousands of lines. */
  maxLines?: number;
  /** Lines already fetched over the paged endpoint, prepended once. */
  initialLines?: readonly string[];
  createSource?: (url: string) => EventSourceLike;
}

export interface LogStreamResult {
  lines: readonly string[];
  status: StreamStatus;
  clear: () => void;
}

function extractLine(data: string): string | null {
  if (!data) return null;
  if (data[0] !== '{') return data;
  try {
    const parsed: unknown = JSON.parse(data);
    if (typeof parsed === 'string') return parsed;
    if (typeof parsed === 'object' && parsed !== null) {
      const record = parsed as Record<string, unknown>;
      for (const key of ['line', 'message', 'text']) {
        const value = record[key];
        if (typeof value === 'string') return value;
      }
    }
  } catch {
    return data;
  }
  return data;
}

export function useLogStream({
  url,
  maxLines = 20_000,
  initialLines,
  createSource,
}: LogStreamOptions): LogStreamResult {
  const [lines, setLines] = useState<readonly string[]>(initialLines ?? []);
  const [status, setStatus] = useState<StreamStatus>('idle');
  const bufferRef = useRef<string[]>([...(initialLines ?? [])]);

  const clear = useCallback(() => {
    bufferRef.current = [];
    setLines([]);
  }, []);

  useEffect(() => {
    if (!url) {
      setStatus('idle');
      return;
    }
    setStatus('connecting');
    const source = createSource
      ? createSource(url)
      : (new EventSource(url, { withCredentials: true }) as unknown as EventSourceLike);

    source.onopen = () => setStatus('live');
    source.onerror = () => setStatus('degraded');
    source.addEventListener('message', (event: MessageEvent) => {
      const line = extractLine(typeof event.data === 'string' ? event.data : '');
      if (line === null) return;
      const buffer = bufferRef.current;
      // A frame may carry several lines; split so the viewer's row count stays exact.
      for (const one of line.split('\n')) buffer.push(one);
      if (buffer.length > maxLines) buffer.splice(0, buffer.length - maxLines);
      setLines([...buffer]);
    });

    return () => {
      source.close();
      setStatus('idle');
    };
  }, [url, maxLines, createSource]);

  return { lines, status, clear };
}
