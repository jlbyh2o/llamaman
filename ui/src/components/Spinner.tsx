import { Loader2 } from 'lucide-react';
import type { ReactNode } from 'react';
import { cn } from './cn';

/**
 * Indeterminate waiting.
 *
 * Deliberately small in scope: DESIGN section 4 says anything job-backed renders the job's progress
 * rather than a spinner, so this is for the short gaps only — a query in flight, a route being
 * resolved — and `<Skeleton>` is the better answer wherever the shape of what is coming is known.
 */

export function Spinner({ className, label }: { className?: string; label?: string }) {
  return (
    <span role="status" aria-live="polite" className="inline-flex items-center gap-2">
      <Loader2
        aria-hidden
        className={cn('size-4 animate-spin text-[var(--lm-text-faint)]', className)}
      />
      <span className="sr-only">{label ?? 'Loading'}</span>
    </span>
  );
}

/** A grey block the size of the thing that is coming. */
export function Skeleton({ className }: { className?: string }) {
  return (
    <span
      aria-hidden
      className={cn('block h-3 animate-pulse rounded bg-[var(--lm-neutral-soft)]', className)}
    />
  );
}

/** A full-panel loading state, for a route whose data has not arrived. */
export function LoadingPanel({ children }: { children?: ReactNode }) {
  return (
    <div className="flex items-center justify-center gap-2 py-16 text-sm text-[var(--lm-text-muted)]">
      <Spinner />
      {children ?? 'Loading…'}
    </div>
  );
}
