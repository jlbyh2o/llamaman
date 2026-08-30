import { Link } from '@tanstack/react-router';
import { Compass } from 'lucide-react';
import { EmptyState } from '../components';

/**
 * An unknown path inside the SPA.
 *
 * Distinct from the server's behavior, which is deliberately asymmetric: unknown paths that accept
 * HTML fall back to index.html so a deep link works after a refresh, and unknown `/api/*` paths
 * return a JSON 404 (DESIGN section 4). This is what the SPA then shows for a route it has never
 * heard of.
 */
export function NotFound() {
  return (
    <div className="p-8">
      <EmptyState
        icon={<Compass />}
        title="No such page"
        description="That path is not one of this app's screens. The link may be from an older version."
        action={
          <Link
            to="/"
            className="text-sm font-medium text-[var(--lm-accent)] underline-offset-4 hover:underline"
          >
            Go to the dashboard
          </Link>
        }
      />
    </div>
  );
}
