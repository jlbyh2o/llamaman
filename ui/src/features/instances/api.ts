/**
 * Instance data hooks.
 *
 * Two rules shape this file.
 *
 * **The list is the live row.** `GET /api/v1/instances` is cached under `queryKeys.instances.list`,
 * which is exactly what `events/patch.ts` rewrites when an `instance.status` frame arrives — so the
 * table, the dashboard and the detail header all read one query that SSE keeps current, and nothing
 * polls (DESIGN section 4). The detail read (`GET /instances/{id}`, which carries the rendered argv
 * and the start ledger) is cached one level deeper, under `[…detail, id, 'full']`, deliberately: a
 * status patch is `InstanceStatusDTO`-shaped and is merged into the *entity* key, and merging it
 * into a `{instance, argv, starts}` envelope would write a phantom `status` at its root.
 *
 * **Every route is typed.** Section 3.10 specifies fifteen instance routes and `api/openapi.json`
 * now carries all of them, so every call here goes through the generated client — the path, its
 * params, its body and its response all checked at compile time (DESIGN section 4, D43). They used
 * to go through a `raw()` helper that cast the path and method to `never`, because the document did
 * not know these routes existed; `isRouteMissing()` survives that era and still turns a 404 into a
 * sentence rather than a red error, which is the right behavior for a daemon older than this UI.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult, UseQueryResult } from '@tanstack/react-query';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type {
  ApiToken,
  FitReport,
  Instance,
  InstanceDetail,
  ListPage,
  Model,
} from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import type {
  FlagPreset,
  InstanceCommand,
  InstanceLogLine,
  InstanceUsage,
  PortSuggestion,
  PresetInput,
  ServerSlot,
  ValidateResponse,
} from './types';

/* -------------------------------------------------------------------------- */
/* plumbing                                                                    */
/* -------------------------------------------------------------------------- */

/**
 * A 404 from one of the ahead-of-contract routes.
 *
 * Ambiguous in principle — `404 not_found` is also how a missing instance answers — and unambiguous
 * in practice at every call site here: the id came from a row this screen had just read, so the
 * route is what is missing. The screens use it to say so plainly rather than to hide the failure.
 */
export function isRouteMissing(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404;
}

function asPage<T>(value: unknown): ListPage<T> {
  if (Array.isArray(value)) return { items: value as T[], total: value.length, next_cursor: null };
  if (typeof value !== 'object' || value === null) return { items: [], total: 0 };
  const record = value as Record<string, unknown>;
  if (!Array.isArray(record['items'])) return { items: [], total: 0 };
  const items = record['items'] as T[];
  return {
    items,
    total: typeof record['total'] === 'number' ? record['total'] : items.length,
    next_cursor: typeof record['next_cursor'] === 'string' ? record['next_cursor'] : null,
  };
}

/** Debounce a value before it drives a network call — the fit panel's every-keystroke problem. */
export function useDebouncedValue<T>(value: T, delayMs = 400): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

/** The detail envelope's key: a sub-resource of the entity, so entity patches never land on it. */
const detailKey = (id: string) => [...queryKeys.instances.detail(id), 'full'];

/* -------------------------------------------------------------------------- */
/* reads                                                                       */
/* -------------------------------------------------------------------------- */

export function useInstances(includeDeleted = false): UseQueryResult<Instance[]> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.instances.list(includeDeleted ? { include_deleted: true } : undefined),
    queryFn: async () => {
      const page = await api.get('/api/v1/instances', {
        ...(includeDeleted ? { query: { include_deleted: true } } : {}),
      });
      return asPage<Instance>(page).items;
    },
    ...live,
  });
}

/**
 * One instance's live row, read out of the list query.
 *
 * Not a second request: the list is the query SSE patches, so a row taken from it is as current as
 * the stream. `undefined` while the list is loading, and for an id the list does not carry — a
 * soft-deleted instance, which the detail screen then fetches with `?include_deleted=true`.
 */
