/**
 * The Hugging Face side of the models area — DESIGN section 3.6.
 *
 * Everything here reaches the Hub through the daemon, never from the browser. That is not a
 * convenience: the token lives sealed in `secrets` and must never reach a page (section 3.4), the
 * model card arrives *already rendered and sanitized* so that no client-side markdown renderer
 * exists to attack (D35), and the quant tree arrives with true `lfs.size` totals the Hub's file
 * listing does not hand out by default (section 3.6).
 *
 * The one behavior worth stating: a gated repository answers `403 hf_gated` with `{repo,
 * request_url}`, and the right response is to link out, because access grants are browser-only on
 * the Hub's side. So these queries do not retry a 403 — the daemon's answer is final, and the
 * screen renders the link instead of a spinner that will never resolve.
 *
 * **On the repo id in the path.** Section 3.6 registers these routes as `/hf/tree/{repo...}`, and
 * the OpenAPI renderer flattens a multi-segment wildcard to a plain `{repo}` parameter whose value
 * may contain a slash — so the generated path key here is `{repo}` and the client percent-encodes
 * the separator. That is correct rather than merely tolerated: Go's `ServeMux` unescapes a `...`
 * wildcard's value, so `bartowski%2FQwen3-8B-GGUF` and `bartowski/Qwen3-8B-GGUF` both reach the
 * handler as the same `PathValue("repo")`. The daemon serves this SPA itself, so there is no proxy
 * in between with an opinion about `%2F`.
 */

import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type {
  HFCard,
  HFModel,
  HFSearchResult,
  HFTree,
  ListPage,
  TokenStatus,
} from '../../api/types';
import { asPage, compact } from './api';
import type { HF_SORTS } from './api';

export interface HFSearchFilters {
  q?: string | undefined;
  author?: string | undefined;
  sort?: (typeof HF_SORTS)[number] | undefined;
}

const PAGE_SIZE = 30;

/**
 * `GET /api/v1/hf/search` — one page of GGUF repositories.
 *
 * The GGUF filter is the daemon's, not a query param: section 3.6 describes the endpoint as
 * "search the Hub's GGUF repositories", so there is no way to ask for anything else and no
 * checkbox that would pretend otherwise. `?cursor=` is the Hub's own opaque cursor, passed through
 * unmodified, which is why the page param is a string rather than an offset.
 */
export function useHFSearch(filters: HFSearchFilters) {
  return useInfiniteQuery({
    queryKey: queryKeys.hf.search(compact({ ...filters, limit: PAGE_SIZE })),
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      asPage<HFSearchResult>(
        await api.get('/api/v1/hf/search', {
          query: compact({ ...filters, cursor: pageParam, limit: PAGE_SIZE }),
        }),
      ),
    getNextPageParam: (last: ListPage<HFSearchResult>) => last.next_cursor ?? undefined,
    retry: (count, error) => !(error instanceof ApiError) && count < 2,
    // The Hub is a remote with rate limits; a search result is worth keeping for a few minutes.
    staleTime: 2 * 60_000,
  });
}

/** `GET /api/v1/hf/model/{repo}` — metadata, the `gguf` summary, and local availability. */
export function useHFModel(repo: string): UseQueryResult<HFModel, Error> {
  return useQuery({
    queryKey: queryKeys.hf.model(repo),
    queryFn: () => api.get('/api/v1/hf/model/{repo}', { path: { repo } }),
    enabled: repo !== '',
    retry: (count, error) => !(error instanceof ApiError) && count < 2,
    staleTime: 2 * 60_000,
  });
}

/**
 * `GET /api/v1/hf/tree/{repo}` — the file tree grouped by quantization.
 *
 * Each group is one *downloadable unit*: a single file or a whole shard set, with `total_bytes`
 * summed from true LFS sizes and `complete` reporting whether the repository actually holds every
 * shard its names promise (section 7.3). `mmproj` is the projector candidates, separate because a
 * projector becomes its own `models` row.
 */
export function useHFTree(repo: string, revision?: string): UseQueryResult<HFTree, Error> {
  return useQuery({
    queryKey: queryKeys.hf.tree(revision ? `${repo}@${revision}` : repo),
    queryFn: () =>
      api.get('/api/v1/hf/tree/{repo}', {
        path: { repo },
        query: compact({ revision }),
      }),
    enabled: repo !== '',
    retry: (count, error) => !(error instanceof ApiError) && count < 2,
    staleTime: 2 * 60_000,
  });
}

/** `GET /api/v1/hf/card/{repo}` — sanitized HTML plus the raw markdown behind "view source". */
export function useHFCard(repo: string, revision?: string): UseQueryResult<HFCard, Error> {
  return useQuery({
    queryKey: queryKeys.hf.card(revision ? `${repo}@${revision}` : repo),
    queryFn: () =>
      api.get('/api/v1/hf/card/{repo}', {
        path: { repo },
        query: compact({ revision }),
      }),
    enabled: repo !== '',
    retry: (count, error) => !(error instanceof ApiError) && count < 2,
    staleTime: 10 * 60_000,
  });
}

/**
 * `GET /api/v1/hf/token` — presence, hint and validity. Never the token.
 *
 * This is what tells the browse screen whether it is anonymous. It matters beyond a badge: without
 * a token, gated and private repositories are simply not reachable, and a search that quietly
 * returns fewer results than the Hub's own website is worse than one that says why.
 */
export function useHFTokenStatus(): UseQueryResult<TokenStatus, Error> {
  return useQuery({
    queryKey: queryKeys.hf.token(),
    queryFn: () => api.get('/api/v1/hf/token'),
    staleTime: 60_000,
  });
}

/** The `{repo, request_url}` a `403 hf_gated` carries, or null when this was some other failure. */
export function gatedFrom(error: unknown): { repo: string; requestUrl: string } | null {
  if (!(error instanceof ApiError)) return null;
  if (error.code !== 'hf_gated' && error.code !== 'hf_private') return null;
  const repo = error.details['repo'];
  const url = error.details['request_url'];
  return {
    repo: typeof repo === 'string' ? repo : '',
    requestUrl: typeof url === 'string' ? url : '',
  };
}

/** The repository's page on the Hub. The one link a gated repo can actually be resolved through. */
export function hubUrl(repo: string): string {
  return `https://huggingface.co/${repo}`;
}
