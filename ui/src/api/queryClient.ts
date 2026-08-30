/**
 * The shared QueryClient.
 *
 * Defaults are tuned for a single-user admin app whose cache is kept fresh by SSE rather than by
 * polling (DESIGN section 4): a long `staleTime`, no refetch on window focus, and no retry on the
 * errors that retrying cannot fix — a `401`, a `409 setup_required`, or any 4xx that is the
 * daemon's considered answer.
 */

import { QueryClient } from '@tanstack/react-query';
import { ApiError } from './errors';

/** 4xx is an answer, not a hiccup; 5xx and transport failures are worth one more try. */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status < 500) return false;
  return failureCount < 2;
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // SSE patches the cache, so a focus-driven refetch would be noise on top of live data.
        refetchOnWindowFocus: false,
        refetchOnReconnect: true,
        staleTime: 30_000,
        gcTime: 5 * 60_000,
        retry: shouldRetry,
      },
      mutations: {
        retry: false,
      },
    },
  });
}
