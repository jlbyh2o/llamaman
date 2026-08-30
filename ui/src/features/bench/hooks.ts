/**
 * Bench data hooks.
 *
 * One React Query wrapper per section 3.13 endpoint. List and detail queries key off
 * `queryKeys.bench.*`, which is what lets the generic SSE patcher (`events/patch.ts`, topic
 * `bench`) keep them live without a polling loop — `useLiveQueryOptions()` is the fallback for a
 * degraded stream, never the primary mechanism.
 *
 * `Data<'…'>` resolves several list endpoints through a generated type the codegen mangled into an
 * unreachable bracket chain (`schema.d.ts`'s `"List[github.com" ]["jlbyh2o"]…` — a real quirk of this
 * checkout's `openapi-typescript` output, not something to fix here); those calls are cast through
 * `ListPage<T>`, which is the shape the DTO actually has on the wire.
 */

import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type {
  BenchCompare,
  BenchPreflight,
  BenchResultRow,
  BenchRun,
  BenchRunDetail,
  BenchSeries,
  BenchState,
  ListPage,
  Model,
} from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import type { Sweep } from './sweep';
import { encodeSweep } from './sweep';

/** Debounce a fast-changing value (sweep edits, the model picker) before it drives a network call. */
export function useDebouncedValue<T>(value: T, delayMs = 400): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

export interface BenchRunsFilters {
  model_id?: string;
  state?: BenchState;
  limit?: number;
}

export function useBenchRuns(filters: BenchRunsFilters = {}): UseQueryResult<ListPage<BenchRun>> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.bench.runs(filters as Record<string, unknown>),
    queryFn: async () =>
      (await api.get('/api/v1/bench/runs', { query: filters })) as unknown as ListPage<BenchRun>,
    ...live,
  });
}

export function useBenchRun(id: string | undefined): UseQueryResult<BenchRunDetail> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.bench.detail(id ?? ''),
    queryFn: () => api.get('/api/v1/bench/runs/{id}', { path: { id: id ?? '' } }),
    enabled: id !== undefined,
    ...live,
  });
}

export function useBenchRunResults(
  id: string | undefined,
): UseQueryResult<ListPage<BenchResultRow>> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.bench.results(id ?? ''),
    queryFn: async () =>
      (await api.get('/api/v1/bench/runs/{id}/results', {
        path: { id: id ?? '' },
      })) as unknown as ListPage<BenchResultRow>,
    enabled: id !== undefined,
    ...live,
  });
}

export interface PreflightParams {
  modelId: string | undefined;
  sweep: Sweep;
  repetitions: number;
}

/** `GET /bench/preflight` — the live "N points ≈ M minutes" estimate and the conflict list. */
export function usePreflight(params: PreflightParams): UseQueryResult<BenchPreflight> {
  const sweepJson = encodeSweep(params.sweep);
  const query = {
    model_id: params.modelId ?? '',
    repetitions: params.repetitions,
    ...(sweepJson ? { sweep: sweepJson } : {}),
  };
  return useQuery({
    queryKey: queryKeys.bench.preflight(query),
    queryFn: () => api.get('/api/v1/bench/preflight', { query }),
    enabled: Boolean(params.modelId),
    // Preflight is a snapshot of "right now" — GPU load, other instances — never worth caching
    // across edits, and cheap enough to re-ask on every debounced keystroke.
    staleTime: 0,
    retry: false,
  });
}

export interface CreateBenchRunInput {
  modelId: string;
  name?: string;
  sweep: Sweep;
  repetitions: number;
  onConflict: 'stop_and_restore' | 'abort';
  draft: boolean;
}

export function useCreateBenchRun() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateBenchRunInput) =>
      api.post('/api/v1/bench/runs', {
        body: {
          model_id: input.modelId,
          ...(input.name ? { name: input.name } : {}),
          ...(() => {
            const s = encodeSweep(input.sweep);
            return s ? { sweep: s } : {};
          })(),
          repetitions: input.repetitions,
          on_conflict: input.onConflict,
          draft: input.draft,
        },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('bench') });
    },
  });
}

export function useStartBenchRun() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post('/api/v1/bench/runs/{id}/start', { path: { id } }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('bench') });
    },
  });
}

export function useCancelBenchRun() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post('/api/v1/bench/runs/{id}/cancel', { path: { id } }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('bench') });
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('instances') });
    },
  });
}

export function useDeleteBenchRun() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete('/api/v1/bench/runs/{id}', { path: { id } }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('bench') });
    },
  });
}

export function usePatchBenchRun(id: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: { name?: string; notes?: string | null }) =>
      api.patch('/api/v1/bench/runs/{id}', { path: { id }, body }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('bench') });
    },
  });
}

export interface CompareParams {
  runIds: string[];
  x: string;
  y: string;
  series?: string;
  filters?: Record<string, string>;
}

/** `POST /bench/compare` — a mutation by transport, a read by nature, so it is wrapped as a query. */
export function useBenchCompare(params: CompareParams | undefined): UseQueryResult<BenchCompare> {
  return useQuery({
    queryKey: queryKeys.list('bench', { kind: 'compare', ...params }),
    queryFn: () =>
      api.post('/api/v1/bench/compare', {
        body: {
          run_ids: params?.runIds ?? [],
          x: params?.x ?? '',
          y: params?.y ?? '',
          ...(params?.series ? { series: params.series } : {}),
          ...(params?.filters ? { filters: params.filters } : {}),
        },
      }),
    enabled: Boolean(params && params.runIds.length > 0 && params.x && params.y),
  });
}

/** `?group=` values `GET /bench/series` accepts — every sweep axis plus the run-identity columns. */
export type BenchSeriesGroup =
  | 'flash_attn'
  | 'llamacpp_backend'
  | 'llamacpp_tag'
  | 'model_label'
  | 'n_batch'
  | 'n_depth'
  | 'n_gen'
  | 'n_gpu_layers'
  | 'n_prompt'
  | 'n_threads'
  | 'n_ubatch'
  | 'quant_label'
  | 'run_id'
  | 'run_name'
  | 'split_mode'
  | 'tensor_split'
  | 'test_kind'
  | 'type_k'
  | 'type_v';

export interface SeriesFilters {
  model_id?: string;
  test?: 'pp' | 'tg' | 'pp+tg';
  metric?: 'avg_ns' | 'avg_ts' | 'stddev_ns' | 'stddev_ts';
  group?: BenchSeriesGroup;
  limit?: number;
}

/** The models a sweep or a history filter can pick from — the ones with a file on disk. */
export function useReadyModels(): UseQueryResult<ListPage<Model>> {
  return useQuery({
    queryKey: queryKeys.models.list({ state: 'ready' }),
    queryFn: async () =>
      (await api.get('/api/v1/models', {
        query: { state: 'ready' },
      })) as unknown as ListPage<Model>,
  });
}

export function useBenchSeries(filters: SeriesFilters): UseQueryResult<BenchSeries> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.bench.series(filters as Record<string, unknown>),
    queryFn: () => api.get('/api/v1/bench/series', { query: filters }),
    ...live,
  });
}
