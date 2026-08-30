/**
 * `/events` — the filterable audit log (DESIGN section 4, screen 17).
 *
 * Every state change the daemon made, newest first, with the filters section 3.14 gives
 * `GET /events/log`: category, subject, a `before` cursor and a free-text match. It is the screen a
 * user reaches when something happened and they need to know what, so two decisions shape it.
 *
 * **Every filter lives in the URL.** Section 4 chose TanStack Router for exactly this — "filters,
 * sort and comparison selections live in the URL, which is what a technical tool needs (shareable
 * links, working back button)" — so a support conversation can be a link rather than a list of
 * instructions. `eventsSearchSchema` in `searchParams.ts` is the schema; nothing here holds filter
 * state in React.
 *
 * **`level` and `q` are filtered client-side and say so.** `GET /events/log` takes `category`,
 * `subject_id`, `before` and `limit`; it does not take a level or a substring. Sending them would
 * be a filter that silently did nothing, so the page narrows what it has and the header states the
 * count it is narrowing from — an audit log that quietly hides rows is worse than one that shows
 * too many.
 *
 * **Paging is the `before` cursor, not an offset.** Ids are ULIDs and sort by creation (section 2),
 * so "older than this row" is a keyset the server can answer without counting. The cursor is in the
 * URL like everything else, which is what makes a deep page linkable and the back button correct.
 */

import { useMemo } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { ChevronLeft, ChevronRight, RotateCcw, ScrollText } from 'lucide-react';
import type { Column } from '../../components';
import {
  Badge,
  Button,
  DataTable,
  EmptyState,
  Input,
  LoadingPanel,
  Panel,
  PanelHeader,
  Select,
} from '../../components';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { AuditEvent } from '../../api/types';
import { useLiveQueryOptions } from '../../events/EventStreamProvider';
import { absoluteTimestamp, formatRelative, formatTimestamp } from '../../format';

/** The closed enum of `events.category` (section 2.11), in the order section 3.14 lists it. */
const CATEGORIES = [
  'llamacpp',
  'model',
  'download',
  'instance',
  'token',
  'bench',
  'auth',
  'update',
  'system',
  'gateway',
] as const;

/** The closed enum of `events.level`. */
const LEVELS = ['debug', 'info', 'warn', 'error'] as const;

const LEVEL_TONE: Record<string, 'neutral' | 'info' | 'warn' | 'danger'> = {
  debug: 'neutral',
  info: 'info',
  warn: 'warn',
  error: 'danger',
};

/** How many rows one page holds. */
const PAGE_SIZE = 100;

