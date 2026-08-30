/**
 * React Query hooks over `system/api.ts` — DESIGN section 3.3 and section 3.4.
 *
 * Section 3.14's SSE topic list has no `system` or `settings` entry; `gpu` and `notifications` are
 * the two exceptions patch.ts already routes into the `system` family (`GET /system/gpus` and
 * `GET /system/notifications`), so those two use `useLiveQueryOptions()` like every other live area
 * and everything else here is fetch-once-and-invalidate, exactly like `features/models/queries.ts`
 * treats `GET /cache/scans/{id}` — "a filesystem walk with no SSE topic of its own" — for the same
 * reason. Disk usage gets a light interval poll for the same precedent: nothing invalidates it on
 * its own, and free space is worth noticing without a manual click.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { queryKeys } from '../../api/keys';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { systemApi, settingsApi } from './api';
import type {
  Capabilities,
  DiskUsageEntry,
  Gpu,
  JournalLine,
  PatchSettingsResponse,
  RestartResponse,
  SettingsResponse,
  SystemInfo,
  SystemNotification,
  ToolchainCheck,
  UnitStatus,
} from './types';

/* -- reads -------------------------------------------------------------------- */

export function useSystemInfo(): UseQueryResult<SystemInfo, Error> {
  return useQuery({
    queryKey: queryKeys.system.info(),
    queryFn: ({ signal }) => systemApi.info(signal),
    staleTime: 15_000,
  });
}

export function useCapabilities(): UseQueryResult<Capabilities, Error> {
  return useQuery({
    queryKey: queryKeys.system.capabilities(),
    queryFn: ({ signal }) => systemApi.capabilities(signal),
    staleTime: 15_000,
  });
}

export function useToolchain(): UseQueryResult<ToolchainCheck[], Error> {
  return useQuery({
    queryKey: queryKeys.system.toolchain(),
    queryFn: ({ signal }) => systemApi.toolchain(signal),
    staleTime: 15_000,
  });
}

export function useGpus(): UseQueryResult<Gpu[], Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.system.gpus(),
    queryFn: ({ signal }) => systemApi.gpus(signal),
    ...live,
  });
}

export function useDisk(): UseQueryResult<DiskUsageEntry[], Error> {
  return useQuery({
    queryKey: queryKeys.system.disk(),
    queryFn: ({ signal }) => systemApi.disk(signal),
    refetchInterval: 30_000,
  });
}

export function useUnits(): UseQueryResult<UnitStatus[], Error> {
  return useQuery({
    queryKey: queryKeys.system.units(),
    queryFn: ({ signal }) => systemApi.units(signal),
    staleTime: 30_000,
  });
}

export function useNotifications(): UseQueryResult<SystemNotification[], Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.notifications.list(),
    queryFn: ({ signal }) => systemApi.notifications(signal),
    ...live,
  });
}

/** A single page of the journal (`GET /system/journal`). Read on demand, never polled. */
export function useJournalPage(
  params: { unit?: string; lines?: number },
  enabled: boolean,
): UseQueryResult<JournalLine[], Error> {
  return useQuery({
    queryKey: [...queryKeys.family('system'), 'journal', params],
    queryFn: ({ signal }) => systemApi.journal(params, signal),
    enabled,
    staleTime: 5_000,
  });
}

/* -- mutations ------------------------------------------------------------------ */

export function useProbeToolchain() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => systemApi.probeToolchain(),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

export function useDismissNotification() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => systemApi.dismissNotification(id),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.notifications.list() });
    },
  });
}

/**
 * `POST /system/restart` — section 3.3's `409`/`429` states are not failures of this mutation, they
 * are its two documented outcomes, so callers read `error.code` (`systemd_denied`,
 * `restart_unavailable`, `job_in_flight`) and `error.retryAfterMs` (`restart_rate_limited`, D93)
 * rather than treating every rejection the same way. Nothing is invalidated on success: the request
 * that lands it is answered by the daemon that is about to stop serving (section 9.4), and
 * `GET /api/v1/meta` — not this hook — is what the shell polls to notice the new one.
 */
export function useRestartDaemon() {
  return useMutation<RestartResponse, Error, void>({
    mutationFn: () => systemApi.restart(),
  });
}

/* -- settings -------------------------------------------------------------------- */

export function useSettings(): UseQueryResult<SettingsResponse, Error> {
  return useQuery({
    queryKey: queryKeys.settings.all(),
    queryFn: ({ signal }) => settingsApi.get(signal),
    staleTime: 15_000,
  });
}

export function usePatchSettings() {
  const client = useQueryClient();
  return useMutation<PatchSettingsResponse, Error, Record<string, unknown>>({
    mutationFn: (values) => settingsApi.patch(values),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.settings.all() });
    },
  });
}

export function useResetSettings() {
  const client = useQueryClient();
  return useMutation<{ values: Record<string, unknown> }, Error, string[]>({
    mutationFn: (keys) => settingsApi.reset(keys),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.settings.all() });
    },
  });
}
