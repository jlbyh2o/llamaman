import { Link } from '@tanstack/react-router';
import { AlertTriangle } from 'lucide-react';
import { useCapabilities } from '../features/system/queries';

/**
 * The one line every screen shows when this host cannot do something it normally can.
 *
 * DESIGN section 11.1a enumerates the degraded modes this daemon SERVES in — F9, a refused polkit
 * grant; F10, no reachable service manager; F23, an identity that cannot read the journal — and
 * `GET /system/capabilities` exists, in section 3.3's own words, "so the UI never has to guess". The
 * System screen renders the full matrix with a card per mode; this is the sentence the other
 * sixteen screens need, because a user who opens the Dashboard on an F10 host and sees instance
 * cards with working-looking controls has been told the host is fine.
 *
 * The daemon computes `degraded` rather than this component deriving it, and that is deliberate:
 * five capability booleans have thirty-two combinations, section 11.1a names the handful that mean
 * something, and a banner that re-derived them would be a second implementation of a table that
 * lives in one document.
 *
 * It renders nothing on a healthy host, nothing while capabilities are loading, and nothing when the
 * read failed — a banner that appeared because a request timed out would cry wolf, and the screens
 * that actually depend on a capability show their own error where the control would have been.
 */
export function DegradedBanner() {
  const capabilities = useCapabilities();
  const modes = capabilities.data?.degraded ?? [];
  if (modes.length === 0) return null;

  // The first is the most severe: the daemon emits them worst-first, and F10 short-circuits the
  // rest because everything downstream of "no service manager" is a consequence rather than a
  // separate thing to fix.
  const [first, ...rest] = modes;
  if (!first) return null;

  return (
    <div
      role="status"
      className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)] px-4 py-2"
    >
      <AlertTriangle aria-hidden className="size-4 shrink-0 text-[var(--lm-warn)]" />
      <span className="min-w-0 flex-1 text-xs text-[var(--lm-text)]">
        <span className="lm-numeric font-medium">{first.id}</span> — {first.summary}
        {rest.length > 0 ? (
          <span className="text-[var(--lm-text-muted)]">
            {' '}
            ({rest.length} more {rest.length === 1 ? 'limitation' : 'limitations'} on this host.)
          </span>
        ) : null}
      </span>
      <Link
        to="/system"
        className="shrink-0 text-xs font-medium text-[var(--lm-accent)] underline-offset-4 hover:underline"
      >
        What this means
      </Link>
    </div>
  );
}