export function EventsScreen() {
  const search = useSearch({ from: '/app/events' });
  const navigate = useNavigate({ from: '/events' });
  const live = useLiveQueryOptions();

  const setFilter = (patch: Record<string, string | undefined>) => {
    void navigate({
      search: (prev) => {
        const next = { ...prev, ...patch };
        // A filter change invalidates the cursor: page three of the old filter is not page three
        // of the new one, and keeping it would show an empty page that looks like "no results".
        if (!('before' in patch)) delete next.before;
        return next;
      },
      replace: true,
    });
  };

  const query = useQuery({
    queryKey: queryKeys.events.log({
      category: search.category ?? '',
      subject_id: search.subject_id ?? '',
      before: search.before ?? '',
      limit: PAGE_SIZE,
    }),
    queryFn: () =>
      api.get('/api/v1/events/log', {
        query: {
          limit: String(PAGE_SIZE),
          ...(search.category ? { category: search.category as never } : {}),
          ...(search.subject_id ? { subject_id: search.subject_id } : {}),
          ...(search.before ? { before: search.before } : {}),
        },
      }),
    // The `events` topic is signal-only — a new row is not an update to an old one — so the
    // stream invalidates this query and it re-reads. Nothing polls while the stream is up.
    ...live,
  });

  const page: readonly AuditEvent[] = query.data?.items ?? [];

  // `level` and `q` are ours to apply; see this file's docstring for why they are not sent.
  const rows = useMemo(() => {
    const needle = search.q?.trim().toLowerCase();
    return page
      .filter((row) => !search.level || row.level === search.level)
      .filter((row) => {
        if (!needle) return true;
        return (
          row.message.toLowerCase().includes(needle) ||
          row.action.toLowerCase().includes(needle) ||
          (row.subject_id ?? '').toLowerCase().includes(needle)
        );
      });
  }, [page, search.level, search.q]);

  const narrowed = rows.length !== page.length;
  const oldest = page.length > 0 ? page[page.length - 1] : undefined;
  const canGoOlder = page.length === PAGE_SIZE && oldest !== undefined;

  const columns: Column<AuditEvent>[] = [
    {
      id: 'at',
      header: 'When',
      width: '11rem',
      mono: true,
      sortValue: (row) => row.at,
      cell: (row) => (
        <time dateTime={absoluteTimestamp(row.at)} title={formatTimestamp(row.at)}>
          {formatRelative(row.at)}
        </time>
      ),
    },
    {
      id: 'level',
      header: 'Level',
      width: '6rem',
      cell: (row) => (
        <Badge tone={LEVEL_TONE[row.level] ?? 'neutral'} dot>
          {row.level}
        </Badge>
      ),
    },
    {
      id: 'category',
      header: 'Category',
      width: '8rem',
      secondary: true,
      cell: (row) => (
        <button
          type="button"
          className="text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
          onClick={() => setFilter({ category: row.category })}
        >
          {row.category}
        </button>
      ),
    },
    {
      id: 'message',
      header: 'What happened',
      cell: (row) => (
        <div className="min-w-0">
          <div className="text-[var(--lm-text)]">{row.message || row.action}</div>
          <div className="text-xs text-[var(--lm-text-faint)]">
            {row.actor} · {row.action}
            {row.from_state && row.to_state ? ` · ${row.from_state} → ${row.to_state}` : ''}
          </div>
        </div>
      ),
    },
    {
      id: 'subject',
      header: 'Subject',
      width: '14rem',
      mono: true,
      secondary: true,
      cell: (row) =>
        row.subject_id ? (
          <button
            type="button"
            className="truncate text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
            title={`${row.subject_type ?? 'subject'} ${row.subject_id}`}
            onClick={() => setFilter({ subject_id: row.subject_id ?? undefined })}
          >
            {row.subject_type ? `${row.subject_type} ` : ''}
            {row.subject_id.slice(-8)}
          </button>
        ) : (
          <span className="text-xs text-[var(--lm-text-faint)]">—</span>
        ),
    },
  ];

  const filtered = Boolean(
    search.category || search.level || search.subject_id || search.q || search.before,
  );

  return (
    <div className="space-y-4 p-4 lg:p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Events</h1>
          <p className="mt-0.5 text-sm text-[var(--lm-text-muted)]">
            Every state change this daemon recorded, newest first.
          </p>
        </div>
        {filtered ? (
          <Button
            size="sm"
            variant="ghost"
            onClick={() =>
              void navigate({
                search: () => ({}),
                replace: true,
              })
            }
          >
            <RotateCcw /> Clear filters
          </Button>
        ) : null}
      </div>

      <Panel>
        <PanelHeader
          title="Filters"
          description="Every one of these is in the URL, so a filtered view is a link you can send."
        />
        <div className="mt-3 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Select
            aria-label="Category"
            value={search.category ?? ''}
            onValueChange={(value) => setFilter({ category: value || undefined })}
            options={[
              { value: '', label: 'Every category' },
              ...CATEGORIES.map((c) => ({ value: c, label: c })),
            ]}
          />
          <Select
            aria-label="Level"
            value={search.level ?? ''}
            onValueChange={(value) => setFilter({ level: value || undefined })}
            options={[
              { value: '', label: 'Every level' },
              ...LEVELS.map((l) => ({ value: l, label: l })),
            ]}
          />
          <Input
            aria-label="Subject id"
            placeholder="Subject id"
            value={search.subject_id ?? ''}
            onChange={(event) => setFilter({ subject_id: event.target.value || undefined })}
          />
          <Input
            aria-label="Search messages"
            placeholder="Search this page"
            value={search.q ?? ''}
            onChange={(event) => setFilter({ q: event.target.value || undefined })}
          />
        </div>
        {narrowed ? (
          <p className="mt-2 text-xs text-[var(--lm-text-faint)]">
            Showing {rows.length} of {page.length} rows on this page. Level and text narrow what has
            already been read; category and subject are asked of the daemon.
          </p>
        ) : null}
      </Panel>

      {query.isPending ? (
        <LoadingPanel>Reading the audit log…</LoadingPanel>
      ) : query.isError ? (
        <EmptyState
          tone="error"
          icon={<ScrollText />}
          title="The audit log could not be read"
          description={
            query.error instanceof Error
              ? query.error.message
              : 'The daemon did not answer for GET /events/log.'
          }
          action={
            <Button variant="primary" onClick={() => void query.refetch()}>
              Try again
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<ScrollText />}
          title={filtered ? 'Nothing matches those filters' : 'Nothing has happened yet'}
          description={
            filtered
              ? 'Every state change is recorded here as it happens. Widen the filters to see more.'
              : 'This daemon has not recorded a state change yet. Installing llama.cpp or creating an instance will fill this in.'
          }
          {...(filtered
            ? {
                action: (
                  <Button variant="ghost" onClick={() => void navigate({ search: () => ({}) })}>
                    Clear filters
                  </Button>
                ),
              }
            : {
                action: (
                  <Link to="/llamacpp">
                    <Button variant="primary">Go to llama.cpp</Button>
                  </Link>
                ),
              })}
        />
      ) : (
        <>
          <DataTable
            columns={columns}
            rows={rows}
            rowKey={(row) => row.id}
            caption="The events table"
          />
          <div className="flex items-center justify-between gap-3">
            <span className="text-xs text-[var(--lm-text-faint)]">
              {search.before ? 'An older page.' : 'The newest page.'}
            </span>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="ghost"
                disabled={!search.before}
                onClick={() => setFilter({ before: undefined })}
              >
                <ChevronLeft /> Newest
              </Button>
              <Button
                size="sm"
                variant="ghost"
                disabled={!canGoOlder}
                onClick={() =>
                  oldest && void navigate({ search: (prev) => ({ ...prev, before: oldest.id }) })
                }
              >
                Older <ChevronRight />
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