export function useInstanceRow(id: string, includeDeleted = false): Instance | undefined {
  const list = useInstances(includeDeleted);
  return useMemo(() => list.data?.find((row) => row.id === id), [list.data, id]);
}

/** `GET /api/v1/instances/{id}` — the row plus rendered argv, unknown flags and the last starts. */
export function useInstanceDetail(id: string): UseQueryResult<InstanceDetail> {
  return useQuery({
    queryKey: detailKey(id),
    queryFn: () => api.get('/api/v1/instances/{id}', { path: { id } }),
    enabled: id !== '',
  });
}

/**
 * Keep the detail envelope honest against the live row.
 *
 * `argv`, `unknown_flags` and the start ledger are not patchable from a status frame, so they are
 * re-read when the facts that could have changed them change: the state, the config hash, and the
 * generation. That is an invalidation driven by the stream, not a polling loop.
 */
export function useDetailFreshness(id: string, row: Instance | undefined): void {
  const queryClient = useQueryClient();
  const signature = row
    ? `${row.status.state}|${row.config_hash}|${row.generation}|${row.status.main_pid ?? ''}`
    : '';
  useEffect(() => {
    if (id === '' || signature === '') return;
    void queryClient.invalidateQueries({ queryKey: detailKey(id) });
  }, [queryClient, id, signature]);
}

/** The local model catalog, for the three model pickers. */
export function useModels(): UseQueryResult<Model[]> {
  return useQuery({
    queryKey: queryKeys.models.list(),
    queryFn: async () => asPage<Model>(await api.get('/api/v1/models')).items,
    staleTime: 60_000,
  });
}

/** Tokens, so the detail screen can show which of them may reach this instance (section 2.9). */
export function useTokens(): UseQueryResult<ApiToken[]> {
  return useQuery({
    queryKey: queryKeys.tokens.list(),
    queryFn: async () => asPage<ApiToken>(await api.get('/api/v1/tokens')).items,
    staleTime: 60_000,
  });
}

export interface FitRequest {
  model_id: string;
  flags: Record<string, unknown>;
  gpus?: string[];
}

/**
 * `POST /api/v1/fit/estimate`, the live panel beside the form.
 *
 * A POST behind a query key on purpose: it is a pure projection of the body (section 3.9), so
 * caching it by that body is exactly right and re-estimating on every keystroke would not be.
 */
export function useFitEstimate(
  input: FitRequest | null,
  enabled = true,
): UseQueryResult<FitReport> {
  const key = input ? JSON.stringify(input) : '';
  return useQuery({
    queryKey: queryKeys.list('fit', { kind: 'estimate', key }),
    queryFn: () =>
      api.post('/api/v1/fit/estimate', {
        body: {
          source: { model_id: input?.model_id ?? '' },
          flags: (input?.flags ?? {}) as never,
          ...(input?.gpus && input.gpus.length > 0 ? { gpus: input.gpus } : {}),
          reserve_bytes_per_gpu: 0,
        },
      }),
    enabled: enabled && input !== null && input.model_id !== '',
    retry: false,
    staleTime: 30_000,
  });
}

/** `POST /api/v1/instances/validate` — the dry run behind the argv preview. */
export function useValidateInstance(
  body: Record<string, unknown> | null,
): UseQueryResult<ValidateResponse> {
  const key = body ? JSON.stringify(body) : '';
  return useQuery({
    queryKey: queryKeys.list('instances', { kind: 'validate', key }),
    queryFn: () => api.post('/api/v1/instances/validate', { body: body as never }),
    enabled: body !== null,
    retry: false,
    staleTime: 30_000,
  });
}

export function useInstanceCommand(id: string): UseQueryResult<InstanceCommand> {
  return useQuery({
    queryKey: queryKeys.instances.command(id),
    queryFn: () => api.get('/api/v1/instances/{id}/command', { path: { id } }),
    enabled: id !== '',
    retry: false,
  });
}

