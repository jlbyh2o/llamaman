/**
 * React Query hooks over DESIGN section 3.14 / section 12's self-update endpoints.
 *
 * `update` is a family in `api/keys.ts` but not one of section 3.14's eight SSE topics, so nothing
 * patches it live — the whole point of section 12.3's confirmation gate is that the daemon may be
 * unreachable for the seconds a restart takes, and `GET /update/status` is what the UI polls through
 * that gap rather than trusting a stream that just went down for the very reason being watched.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { ListPage, UpdateRelease, UpdateStatus } from '../../api/types';
import { asPage } from '../../features/system/api';

/** Section 2.11's self-update states that mean "still working". */
const IN_FLIGHT_STATES = new Set(['planned', 'downloading', 'verifying', 'staged', 'swapping']);

export function isUpdateInFlight(status: UpdateStatus | undefined): boolean {
  const state = status?.in_flight?.state;
  return Boolean(status?.pending) || (state !== undefined && IN_FLIGHT_STATES.has(state));
}

/**
 * `GET /update/status`. Polls every 2s while a swap is in flight — through the daemon's own restart
 * (section 9.4's fd-store hand-off keeps gateway ports open, but not the management API) — and every
 * 60s otherwise, since nothing else invalidates it.
 */
export function useUpdateStatus(): UseQueryResult<UpdateStatus, Error> {
  return useQuery({
    queryKey: queryKeys.update.status(),
    queryFn: () => api.get('/api/v1/update/status'),
    refetchInterval: (query) => (isUpdateInFlight(query.state.data) ? 2000 : 60_000),
    // A restart makes the daemon briefly unreachable by design (section 9.4) — that is data, not a
    // reason to stop polling, so retries keep going rather than the default two-and-give-up.
    retry: (count) => count < 30,
    retryDelay: 2000,
  });
}

export function useUpdateReleases(): UseQueryResult<ListPage<UpdateRelease>, Error> {
  return useQuery({
    queryKey: queryKeys.update.releases(),
    queryFn: async () => asPage<UpdateRelease>(await api.get('/api/v1/update/releases')),
    staleTime: 60_000,
  });
}

export function useCheckUpdates() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => api.post('/api/v1/update/check'),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('update') });
    },
  });
}

/**
 * `POST /update/apply`. Section 3.14's four-clause guard and section 12.4's schema warning both
 * arrive as this call's ordinary answers, not exceptions to it — see `UpdatesPanel` for how each is
 * shown.
 */
export function useApplyUpdate() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (tag: string) => api.post('/api/v1/update/apply', { body: { tag } }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.update.status() });
    },
  });
}
