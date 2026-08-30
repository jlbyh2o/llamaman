/**
 * Browse Hugging Face — DESIGN section 4, screen 8.
 *
 * "HF search with the GGUF filter, gated badge, sort, infinite list."
 *
 * Everything that shapes the list is in the URL (`q`, `author`, `sort`, `gated`), which is what
 * makes a search a thing you can send someone. Nothing here talks to the Hub: the daemon owns the
 * token, the GGUF filter and the rate limit, and this screen renders what it returns.
 *
 * The anonymous state is treated as information rather than as a nag. Without a stored token the
 * Hub still answers, but gated and private repositories are unreachable — so the banner says
 * exactly that, once, above the results, and links to the one place the token can be added.
 */

import { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { Ban, KeyRound, Lock, Search, ThumbsUp } from 'lucide-react';

import { Badge, Button, EmptyState, Input, Panel, Select, Spinner } from '../../components';
import { cn } from '../../components/cn';
import type { HFSearchResult } from '../../api/types';
import { formatCount, formatRelative } from '../../format';
import { hfSort, HF_SORTS } from '../../features/models/api';
import { ExternalLinkButton } from '../../features/models/LinkButton';
import { gatedFrom, hubUrl, useHFSearch, useHFTokenStatus } from '../../features/models/hf';

const SORT_LABELS: Record<(typeof HF_SORTS)[number], string> = {
  downloads: 'Most downloaded',
  likes: 'Most liked',
  lastModified: 'Recently updated',
  trendingScore: 'Trending',
};

export function BrowseScreen() {
  // The route *id* — the app shell is a pathless layout route (`id: 'app'`), so every screen under
  // it is addressed as `/app/…` even though its URL has no such segment.
  const search = useSearch({ from: '/app/models/browse/' });
  const navigate = useNavigate({ from: '/models/browse' });
  const token = useHFTokenStatus();

  // The text field is local and the URL is debounced behind it: a search param that changed on
  // every keystroke would put thirty history entries between a user and their back button.
  const [text, setText] = useState(search.q ?? '');
  useEffect(() => setText(search.q ?? ''), [search.q]);
  useEffect(() => {
    const current = search.q ?? '';
    if (text === current) return;
    const timer = setTimeout(() => {
      void navigate({
        search: (prev) => ({ ...prev, q: text === '' ? undefined : text }),
        replace: true,
      });
    }, 300);
    return () => clearTimeout(timer);
  }, [text, search.q, navigate]);

  const sort = hfSort(search.sort);
  const results = useHFSearch({
    ...(search.q === undefined ? {} : { q: search.q }),
    ...(search.author === undefined ? {} : { author: search.author }),
    ...(sort === undefined ? {} : { sort }),
  });

  const items = useMemo(
    () => (results.data?.pages ?? []).flatMap((page) => page.items),
    [results.data],
  );

  // `gated` is an access filter, not a badge toggle: unset shows everything, `true` shows only what
  // needs a grant, `false` shows only what can be downloaded without one. Private counts as
  // restricted — reaching one needs a token that can see it, which is the same wall from here.
  const visible = useMemo(() => {
    if (search.gated === undefined) return items;
    return items.filter((item) =>
      search.gated ? item.gated || item.private : !item.gated && !item.private,
    );
  }, [items, search.gated]);

  const gated = gatedFrom(results.error);
  const anonymous = token.data !== undefined && !token.data.present;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">
            Browse Hugging Face
          </h1>
          <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">
            Repositories that publish GGUF files. Everything else on the Hub is filtered out by the
            daemon.
          </p>
        </div>
        <Link
          to="/downloads"
          className="text-sm text-[var(--lm-accent)] underline-offset-4 hover:underline"
        >
          Download queue
        </Link>
      </div>

      <Panel className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)]">
          <label className="block">
            <span className="sr-only">Search</span>
            <Input
              value={text}
              onChange={(event) => setText(event.target.value)}
              placeholder="qwen3 8b instruct…"
              autoFocus
              aria-label="Search Hugging Face"
            />
          </label>

          <label className="block">
            <span className="sr-only">Author</span>
            <Input
              value={search.author ?? ''}
              onChange={(event) =>
                void navigate({
                  search: (prev) => ({
                    ...prev,
                    author: event.target.value === '' ? undefined : event.target.value,
                  }),
                  replace: true,
                })
              }
              placeholder="author, e.g. bartowski"
              mono
              aria-label="Author"
            />
          </label>

          <Select
            value={sort ?? 'downloads'}
            onValueChange={(value) =>
              void navigate({
                search: (prev) => ({ ...prev, sort: value === 'downloads' ? undefined : value }),
              })
            }
            aria-label="Sort"
            options={HF_SORTS.map((value) => ({ value, label: SORT_LABELS[value] }))}
          />

          <Select
            value={search.gated === undefined ? 'any' : search.gated ? 'gated' : 'open'}
            onValueChange={(value) =>
              void navigate({
                search: (prev) => ({
                  ...prev,
                  gated: value === 'any' ? undefined : value === 'gated',
                }),
              })
            }
            aria-label="Access"
            options={[
              { value: 'any', label: 'Any access' },
              { value: 'open', label: 'Open only', description: 'Downloadable without a grant' },
              { value: 'gated', label: 'Gated only', description: 'Needs approval on the Hub' },
            ]}
          />
        </div>

        {anonymous ? (
          <p className="flex flex-wrap items-center gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] px-3 py-2 text-xs text-[var(--lm-text-muted)]">
            <KeyRound aria-hidden className="size-3.5 shrink-0" />
            Browsing anonymously. Gated and private repositories will not be reachable.
            <Link
              to="/settings"
              search={{ group: 'huggingface' }}
              className="text-[var(--lm-accent)] underline-offset-4 hover:underline"
            >
              Add a Hugging Face token
            </Link>
          </p>
        ) : token.data?.present ? (
          <p className="flex flex-wrap items-center gap-2 text-xs text-[var(--lm-text-muted)]">
            <KeyRound aria-hidden className="size-3.5 shrink-0 text-[var(--lm-ok)]" />
            Signed in to the Hub as{' '}
            <span className="lm-numeric text-[var(--lm-text)]">{token.data.user || 'unknown'}</span>
            {token.data.valid === false ? (
              <Badge tone="danger">token rejected on last use</Badge>
            ) : null}
          </p>
        ) : null}
      </Panel>

      {results.isPending ? (
        <div className="flex justify-center py-16">
          <Spinner label="Searching the Hub" />
        </div>
      ) : gated ? (
        <EmptyState
          tone="error"
          icon={<Lock />}
          title="That repository is gated"
          description="Access grants are browser-only on the Hub's side, so this has to be requested there."
          action={
            <ExternalLinkButton href={gated.requestUrl || hubUrl(gated.repo)} variant="primary">
              Request access on Hugging Face
            </ExternalLinkButton>
          }
        />
      ) : results.error ? (
        <EmptyState
          tone="error"
          icon={<Ban />}
          title="The Hub could not be searched"
          description={(results.error as Error).message}
          action={
            <Button onClick={() => void results.refetch()} loading={results.isFetching}>
              Try again
            </Button>
          }
        />
      ) : visible.length === 0 ? (
        <EmptyState
          icon={<Search />}
          title="Nothing matched"
          description={
            search.gated !== undefined
              ? 'No repository on this page matches that access filter. Try “Any access”.'
              : 'Try a shorter query, or drop the author filter.'
          }
        />
      ) : (
        <>
          <ul className="space-y-2">
            {visible.map((item) => (
              <li key={item.id}>
                <ResultRow result={item} />
              </li>
            ))}
          </ul>

          <div className="flex items-center justify-center gap-3 py-2">
            {results.hasNextPage ? (
              <Button
                onClick={() => void results.fetchNextPage()}
                loading={results.isFetchingNextPage}
              >
                Load more
              </Button>
            ) : (
              <p className="text-xs text-[var(--lm-text-faint)]">
                {formatCount(items.length)} results — the end of what the Hub returned.
              </p>
            )}
          </div>
        </>
      )}
    </div>
  );
}