export function useInstanceUsage(id: string, days = 14): UseQueryResult<InstanceUsage> {
  const from = useMemo(() => {
    const date = new Date(Date.now() - days * 86_400_000);
    return date.toISOString().slice(0, 10);
  }, [days]);
  return useQuery({
    queryKey: queryKeys.instances.usage(id, { from }),
    queryFn: () => api.get('/api/v1/instances/{id}/usage', { path: { id }, query: { from } }),
    enabled: id !== '',
    retry: false,
  });
}

/**
 * llama-server's own `/slots`, proxied.
 *
 * `signature` is the part of the instance's status that changes when its slots do — the counters the
 * supervisor writes and the timestamp beside them. Putting it in the key is what makes an SSE status
 * patch re-read the slot detail: the stream says "something moved", and the read that follows is the
 * only way to learn *what*, since no topic carries per-slot state (DESIGN section 3.14).
 */
export function useInstanceSlots(
  id: string,
  enabled: boolean,
  signature = '',
): UseQueryResult<ServerSlot[]> {
  const live = useLiveQueryOptions(5_000);
  return useQuery({
    queryKey: [...queryKeys.instances.detail(id), 'slots', signature],
    queryFn: async () => {
      // `json` is `unknown` by contract — llama.cpp owns this shape, not us — so the narrowing
      // happens here, once, rather than in the component that renders the slots.
      const { json } = await api.get('/api/v1/instances/{id}/slots', { path: { id } });
      return Array.isArray(json) ? (json as ServerSlot[]) : asPage<ServerSlot>(json).items;
    },
    enabled: enabled && id !== '',
    retry: false,
    ...live,
  });
}

/** The first page of the journal, before the SSE tail takes over. */
export function useInstanceLogPage(
  id: string,
  enabled = true,
  lines = 500,
): UseQueryResult<string[]> {
  return useQuery({
    queryKey: [...queryKeys.instances.detail(id), 'logs', lines],
    queryFn: async () => {
      const page = await api.get('/api/v1/instances/{id}/logs', {
        path: { id },
        query: { lines },
      });
      return asPage<InstanceLogLine>(page).items.map((line) => line.message);
    },
    enabled: enabled && id !== '',
    retry: false,
    staleTime: Infinity,
  });
}

/** The live tail's URL. `EventSource` sets `Accept: text/event-stream`, which is what selects it. */
export function instanceLogStreamUrl(id: string): string {
  return `/api/v1/instances/${encodeURIComponent(id)}/logs?follow=true`;
}

export function usePresets(): UseQueryResult<FlagPreset[]> {
  return useQuery({
    queryKey: queryKeys.list('presets'),
    queryFn: async () => asPage<FlagPreset>(await api.get('/api/v1/presets')).items,
    retry: false,
    staleTime: 60_000,
  });
}

export function useSuggestedPort(
  kind: 'public' | 'internal',
  enabled: boolean,
): UseQueryResult<PortSuggestion> {
  return useQuery({
    queryKey: queryKeys.list('instances', { kind: 'ports-suggest', port_kind: kind }),
    queryFn: () => api.get('/api/v1/ports/suggest', { query: { kind } }),
    enabled,
    retry: false,
    staleTime: 10_000,
  });
}

/* -------------------------------------------------------------------------- */
/* writes                                                                      */
/* -------------------------------------------------------------------------- */

/** Everything under `['instances']`: the list, the detail envelope and every sub-resource. */
function useInvalidateInstances() {
  const queryClient = useQueryClient();
  return useCallback(
    () => queryClient.invalidateQueries({ queryKey: queryKeys.family('instances') }),
    [queryClient],
  );
}

export interface CreateResult {
  instance: Instance;
  warnings: { code: string; message: string }[];
}

export function useCreateInstance(): UseMutationResult<
  CreateResult,
  Error,
  Record<string, unknown>
> {
  const invalidate = useInvalidateInstances();
  return useMutation({
    mutationFn: async (body: Record<string, unknown>) =>
      (await api.post('/api/v1/instances', { body: body as never })) as unknown as CreateResult,
    onSuccess: () => void invalidate(),
  });
}

