import { AlertTriangle, Bell, Info, X } from 'lucide-react';
import {
  Badge,
  Button,
  LoadingPanel,
  Panel,
  PanelHeader,
  QueryError,
  toast,
} from '../../components';
import { CommandLine } from '../../features/system/CommandBlock';
import { formatRelative } from '../../format';
import { useDismissNotification, useNotifications } from '../../features/system/queries';
import type { SystemNotification } from '../../features/system/types';

/**
 * The notification feed of DESIGN section 4, screen 3.
 *
 * `notifications` is section 2.11's small table beside `events` — "things that need a human" — and
 * section 17 is a whole matrix of failures that reach a user through it: a denied polkit grant, a
 * failed canary, a unit left enabled for a deleted instance, a rebuild recommendation, an update
 * that reverted. Every one of those was being written and none of them had a surface: `useGpus`
 * aside, `useNotifications()` was consumed by exactly one component (a "rebuild recommended" badge
 * on the versions list) and `useDismissNotification()` was defined and called from NOWHERE, so
 * `POST /system/notifications/{id}/dismiss` was unreachable and a card, once raised, was permanent.
 *
 * The card carries its remediation commands verbatim, because that is what the daemon put in
 * `action_json` and what makes the difference between "polkit denied something" and a line the user
 * can paste. Dismissing stamps the row rather than deleting it (section 2.11 keeps dismissed cards
 * for 30 days), so clearing one is not destroying a record.
 */

/**
 * The `notifications.severity` CHECK, mapped to a tone and an icon.
 *
 * Typed as a total map over the enum rather than a lookup with a fallback, so adding a fourth
 * severity on the Go side is a compile error here instead of a card that silently renders as `info`.
 */
const SEVERITY: Record<
  SystemNotification['severity'],
  { tone: 'danger' | 'warn' | 'info'; Icon: typeof Info }
> = {
  error: { tone: 'danger', Icon: AlertTriangle },
  warn: { tone: 'warn', Icon: AlertTriangle },
  info: { tone: 'info', Icon: Info },
};

export function Notifications() {
  const notifications = useNotifications();
  const dismiss = useDismissNotification();

  const rows = notifications.data ?? [];

  // Nothing to say is the ordinary state of a healthy host, and a permanent "no notifications"
  // panel is one more thing to read past every day.
  if (!notifications.isPending && !notifications.isError && rows.length === 0) return null;

  return (
    <Panel>
      <PanelHeader
        title="Needs attention"
        description="Things the daemon could not resolve on its own."
      />

      {notifications.isPending ? (
        <LoadingPanel>Reading notifications…</LoadingPanel>
      ) : notifications.isError ? (
        <QueryError
          title="Notifications could not be read"
          error={notifications.error}
          onRetry={() => void notifications.refetch()}
          dense
          className="mt-3"
        />
      ) : (
        <ul className="mt-3 space-y-3">
          {rows.map((row) => {
            const { tone, Icon } = SEVERITY[row.severity];
            return (
              <li
                key={row.id}
                className="rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3"
              >
                <div className="flex items-start gap-2">
                  <Badge tone={tone} icon={<Icon />}>
                    {row.code}
                  </Badge>
                  <div className="min-w-0 flex-1">
                    <p className="text-xs font-medium text-[var(--lm-text)]">{row.title}</p>
                    <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">{row.message}</p>
                  </div>
                  <Button
                    size="icon"
                    variant="ghost"
                    aria-label={`Dismiss ${row.title}`}
                    disabled={dismiss.isPending}
                    onClick={() =>
                      dismiss.mutate(row.id, {
                        onError: (error) => toast.error(error),
                      })
                    }
                  >
                    <X />
                  </Button>
                </div>

                {row.hints.length > 0 ? (
                  <div className="mt-2 space-y-1.5">
                    {row.hints.map((hint) => (
                      <CommandLine key={hint} command={hint} />
                    ))}
                  </div>
                ) : null}

                <p className="mt-2 text-[11px] text-[var(--lm-text-faint)]">
                  {formatRelative(row.created_at)}
                  {row.subject ? ` · ${row.subject.type}` : ''}
                </p>
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}

/** The header badge: how many cards are outstanding, for the shell to show at a glance. */
export function NotificationCount() {
  const notifications = useNotifications();
  const count = notifications.data?.length ?? 0;
  if (count === 0) return null;
  return (
    <Badge tone="warn" icon={<Bell />}>
      {count}
    </Badge>
  );
}

/** Exported for the tests, which assert the map is exhaustive over the severity enum. */
export const NOTIFICATION_SEVERITIES = Object.keys(SEVERITY);
