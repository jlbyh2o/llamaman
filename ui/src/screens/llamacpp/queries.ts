/**
 * React Query hooks over DESIGN section 3.5's llama.cpp lifecycle endpoints.
 *
 * `llamacpp` is a real SSE topic (section 3.14), so list and detail reads use
 * `useLiveQueryOptions()` exactly like every other live area; a build's own progress additionally
 * arrives on its job (section 2.3), read through `features/system/jobs.ts`'s `useJob`.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type {
  JobReceipt,
  LlamacppPlan,
  LlamacppVersion,
  LlamacppVersionDetail,
  ListPage,
  Release,
} from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { asPage, compact } from '../../features/system/api';

export type LlamacppChannel = 'stable' | 'nightly' | 'custom';

/** `GET /llamacpp/active` — the running build, its options and the devices it can see. */
export function useActiveLlamacpp(): UseQueryResult<LlamacppVersionDetail, Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.llamacpp.active(),
    queryFn: () => api.get('/api/v1/llamacpp/active'),
    ...live,
  });
}

/** `GET /llamacpp/versions` — every build this host has, newest first. */
export function useLlamacppVersions(): UseQueryResult<ListPage<LlamacppVersion>, Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.llamacpp.versions(),
    queryFn: async () => asPage<LlamacppVersion>(await api.get('/api/v1/llamacpp/versions')),
    ...live,
  });
}

export function useLlamacppVersion(
  id: string | null,
): UseQueryResult<LlamacppVersionDetail, Error> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.llamacpp.detail(id ?? 'none'),
    queryFn: () => api.get('/api/v1/llamacpp/versions/{id}', { path: { id: id as string } }),
    enabled: id !== null,
    ...live,
  });
}

/** `GET /llamacpp/releases?channel=` — cached releases with the rendered changelog (D35). */
export function useLlamacppReleases(channel: 'stable' | 'nightly') {
  return useQuery({
    queryKey: queryKeys.llamacpp.releases(channel),
    queryFn: () => api.get('/api/v1/llamacpp/releases', { query: { channel } }),
    staleTime: 60_000,
  });
}

export interface PlanParams {
  channel?: LlamacppChannel;
  backend?: 'cpu' | 'cuda';
  tag?: string;
  git_url?: string;
  git_ref?: string;
  force_source?: boolean;
}

/** `GET /llamacpp/plan` — what installing this would do, before committing (section 6.3). */
export function useLlamacppPlan(
  params: PlanParams,
  enabled: boolean,
): UseQueryResult<LlamacppPlan, Error> {
  const query = compact(params);
  return useQuery({
    queryKey: queryKeys.llamacpp.plan(query),
    queryFn: () => api.get('/api/v1/llamacpp/plan', { query }),
    enabled,
    staleTime: 10_000,
  });
}

function invalidateLlamacpp(client: ReturnType<typeof useQueryClient>) {
  return Promise.all([
    client.invalidateQueries({ queryKey: queryKeys.family('llamacpp') }),
    client.invalidateQueries({ queryKey: queryKeys.family('jobs') }),
  ]);
}

export interface InstallVars {
  channel: LlamacppChannel;
  tag?: string;
  git_url?: string;
  git_ref?: string;
  backend?: 'cpu' | 'cuda';
  force_source?: boolean;
  force_rebuild?: boolean;
  cmake_extra?: string[];
}

/** `POST /llamacpp/versions` — install or build. D71's five branches all land as `200`/`202`. */
export function useInstallLlamacpp() {
  const client = useQueryClient();
  return useMutation({
    // `vars` is already clean at every call site (built with conditional spreads, never an explicit
    // `undefined`), and `channel` is required on the wire — `compact()` would make it optional and
    // no longer assignable to `InstallLlamacppRequest`.
    mutationFn: (vars: InstallVars) => api.post('/api/v1/llamacpp/versions', { body: vars }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

export function useCancelLlamacpp() {
  const client = useQueryClient();
  return useMutation<JobReceipt, Error, string>({
    mutationFn: (id) => api.post('/api/v1/llamacpp/versions/{id}/cancel', { path: { id } }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

export function useRetryLlamacpp() {
  const client = useQueryClient();
  return useMutation<JobReceipt, Error, string>({
    mutationFn: (id) => api.post('/api/v1/llamacpp/versions/{id}/retry', { path: { id } }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

export function useDeleteLlamacpp() {
  const client = useQueryClient();
  return useMutation<JobReceipt, Error, string>({
    mutationFn: (id) => api.delete('/api/v1/llamacpp/versions/{id}', { path: { id } }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

export interface ActivateVars {
  id: string;
  restart_instances: 'none' | 'rolling';
  canary_instance_id?: string;
}

/** `POST /llamacpp/versions/{id}/activate` — section 6.6's flip, plus the optional canary roll. */
export function useActivateLlamacpp() {
  const client = useQueryClient();
  return useMutation<JobReceipt, Error, ActivateVars>({
    mutationFn: ({ id, ...body }) =>
      api.post('/api/v1/llamacpp/versions/{id}/activate', { path: { id }, body: compact(body) }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

/** `POST /llamacpp/rollback` — the identical routine with `previous_active` as the target. */
export function useRollbackLlamacpp() {
  const client = useQueryClient();
  return useMutation<
    JobReceipt,
    Error,
    { restart_instances: 'none' | 'rolling'; canary_instance_id?: string }
  >({
    mutationFn: (body) => api.post('/api/v1/llamacpp/rollback', { body: compact(body) }),
    onSuccess: () => invalidateLlamacpp(client),
  });
}

export type { Release };