function ResultRow({ result }: { result: HFSearchResult }) {
  const tags = result.tags.filter((tag) => !tag.startsWith('base_model:')).slice(0, 6);

  return (
    <Link
      to="/models/browse/$"
      params={{ _splat: result.id }}
      className={cn(
        'block rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)] p-3',
        'transition-colors duration-[var(--lm-duration-fast)] hover:border-[var(--lm-border-strong)]',
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="lm-numeric truncate text-[13px] font-medium text-[var(--lm-text)]">
              {result.id}
            </span>
            {result.gated ? (
              <Badge tone="warn" icon={<Lock />} title="Access must be requested on the Hub.">
                Gated
              </Badge>
            ) : null}
            {result.private ? <Badge tone="info">Private</Badge> : null}
          </div>

          {result.gguf ? (
            <p className="lm-numeric mt-1 text-xs text-[var(--lm-text-muted)]">
              {result.gguf.architecture} · {formatCount(result.gguf.context_length)} ctx ·{' '}
              {formatCount(result.gguf.total)} quantizations
            </p>
          ) : (
            <p className="mt-1 text-xs text-[var(--lm-text-faint)]">
              The Hub has not computed GGUF metadata for this repository yet.
            </p>
          )}
        </div>

        <div className="lm-numeric flex shrink-0 items-center gap-3 text-xs text-[var(--lm-text-muted)]">
          <span title="Downloads in the last month">↓ {formatCount(result.downloads)}</span>
          <span className="flex items-center gap-1" title="Likes">
            <ThumbsUp aria-hidden className="size-3" />
            {formatCount(result.likes)}
          </span>
          {result.updated_at ? (
            <span title={result.updated_at}>{formatRelative(result.updated_at)}</span>
          ) : null}
        </div>
      </div>

      {tags.length > 0 ? (
        <ul className="mt-2 flex flex-wrap gap-1">
          {tags.map((tag) => (
            <li
              key={tag}
              className="lm-numeric rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-sunken)] px-1.5 py-0.5 text-[11px] text-[var(--lm-text-faint)]"
            >
              {tag}
            </li>
          ))}
        </ul>
      ) : null}
    </Link>
  );
}
