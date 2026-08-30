/**
 * Session bootstrap, login, logout.
 *
 * The whole auth model is one public endpoint and one cookie pair. `GET /api/v1/auth/session`
 * answers `{authenticated, setup_complete, expires_at}` without a session of its own (DESIGN
 * section 3.1), which is what makes the shell's first decision — wizard, login, or app — a single
 * query rather than a chain of redirects.
 *
 * The setup gate is section 3's: until `admin_account` exists every session endpoint answers
 * `409 setup_required`, "and the SPA routes to the wizard on that code alone, so there is no
 * separate 'is it configured' flag to keep in sync". `setup_complete` here is the same fact read
 * ahead of time, so the first paint is already right.
 */

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';
import { api } from '../api/client';
import { ApiError } from '../api/errors';
import { queryKeys } from '../api/keys';
import type { SessionState, SetupState } from '../api/types';

export function useSession(): UseQueryResult<SessionState, Error> {
  return useQuery({
    queryKey: queryKeys.auth.session(),
    queryFn: () => api.get('/api/v1/auth/session'),
    // The one query that must not be served stale: it decides which shell renders.
    staleTime: 0,
    retry: false,
  });
}

export function useSetupState(enabled = true): UseQueryResult<SetupState, Error> {
  return useQuery({
    queryKey: queryKeys.setup.state(),
    queryFn: () => api.get('/api/v1/setup/state'),
    enabled,
    retry: false,
  });
}

export interface LockoutInfo {
  retryAfterMs: number;
}

/**
 * `POST /api/v1/auth/login`. `401 bad_credentials` and `429 locked_out` both arrive as `ApiError`,
 * and the login screen reads `code` to tell "wrong password" from "stop trying for a while".
 */
export function useLogin() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (password: string) => api.post('/api/v1/auth/login', { body: { password } }),
    onSuccess: async () => {
      // A fresh session changes what every query is allowed to see.
      await queryClient.invalidateQueries();
    },
  });
}

export function useLogout() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => api.post('/api/v1/auth/logout'),
    onSettled: () => {
      queryClient.clear();
    },
  });
}

/** Lockout detail from a failed login, or null when the failure was something else. */
export function lockoutFrom(error: unknown): LockoutInfo | null {
  if (!(error instanceof ApiError) || error.code !== 'locked_out') return null;
  return { retryAfterMs: error.retryAfterMs ?? 60_000 };
}
