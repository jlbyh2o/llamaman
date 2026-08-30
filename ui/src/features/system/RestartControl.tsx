/**
 * The one restart button (DESIGN section 3.3 / section 3.4).
 *
 * `POST /system/restart` has exactly two rejection shapes, and the whole point of building this once
 * is that neither is treated as a generic failure:
 *
 *  - **`409`** — `job_in_flight` (a build or self-update is live, D4), `restart_unavailable` (no
 *    service manager to restart with — the flag stays advisory and the response names the manual
 *    command), or `systemd_denied` (the polkit grant was refused — both the manual command and the
 *    `install-units --repair-polkit` repair). "The 409 wins" over the rate limit below.
 *  - **`429 restart_rate_limited`** — the only 429 in this API that is not a login lockout (section
 *    3): this boot has not yet cleared its unit's start-limit counter (D93). It carries
 *    `retry_after_ms`, and the button disables for exactly that long rather than spending a start
 *    the revert deadline needs.
 *
 * `capabilities` is read when the caller has it, so a host with `systemd_control: 'unavailable'`
 * sees the explanation *before* clicking rather than after a doomed request (F10, section 11.1a).
 */

import { useEffect, useState } from 'react';
import { RotateCw } from 'lucide-react';
import { Button } from '../../components/Button';
import { toast } from '../../components/Toast';
import { isApiError } from '../../api/errors';
import { CommandLine } from './CommandBlock';
import { useRestartDaemon } from './queries';
import type { Capabilities } from './types';
import { formatSeconds } from '../../format';

export interface RestartControlProps {
  capabilities?: Capabilities | undefined;
  /** Compact icon-only trigger for a toolbar; the default is a labeled button. */
  size?: 'sm' | 'md';
  label?: string;
  className?: string;
}

export function RestartControl({
  capabilities,
  size = 'md',
  label = 'Restart daemon',
  className,
}: RestartControlProps) {
  const restart = useRestartDaemon();
  const [blockedUntil, setBlockedUntil] = useState<number | null>(null);
  const [remainingMs, setRemainingMs] = useState(0);
  const [hints, setHints] = useState<string[]>([]);

  useEffect(() => {
    if (blockedUntil === null) return;
    const tick = () => setRemainingMs(Math.max(0, blockedUntil - Date.now()));
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [blockedUntil]);

  useEffect(() => {
    if (blockedUntil !== null && remainingMs === 0) setBlockedUntil(null);
  }, [blockedUntil, remainingMs]);

  const onClick = () => {
    setHints([]);
    restart.mutate(undefined, {
      onSuccess: () => {
        toast.success('Restarting the daemon', {
          description: 'The public gateway ports stay open across the restart. Reconnecting…',
        });
      },
      onError: (err) => {
        if (isApiError(err)) {
          if (err.code === 'restart_rate_limited') {
            const ms = err.retryAfterMs ?? 60_000;
            setBlockedUntil(Date.now() + ms);
            toast.warn('Restart is rate-limited', {
              description: `This boot hasn't cleared its start-limit counter yet. Try again in ${formatSeconds(ms / 1000)}.`,
            });
            return;
          }
          if (err.hints.length > 0) setHints(err.hints);
          toast.error(err);
          return;
        }
        toast.error(err);
      },
    });
  };

  const unavailable = capabilities?.systemd_control === 'unavailable';
  const disabled = restart.isPending || blockedUntil !== null || unavailable;

  return (
    <div className="space-y-2">
      <Button
        variant="secondary"
        size={size}
        icon={<RotateCw />}
        loading={restart.isPending}
        disabled={disabled}
        onClick={onClick}
        className={className}
      >
        {blockedUntil !== null
          ? `Rate-limited — ${formatSeconds(remainingMs / 1000)}`
          : unavailable
            ? `${label} (unavailable)`
            : label}
      </Button>
      {unavailable ? (
        <p className="text-xs text-[var(--lm-text-muted)]">
          No service manager is reachable on this host. Restart it by hand:
        </p>
      ) : null}
      {unavailable || hints.length > 0 ? (
        <div className="space-y-1.5">
          {(unavailable ? ['sudo systemctl restart llamaman.service'] : hints).map((hint) => (
            <CommandLine key={hint} command={hint} />
          ))}
        </div>
      ) : null}
    </div>
  );
}
