/**
 * Local models and the Hugging Face cache — DESIGN section 3.7.
 *
 * Every hook here is a projection of DB rows the daemon owns, so the rules of section 4 apply
 * without exception: TanStack Query is the only cache, SSE frames patch it (`downloads` frames
 * carry the model rows a transfer is filling in), and nothing polls where a topic exists.
 *
 * The mutations are deliberately *not* optimistic. Section 4 allows optimism only for "cheap
 * toggles"; deleting a model, verifying one, or rescanning a cache root are all job-backed or
 * filesystem-backed, and showing a result that the daemon has not yet agreed to would be a lie
 * about the one thing this screen exists to report — what is actually on this disk.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type {
  CacheRoot,
  CacheScan,
  DeletePreview,
  ListPage,
  Model,
  ModelDetail,
  StrayFile,
} from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { asPage, compact } from './api';
import type { MODEL_KINDS, MODEL_FILTER_STATES, MODEL_SORTS } from './api';

export interface ModelFilters {
  q?: string | undefined;
  kind?: (typeof MODEL_KINDS)[number] | undefined;
  state?: (typeof MODEL_FILTER_STATES)[number] | undefined;
  sort?: (typeof MODEL_SORTS)[number] | undefined;
  root_id?: string | undefined;
}

/** `GET /api/v1/models` — the catalog, with `in_use_by` naming the instances holding each row. */
export function useModels(filters: ModelFilters = {}): UseQueryResult<ListPage<Model>, Error> {
  const live = useLiveQueryOptions();
  const query = compact({ ...filters });
  return useQuery({
    queryKey: queryKeys.models.list(query),
    queryFn: async () => asPage<Model>(await api.get('/api/v1/models', { query })),
    ...live,
  });
}

/** `GET /api/v1/models/{id}` — the row, its files, its projector and the projector picker. */
export function useModel(id: string): UseQueryResult<ModelDetail, Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.models.detail(id),
    queryFn: () => api.get('/api/v1/models/{id}', { path: { id } }),
    enabled: id !== '',
    ...live,
  });
}

/**
 * `GET /api/v1/models/{id}/metadata` — the full GGUF key/value map.
 *
 * Lazy by design: the daemon re-reads it from the file rather than retaining a tokenizer table it
 * will be asked about once, so this query is enabled only when the metadata pane is actually open.
 */
export function useModelMetadata(
  id: string,
  enabled: boolean,
): UseQueryResult<{ model_id: string; kv: Record<string, unknown> }, Error> {
  return useQuery({
    queryKey: queryKeys.models.metadata(id),
    queryFn: () => api.get('/api/v1/models/{id}/metadata', { path: { id } }),
    enabled: enabled && id !== '',
    staleTime: 5 * 60_000,
  });
}

/**
 * `GET /api/v1/models/{id}/delete-preview` — what deleting would actually free.
 *
 * D28's whole point is that the honest number is not the model's size: blobs are refcounted across
 * every snapshot in the repository, so a second quant of the same repo can keep most of them alive.
 * The dialog asks for this before it offers the button, never after.
 */
export function useDeletePreview(
  id: string,
  enabled: boolean,
): UseQueryResult<DeletePreview, Error> {
  return useQuery({
    queryKey: queryKeys.models.deletePreview(id),
    queryFn: () => api.get('/api/v1/models/{id}/delete-preview', { path: { id } }),
    enabled: enabled && id !== '',
    staleTime: 0,
  });
}

/** `DELETE /api/v1/models/{id}` — `409 model_in_use` names the instances that hold it. */
export function useDeleteModel() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete('/api/v1/models/{id}', { path: { id } }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('models') });
      await client.invalidateQueries({ queryKey: queryKeys.family('cache') });
    },
  });
}

/** `POST /api/v1/models/{id}/verify` — `202`, a job; the row's state carries the outcome. */
export function useVerifyModel() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post('/api/v1/models/{id}/verify', { path: { id } }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('models') });
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

/**
 * `POST /api/v1/models/{id}/pair-mmproj` — attach or detach a projector.
 *
 * It sets `mmproj_auto = 0` server-side, so a later scan never overrules the choice. The UI says
 * that in the panel rather than leaving the user to discover it.
 */
export function usePairMmproj() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; mmproj_model_id: string }) =>
      api.post('/api/v1/models/{id}/pair-mmproj', {
        path: { id: vars.id },
        body: { mmproj_model_id: vars.mmproj_model_id },
      }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('models') });
    },
  });
}

/* -- the cache itself ------------------------------------------------------ */

/** `GET /api/v1/cache/roots` — every known hub directory, primary first. */
export function useCacheRoots(): UseQueryResult<ListPage<CacheRoot>, Error> {
  return useQuery({
    queryKey: queryKeys.cache.roots(),
    queryFn: async () => asPage<CacheRoot>(await api.get('/api/v1/cache/roots')),
  });
}

/** `GET /api/v1/cache/strays` — files in a root that belong to no model, largest first. */
export function useStrays(): UseQueryResult<ListPage<StrayFile>, Error> {
  return useQuery({
    queryKey: queryKeys.cache.strays(),
    queryFn: async () => asPage<StrayFile>(await api.get('/api/v1/cache/strays')),
  });
}

/** `GET /api/v1/cache/scans/{id}` — polled only while a scan this session started is unfinished. */
export function useCacheScan(id: string | null): UseQueryResult<CacheScan, Error> {
  return useQuery({
    queryKey: queryKeys.cache.scan(id ?? 'none'),
    queryFn: () => api.get('/api/v1/cache/scans/{id}', { path: { id: id as string } }),
    enabled: id !== null,
    // A scan is a filesystem walk with no SSE topic of its own (section 3.14 lists eight, and
    // `cache` is not one), so this is the one place in the area that legitimately polls.
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state === 'queued' || state === 'running' ? 1000 : false;
    },
  });
}

/** `POST /api/v1/cache/scan` — `202`, a job. Makes no network calls; it reconciles the catalog. */
export function useScanCache() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (rootId?: string) =>
      api.post('/api/v1/cache/scan', { body: rootId ? { root_id: rootId } : {} }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('jobs') });
    },
  });
}

/** `POST /api/v1/cache/strays/{id}/dismiss` — hide it without touching the file. */
export function useDismissStray() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post('/api/v1/cache/strays/{id}/dismiss', { path: { id } }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.cache.strays() });
    },
  });
}

/** `DELETE /api/v1/cache/strays/{id}` — forget it, and optionally unlink the file it names. */
export function useDeleteStray() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; deleteFile: boolean }) =>
      api.delete('/api/v1/cache/strays/{id}', {
        path: { id: vars.id },
        query: { delete_file: vars.deleteFile },
      }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: queryKeys.family('cache') });
      await client.invalidateQueries({ queryKey: queryKeys.family('system') });
    },
  });
}
