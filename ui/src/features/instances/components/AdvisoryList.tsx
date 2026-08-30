/**
 * The form's second channel: things that are true and worth saying, but are not refusals.
 *
 * Kept visually distinct from field errors on purpose. An error is the daemon's answer rendered
 * early; an advisory is a judgment about a configuration the daemon will accept — `-ub` above `-b`,
 * a context that does not divide across the slots, metrics turned off so `requests_served` reads as
 * unavailable rather than zero. Mixing them would teach a user to ignore both.
 */

import { AlertTriangle, Info } from 'lucide-react';
import { cn } from '../../../components/cn';
import type { Advisory } from '../advisories';

export function AdvisoryList({
  advisories,
  className,
}: {
  advisories: readonly Advisory[];
  className?: string;
}) {
  if (advisories.length === 0) return null;
  return (
    <ul className={cn('space-y-1.5', className)} aria-label="Advisories">
      {advisories.map((advisory) => {
        const Icon = advisory.tone === 'warn' ? AlertTriangle : Info;
        return (
          <li
            key={advisory.code}
            data-advisory={advisory.code}
            className={cn(
              'flex items-start gap-2 rounded-[var(--lm-radius)] border px-2.5 py-2 text-xs',
              advisory.tone === 'warn'
                ? 'border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)] text-[var(--lm-text)]'
                : 'border-[var(--lm-border)] bg-[var(--lm-surface-raised)] text-[var(--lm-text-muted)]',
            )}
          >
            <Icon
              aria-hidden
              className={cn(
                'mt-0.5 size-3.5 shrink-0',
                advisory.tone === 'warn' ? 'text-[var(--lm-warn)]' : 'text-[var(--lm-text-faint)]',
              )}
            />
            <span>{advisory.message}</span>
          </li>
        );
      })}
    </ul>
  );
}
