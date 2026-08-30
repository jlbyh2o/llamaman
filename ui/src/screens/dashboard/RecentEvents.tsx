import { Link } from '@tanstack/react-router';
import { Badge, Panel, PanelHeader } from '../../components';
import type { AuditEvent } from '../../api/types';
import { absoluteTimestamp, formatRelative } from '../../format';

/**
 * The recent-events feed of DESIGN section 4, screen 3.
 *
 * It is the same rows the `/events` screen filters, read newest-first and capped: an audit log's
 * value on a landing page is answering "what happened while I was away" in one glance, and a
 * hundred rows answer it worse than twelve do. `events` is an SSE topic whose frames carry no patch
 * — a new row is not an update to an old one — so the list is invalidated and re-read by the
 * patcher rather than mutated in place.
 */
const LEVEL_TONE: Record<string, 'neutral' | 'info' | 'warn' | 'danger'> = {
  debug: 'neutral',
  info: 'info',
  warn: 'warn',
  error: 'danger',
};

export function RecentEvents({ events }: { events: readonly AuditEvent[] }) {
  return (
    <Panel>
      <PanelHeader
        title="Recent events"
        description="Every state change the daemon recorded, newest first."
        actions={
          <Link
            to="/events"
            className="text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
          >
            All events
          </Link>
        }
      />

      {events.length === 0 ? (
        <p className="mt-3 text-xs text-[var(--lm-text-faint)]">Nothing has happened yet.</p>
      ) : (
        <ol className="mt-3 space-y-2">
          {events.map((event) => (
            <li key={event.id} className="flex items-start gap-2">
              <Badge tone={LEVEL_TONE[event.level] ?? 'neutral'} dot className="mt-0.5 shrink-0">
                {event.category}
              </Badge>
              <span className="min-w-0 flex-1">
                <span className="block truncate text-xs text-[var(--lm-text)]">
                  {event.message}
                </span>
                <span className="block text-[11px] text-[var(--lm-text-faint)]">
                  {event.actor} · {event.action}
                  {event.from_state && event.to_state
                    ? ` · ${event.from_state} → ${event.to_state}`
                    : ''}
                </span>
              </span>
              <time
                dateTime={absoluteTimestamp(event.at)}
                title={absoluteTimestamp(event.at)}
                className="shrink-0 text-[11px] text-[var(--lm-text-faint)]"
              >
                {formatRelative(event.at)}
              </time>
            </li>
          ))}
        </ol>
      )}
    </Panel>
  );
}
