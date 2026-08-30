/**
 * "What went wrong, and the one thing to do about it."
 *
 * Section 17's promise, rendered: every failure this design anticipates has a card, and the card
 * carries the action rather than describing it. When the daemon cannot act — F9's withheld
 * `manage-unit-files` grant, F10's absent systemd — the hints the `409` carried are printed as
 * commands instead, because a button that cannot work is worse than a line you can paste.
 */

import { AlertTriangle, Terminal } from 'lucide-react';
import { Button, Panel } from '../../../components';
import { cn } from '../../../components/cn';
import type { Remediation, RemediationAction } from '../remediation';

export interface RemediationCardProps {
  remediation: Remediation;
  onAction?: (action: RemediationAction) => void;
  /** `sudo systemctl …` lines a degraded-mode response handed back. */
  hints?: readonly string[];
}

export function RemediationCard({ remediation, onAction, hints = [] }: RemediationCardProps) {
  const action = remediation.action;
  return (
    <Panel
      className={cn(
        remediation.tone === 'danger'
          ? 'border-[var(--lm-danger)]/40 bg-[var(--lm-danger-soft)]'
          : 'border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)]',
      )}
    >
      <div className="flex items-start gap-3">
        <AlertTriangle
          aria-hidden
          className={cn(
            'mt-0.5 size-4 shrink-0',
            remediation.tone === 'danger' ? 'text-[var(--lm-danger)]' : 'text-[var(--lm-warn)]',
          )}
        />
        <div className="min-w-0 space-y-2">
          <p className="text-sm font-medium text-[var(--lm-text)]">{remediation.title}</p>
          <p className="text-xs text-[var(--lm-text-muted)]">{remediation.detail}</p>

          {hints.length > 0 ? (
            <ul className="space-y-1">
              {hints.map((hint) => (
                <li
                  key={hint}
                  className="lm-numeric flex items-center gap-1.5 rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-sunken)] px-2 py-1 text-[12px] text-[var(--lm-text-muted)]"
                >
                  <Terminal aria-hidden className="size-3 shrink-0" />
                  {hint}
                </li>
              ))}
            </ul>
          ) : null}

          {action && onAction ? (
            <Button size="sm" onClick={() => onAction(action.kind)}>
              {action.label}
            </Button>
          ) : null}
        </div>
      </div>
    </Panel>
  );
}
