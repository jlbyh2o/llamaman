import { useMemo, useState } from 'react';
import type { KeyboardEvent, ReactNode } from 'react';
import { ArrowDown, ArrowUp, ChevronsUpDown } from 'lucide-react';
import { cn } from './cn';
import { EmptyState } from './EmptyState';

/**
 * The table.
 *
 * Every list screen in DESIGN section 4 is one — instances, models, downloads, versions, tokens,
 * bench runs, events — so sorting, the sticky header, the keyboard path and the empty state are
 * decided once.
 *
 * **Sorting is controlled when the screen owns the URL.** Filters, sort and comparison selections
 * live in the router's search params (DESIGN section 4), so a screen passes `sort` and
 * `onSortChange` and does the sorting itself — server-side for a paginated list, or with
 * `sortRows()` for a small one. Omit both and the table sorts itself locally, which is right for a
 * table that is already fully loaded and whose order nobody needs to link to.
 */

export interface Column<T> {
  id: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  /** Local sorting needs a comparable; a column without one is not sortable locally. */
  sortValue?: (row: T) => string | number | boolean | null | undefined;
  sortable?: boolean;
  align?: 'left' | 'right' | 'center';
  /** A CSS width — `'12rem'`, `'1%'` for a shrink-to-fit action column. */
  width?: string;
  /** Numbers and identifiers get JetBrains Mono and tabular figures. */
  mono?: boolean;
  /** Hidden below `md`, for columns that are context rather than content. */
  secondary?: boolean;
  headerClassName?: string;
  cellClassName?: string;
}

export interface SortState {
  id: string;
  desc: boolean;
}

export interface DataTableProps<T> {
  columns: readonly Column<T>[];
  rows: readonly T[];
  rowKey: (row: T) => string;
  sort?: SortState | null;
  onSortChange?: (sort: SortState | null) => void;
  /** Row activation: click, Enter or Space. Adds the affordances that make that discoverable. */
  onRowClick?: (row: T) => void;
  /** Marks the row that the current route is showing. */
  isRowActive?: (row: T) => boolean;
  empty?: ReactNode;
  loading?: boolean;
  caption?: string;
  className?: string;
}

const ALIGN = { left: 'text-left', right: 'text-right', center: 'text-center' } as const;

/** Stable, null-last comparison used by the local sort. Exported so screens can reuse it. */
export function compareValues(
  a: string | number | boolean | null | undefined,
  b: string | number | boolean | null | undefined,
): number {
  const aEmpty = a === null || a === undefined;
  const bEmpty = b === null || b === undefined;
  if (aEmpty && bEmpty) return 0;
  if (aEmpty) return 1;
  if (bEmpty) return -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'boolean' && typeof b === 'boolean') return Number(a) - Number(b);
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
}

/** Sort rows by a column, for screens that own the sort state but not a server-side order. */
export function sortRows<T>(
  rows: readonly T[],
  columns: readonly Column<T>[],
  sort: SortState | null,
): readonly T[] {
  if (!sort) return rows;
  const column = columns.find((c) => c.id === sort.id);
  if (!column?.sortValue) return rows;
  const get = column.sortValue;
  return [...rows].sort((a, b) => (sort.desc ? -1 : 1) * compareValues(get(a), get(b)));
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  sort,
  onSortChange,
  onRowClick,
  isRowActive,
  empty,
  loading = false,
  caption,
  className,
}: DataTableProps<T>) {
  const controlled = onSortChange !== undefined;
  const [localSort, setLocalSort] = useState<SortState | null>(null);
  const activeSort = controlled ? (sort ?? null) : localSort;

  const visibleRows = useMemo(
    () => (controlled ? rows : sortRows(rows, columns, localSort)),
    [controlled, rows, columns, localSort],
  );

  const toggle = (column: Column<T>) => {
    const sortable = column.sortable ?? column.sortValue !== undefined;
    if (!sortable) return;
    const next: SortState | null =
      activeSort?.id !== column.id
        ? { id: column.id, desc: false }
        : activeSort.desc
          ? null
          : { id: column.id, desc: true };
    if (controlled) onSortChange(next);
    else setLocalSort(next);
  };

  const activate = (row: T) => onRowClick?.(row);
  const onRowKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, row: T) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    activate(row);
  };

  if (!loading && visibleRows.length === 0) {
    return <>{empty ?? <EmptyState title="Nothing here yet" />}</>;
  }

  return (
    <div className={cn('w-full overflow-x-auto', className)}>
      <table className="w-full border-collapse text-sm">
        {caption ? <caption className="sr-only">{caption}</caption> : null}
        <thead>
          <tr className="border-b border-[var(--lm-border)]">
            {columns.map((column) => {
              const sortable = column.sortable ?? column.sortValue !== undefined;
              const isSorted = activeSort?.id === column.id;
              const Icon = !isSorted ? ChevronsUpDown : activeSort.desc ? ArrowDown : ArrowUp;
              return (
                <th
                  key={column.id}
                  scope="col"
                  style={column.width ? { width: column.width } : undefined}
                  aria-sort={isSorted ? (activeSort.desc ? 'descending' : 'ascending') : 'none'}
                  className={cn(
                    'bg-[var(--lm-surface)] px-3 py-2 text-[11px] font-medium tracking-wide',
                    'text-[var(--lm-text-faint)] uppercase',
                    ALIGN[column.align ?? 'left'],
                    column.secondary && 'hidden md:table-cell',
                    column.headerClassName,
                  )}
                >
                  {sortable ? (
                    <button
                      type="button"
                      onClick={() => toggle(column)}
                      className={cn(
                        'inline-flex items-center gap-1 rounded-[var(--lm-radius-sm)]',
                        'hover:text-[var(--lm-text)]',
                        isSorted && 'text-[var(--lm-text)]',
                      )}
                    >
                      {column.header}
                      <Icon aria-hidden className="size-3" />
                    </button>
                  ) : (
                    column.header
                  )}
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {loading
            ? Array.from({ length: 3 }, (_, i) => (
                <tr key={`skeleton-${i}`} className="border-b border-[var(--lm-border)]">
                  {columns.map((column) => (
                    <td key={column.id} className="px-3 py-2.5">
                      <span className="block h-3 w-2/3 animate-pulse rounded bg-[var(--lm-neutral-soft)]" />
                    </td>
                  ))}
                </tr>
              ))
            : visibleRows.map((row) => {
                const active = isRowActive?.(row) ?? false;
                return (
                  <tr
                    key={rowKey(row)}
                    {...(onRowClick
                      ? {
                          tabIndex: 0,
                          onClick: () => activate(row),
                          onKeyDown: (event: KeyboardEvent<HTMLTableRowElement>) =>
                            onRowKeyDown(event, row),
                        }
                      : {})}
                    aria-current={active ? 'true' : undefined}
                    className={cn(
                      'border-b border-[var(--lm-border)] last:border-b-0',
                      onRowClick && 'cursor-pointer hover:bg-[var(--lm-neutral-soft)]',
                      active && 'bg-[var(--lm-accent-soft)]',
                    )}
                  >
                    {columns.map((column) => (
                      <td
                        key={column.id}
                        className={cn(
                          'px-3 py-2.5 align-middle text-[var(--lm-text)]',
                          ALIGN[column.align ?? 'left'],
                          column.mono && 'lm-numeric text-[13px]',
                          column.secondary && 'hidden md:table-cell',
                          column.cellClassName,
                        )}
                      >
                        {column.cell(row)}
                      </td>
                    ))}
                  </tr>
                );
              })}
        </tbody>
      </table>
    </div>
  );
}
