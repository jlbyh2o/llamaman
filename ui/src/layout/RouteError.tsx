import { useRouter } from '@tanstack/react-router';
import { AlertTriangle } from 'lucide-react';
import type { ErrorComponentProps } from '@tanstack/react-router';
import { Button, EmptyState, describeError } from '../components';

/**
 * The router's last line of defense.
 *
 * Every screen handles its own query errors — that is what `QueryError` is for, and it is the
 * better answer because it keeps the rest of the screen usable. This is for what those cannot
 * catch: a render that threw, a bad patch merged from a frame, a bug. Without it a thrown render
 * unmounts the route into whatever the router does by default, which is a blank region inside an
 * otherwise-working shell — and a blank region is the one failure a user cannot report usefully.
 *
 * `invalidate()` rather than a reload: the router re-runs the route and the queries re-read, so a
 * transient failure clears without losing the SSE stream the shell is holding open.
 */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter();
  const { title, description } = describeError(error);

  return (
    <div className="p-6">
      <EmptyState
        tone="error"
        icon={<AlertTriangle />}
        title="This screen could not be rendered"
        description={
          <>
            {title}
            {description && description !== title ? (
              <span className="lm-numeric block text-[11px] text-[var(--lm-text-faint)]">
                {description}
              </span>
            ) : null}
          </>
        }
        action={
          <Button
            variant="primary"
            onClick={() => {
              reset();
              void router.invalidate();
            }}
          >
            Try again
          </Button>
        }
        secondaryAction={
          <Button variant="ghost" onClick={() => window.location.reload()}>
            Reload the app
          </Button>
        }
      />
    </div>
  );
}
