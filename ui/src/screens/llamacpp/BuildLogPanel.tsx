/**
 * The build log (DESIGN section 4 screen 12: "a virtualized build-log viewer — ANSI-stripped,
 * auto-scrolling, failing step highlighted, jump-to-first-error").
 *
 * `LogViewer` already does the ANSI-stripping, virtualization, follow and jump-to-first-error; this
 * component's whole job is deciding where the lines come from — the paged JSON body while there is
 * nothing live to watch, `EventSource` (which sends `Accept: text/event-stream` itself, so no header
 * needs setting by hand) for a build still running.
 */

import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, API_BASE } from '../../api/client';
import { queryKeys } from '../../api/keys';
import { LogViewer } from '../../components';
import { useLogStream } from '../../events/useLogStream';
import type { LlamacppState } from '../../api/types';

const LIVE_STATES: readonly LlamacppState[] = [
  'pending',
  'resolving',
  'fetching',
  'building',
  'verifying',
];

export function BuildLogPanel({ id, state }: { id: string; state: LlamacppState }) {
  const live = LIVE_STATES.includes(state);

  const page = useQuery({
    queryKey: queryKeys.llamacpp.log(id),
    queryFn: () =>
      api.get('/api/v1/llamacpp/versions/{id}/log', {
        path: { id },
        query: { limit: 262_144 },
      }),
    staleTime: live ? 0 : 30_000,
  });

  // The operation's success type unions all three `Accept`-selected bodies (LogPageDTO | string),
  // since `openapi-typescript` has no way to key one content type to the header a caller sends —
  // narrow at runtime even though `Accept: application/json` (sent by every typed request) always
  // gets the JSON envelope in practice.
  const initialLines = useMemo(() => {
    if (!page.data) return [];
    return (typeof page.data === 'string' ? page.data : page.data.text).split('\n');
  }, [page.data]);

  const url = live ? `${API_BASE}/llamacpp/versions/${encodeURIComponent(id)}/log?tail=300` : null;
  const stream = useLogStream({ url, initialLines });

  return (
    <div className="rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)]">
      <LogViewer
        lines={live ? stream.lines : initialLines}
        rows={32}
        aria-label="Build log"
        toolbar={
          <span>
            {live
              ? stream.status === 'live'
                ? 'Following build output'
                : stream.status === 'degraded'
                  ? 'Reconnecting…'
                  : 'Connecting…'
              : `${initialLines.length} lines`}
          </span>
        }
      />
    </div>
  );
}
