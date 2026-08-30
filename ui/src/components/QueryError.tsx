import { AlertTriangle } from 'lucide-react';
import type { ReactNode } from 'react';
import { Button } from './Button';
import { EmptyState } from './EmptyState';
import { describeError } from './Toast';

/**
 * The state between "loading" and "empty".
 *
 * Most data screens in this app had two states and needed three. A query that FAILED fell through
 * to the empty branch, so a `500` on `GET /system/gpus` rendered "No GPUs detected — instances will
 * run CPU-only until a supported device is found" to a user with two working cards, a dropped
 * connection on `GET /bench/runs` rendered "No benchmarks yet" over a real history, and a failed
 * `GET /llamacpp/active` rendered "No active build" on a host with one installed. Each of those is
 * a sentence the app cannot know to be true, stated as if it were — which is worse than an error,
 * because the user acts on it.
 *
 * So: `isPending` → a loading panel, `isError` → this, otherwise the empty copy is safe to trust.
 *
 * The message comes from the daemon. `describeError` unwraps an `ApiError` into its closed `code`
 * and the message the error envelope carried (DESIGN section 3), which is the difference between
 * "something went wrong" and "this daemon's identity cannot read the journal". `onRetry` is
 * `query.refetch`; omit it only where a retry cannot help.
 */

export interface QueryErrorProps {
  /** What could not be read, in the user's words: "The GPU inventory". */
  title: ReactNode;
  /** The thrown value. Its message and code are rendered beneath the title. */
  error: unknown;
  onRetry?: () => void;
  /** Compact form, for a panel inside a busy screen. */
  dense?: boolean;
  className?: string;
}

export function QueryError({ title, error, onRetry, dense = false, className }: QueryErrorProps) {
  const { title: message, description } = describeError(error);
  return (
    <EmptyState
      tone="error"
      icon={<AlertTriangle />}
      title={title}
      description={
        <>
          {message}
          {description && description !== message ? (
            <span className="lm-numeric block text-[11px] text-[var(--lm-text-faint)]">
              {description}
            </span>
          ) : null}
        </>
      }
      dense={dense}
      {...(className ? { className } : {})}
      {...(onRetry
        ? {
            action: (
              <Button variant="primary" size="sm" onClick={onRetry}>
                Try again
              </Button>
            ),
          }
        : {})}
    />
  );
}
