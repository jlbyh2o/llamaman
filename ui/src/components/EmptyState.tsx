import type { ReactNode } from 'react';
import { cn } from './cn';

/**
 * The empty state.
 *
 * An empty list in this app is usually a *stage*, not a failure — no models yet, no benchmarks yet,
 * no instances yet — so the shape is a sentence about where you are and the one button that moves
 * you on. `tone="error"` covers the other case: a list that is empty because something is wrong.
 */

export interface EmptyStateProps {
  title: ReactNode;
  description?: ReactNode;
  icon?: ReactNode;
  /** The single next step. A second one belongs in `secondaryAction`, not beside it. */
  action?: ReactNode;
  secondaryAction?: ReactNode;
  tone?: 'neutral' | 'error';
  className?: string;
  /** Compact form for an empty panel inside a busy screen. */
  dense?: boolean;
}

export function EmptyState({
  title,
  description,
  icon,
  action,
  secondaryAction,
  tone = 'neutral',
  dense = false,
  className,
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center rounded-[var(--lm-radius-lg)] border border-dashed text-center',
        dense ? 'gap-2 px-4 py-8' : 'gap-3 px-6 py-14',
        tone === 'error'
          ? 'border-[var(--lm-danger)]/40 bg-[var(--lm-danger-soft)]'
          : 'border-[var(--lm-border)] bg-[var(--lm-surface)]',
        className,
      )}
    >
      {icon ? (
        <span
          aria-hidden
          className={cn(
            'flex size-9 items-center justify-center rounded-[var(--lm-radius-lg)] [&>svg]:size-5',
            tone === 'error'
              ? 'bg-[var(--lm-danger-soft)] text-[var(--lm-danger)]'
              : 'bg-[var(--lm-neutral-soft)] text-[var(--lm-text-faint)]',
          )}
        >
          {icon}
        </span>
      ) : null}
      <p className="text-sm font-medium text-[var(--lm-text)]">{title}</p>
      {description ? (
        <p className="max-w-md text-xs text-[var(--lm-text-muted)]">{description}</p>
      ) : null}
      {action || secondaryAction ? (
        <div className="mt-1 flex items-center gap-2">
          {action}
          {secondaryAction}
        </div>
      ) : null}
    </div>
  );
}
