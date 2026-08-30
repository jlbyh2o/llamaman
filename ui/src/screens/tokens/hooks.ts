/**
 * Token and gateway data hooks (DESIGN section 3.12, section 2.9).
 *
 * Like `features/bench/hooks.ts`, several of these cast through `ListPage<T>` because the generated
 * `schema.d.ts` mangles the shared `List[github.com/…]` generic into an unreachable bracket chain for
 * every *usage* site (the type still exists correctly under `components["schemas"]`) — a quirk of
 * this checkout's `openapi-typescript` output, not a reason to leave these calls untyped.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type {
  ApiToken,
  ApiTokenDetail,
  GatewayDenial,
  Instance,
  ListPage,
  TokenState,
  TokenUsage,
} from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';

export interface TokensFilters {
  q?: string;
  state?: TokenState;
}

/** `GET /tokens` returns every token unfiltered; the search box and state filter are client-side. */
export function useTokens(): UseQueryResult<ListPage<ApiToken>> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.tokens.list(),
    queryFn: async () => (await api.get('/api/v1/tokens')) as unknown as ListPage<ApiToken>,
    ...live,
  });
}

export function useToken(id: string | undefined): UseQueryResult<ApiTokenDetail> {
  return useQuery({
    queryKey: queryKeys.tokens.detail(id ?? ''),
    queryFn: () => api.get('/api/v1/tokens/{id}', { path: { id: id ?? '' } }),
    enabled: id !== undefined,
  });
}

export function useTokenUsage(
  id: string | undefined,
  range: { from?: string; to?: string; group?: 'day' | 'instance' } = {},
): UseQueryResult<ListPage<TokenUsage>> {
  return useQuery({
    queryKey: queryKeys.tokens.usage(id ?? '', range),
    queryFn: async () =>
      (await api.get('/api/v1/tokens/{id}/usage', {
        path: { id: id ?? '' },
        query: range,
      })) as unknown as ListPage<TokenUsage>,
    enabled: id !== undefined,
  });
}

export interface CreateTokenInput {
  name: string;
  scope: 'global' | 'instances';
  instanceIds?: string[];
  rateLimitRpm?: number;
  expiresAt?: string;
}

export function useCreateToken() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: CreateTokenInput) =>
      api.post('/api/v1/tokens', {
        body: {
          name: input.name,
          scope: input.scope,
          ...(input.instanceIds && input.instanceIds.length > 0
            ? { instance_ids: input.instanceIds }
            : {}),
          ...(input.rateLimitRpm ? { rate_limit_rpm: input.rateLimitRpm } : {}),
          ...(input.expiresAt ? { expires_at: input.expiresAt } : {}),
        },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('tokens') });
    },
  });
}

export interface PatchTokenInput {
  name?: string;
  state?: 'active' | 'disabled';
  instanceIds?: string[];
  rateLimitRpm?: number | null;
}

export function usePatchToken(id: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: PatchTokenInput) =>
      api.patch('/api/v1/tokens/{id}', {
        path: { id },
        body: {
          ...(input.name !== undefined ? { name: input.name } : {}),
          ...(input.state !== undefined ? { state: input.state } : {}),
          ...(input.instanceIds !== undefined ? { instance_ids: input.instanceIds } : {}),
          ...(input.rateLimitRpm !== undefined ? { rate_limit_rpm: input.rateLimitRpm } : {}),
        },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('tokens') });
    },
  });
}

export function useRevokeToken() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.delete('/api/v1/tokens/{id}', { path: { id } }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('tokens') });
    },
  });
}

export function useGatewayDenials(
  filters: { from?: string; to?: string; instance_id?: string } = {},
) {
  return useQuery({
    queryKey: queryKeys.gateway.denials(),
    queryFn: async () =>
      (await api.get('/api/v1/gateway/denials', {
        query: filters,
      })) as unknown as ListPage<GatewayDenial>,
  });
}

/** The instance picker for a token's scope editor, and the auth-mode panel's row set. */
export function useInstancesForTokens(): UseQueryResult<ListPage<Instance>> {
  const live = useLiveQueryOptions();
  return useQuery({
    queryKey: queryKeys.instances.list(),
    queryFn: async () => (await api.get('/api/v1/instances')) as unknown as ListPage<Instance>,
    ...live,
  });
}

/** Flips one instance's `auth_mode` — the only field this screen ever changes on an instance. */
export function useSetInstanceAuthMode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      generation,
      authMode,
    }: {
      id: string;
      generation: number;
      authMode: 'token' | 'none';
    }) =>
      api.patch('/api/v1/instances/{id}', {
        path: { id },
        body: { generation, auth_mode: authMode },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('instances') });
    },
  });
}