export function usePatchInstance(
  id: string,
): UseMutationResult<CreateResult, Error, Record<string, unknown>> {
  const invalidate = useInvalidateInstances();
  return useMutation({
    mutationFn: async (body: Record<string, unknown>) =>
      (await api.patch('/api/v1/instances/{id}', {
        path: { id },
        body: body as never,
      })) as unknown as CreateResult,
    onSuccess: () => void invalidate(),
  });
}

export interface DeleteVariables {
  purge?: boolean;
  keepTokens?: boolean;
}

export function useDeleteInstance(id: string) {
  const invalidate = useInvalidateInstances();
  return useMutation({
    mutationFn: ({ purge = false, keepTokens = false }: DeleteVariables) =>
      api.delete('/api/v1/instances/{id}', {
        path: { id },
        query: { purge, keep_tokens: keepTokens },
      }),
    onSuccess: () => void invalidate(),
  });
}

/** The five control verbs of section 3.10, each a `202` that the stream then narrates. */
export type ControlAction = 'start' | 'stop' | 'restart' | 'safe-start' | 'reset-failed';

export function useInstanceControl(id: string) {
  const invalidate = useInvalidateInstances();
  return useMutation({
    mutationFn: ({ action, drainSec }: { action: ControlAction; drainSec?: number }) => {
      // Five paths rather than one interpolated string: the generated client checks each against
      // `paths`, so a verb this daemon does not serve is a compile error rather than a 404.
      switch (action) {
        case 'start':
          return api.post('/api/v1/instances/{id}/start', { path: { id } });
        case 'stop':
          return api.post('/api/v1/instances/{id}/stop', {
            path: { id },
            ...(drainSec !== undefined ? { query: { drain_sec: drainSec } } : {}),
          });
        case 'restart':
          return api.post('/api/v1/instances/{id}/restart', { path: { id } });
        case 'safe-start':
          return api.post('/api/v1/instances/{id}/safe-start', { path: { id } });
        case 'reset-failed':
          return api.post('/api/v1/instances/{id}/reset-failed', { path: { id } });
      }
    },
    onSuccess: () => void invalidate(),
  });
}

/**
 * `PUT /instances/{id}/autostart` — enable or disable the unit, and nothing else.
 *
 * One of the app's two optimistic mutations (DESIGN section 4): the toggle moves at once and snaps
 * back if the daemon refuses, which it does with `409 autostart_unavailable` when the
 * `manage-unit-files` grant was withheld and with `409 systemd_unavailable` in the F10 degraded
 * mode — both carrying the exact `systemctl` line to run by hand.
 */
export function useAutostart() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.put('/api/v1/instances/{id}/autostart', { path: { id }, body: { enabled } }),
    onMutate: async ({ id, enabled }) => {
      await queryClient.cancelQueries({ queryKey: [...queryKeys.family('instances'), 'list'] });
      const snapshot = queryClient.getQueriesData({ queryKey: ['instances', 'list'] });
      queryClient.setQueriesData({ queryKey: ['instances', 'list'] }, (prev: unknown) =>
        Array.isArray(prev)
          ? prev.map((row) =>
              typeof row === 'object' && row !== null && (row as Instance).id === id
                ? { ...(row as Instance), autostart: enabled }
                : row,
            )
          : prev,
      );
      return { snapshot };
    },
    onError: (_error, _variables, context) => {
      for (const [key, value] of context?.snapshot ?? []) queryClient.setQueryData(key, value);
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('instances') });
    },
  });
}

/** `POST /instances/{id}/pin-ngl` — turn the calculator's advisory into a saved count (D51). */
export function usePinNgl(id: string) {
  const invalidate = useInvalidateInstances();
  return useMutation({
    mutationFn: () => api.post('/api/v1/instances/{id}/pin-ngl', { path: { id } }),
    onSuccess: () => void invalidate(),
  });
}

export function useSavePreset() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: PresetInput) => api.post('/api/v1/presets', { body }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('presets') });
    },
  });
}
