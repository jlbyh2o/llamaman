/**
 * Start, stop, restart, safe start, reset failed — with the confirmations each one earns.
 *
 * DESIGN section 4, screen 6 lists the five; sections 3.10 and 3.10b decide which of them need to
 * be explained before they happen:
 *
 *  - **Stop** drains for `?drain_sec`, and on an autostart instance the response carries
 *    `"hint":"will_start_at_boot"` — so the dialog says that up front rather than letting it be a
 *    surprise at the next reboot.
 *  - **Safe start** is F3's recovery: one run with `-ngl 0 -c 2048`, delivered through
 *    `pending_override_json`, **never persisted**. While it is the running configuration the
 *    instance wears `restart_required`, because the saved configuration is not what is live. All of
 *    that has to be in the dialog, because a user who does not know it will read the badge as a bug.
 *  - **Reset failed** stamps `restart_window_reset_at`, so the crash-loop window genuinely starts
 *    over instead of the state re-latching on the next failure (D64).
 *
 * Every control can answer `409 systemd_unavailable` or `409 systemd_denied` on a degraded host
 * (F9/F10), each carrying the exact command to run by hand — which is why failures here surface the
 * hints rather than a generic error.
 */

import { useState } from 'react';
import { LifeBuoy, Play, RotateCcw, Square, Wrench } from 'lucide-react';
import { Button, ConfirmDialog } from '../../../components';
import type { ControlAction } from '../api';

export interface InstanceActionsProps {
  state: string;
  desiredState: string;
  autostart: boolean;
  inhibited?: boolean;
  busy?: boolean;
  /** Null while the daemon does not serve the control routes; the buttons then explain. */
  onAction: ((action: ControlAction, drainSec?: number) => void) | null;
  size?: 'sm' | 'md';
}

const RUNNING = new Set(['starting', 'loading', 'ready', 'degraded', 'stopping']);

export function InstanceActions({
  state,
  desiredState,
  autostart,
  inhibited = false,
  busy = false,
  onAction,
  size = 'md',
}: InstanceActionsProps) {
  const [confirm, setConfirm] = useState<'stop' | 'restart' | 'safe-start' | null>(null);
  const running = RUNNING.has(state);
  const disabled = onAction === null;
  const title = disabled ? 'This daemon cannot control units right now.' : undefined;

  const run = (action: ControlAction, drainSec?: number) => {
    setConfirm(null);
    onAction?.(action, drainSec);
  };

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        {running ? (
          <Button
            size={size}
            icon={<Square />}
            disabled={disabled}
            loading={busy}
            title={title}
            onClick={() => setConfirm('stop')}
          >
            Stop
          </Button>
        ) : (
          <Button
            size={size}
            variant="primary"
            icon={<Play />}
            disabled={disabled}
            loading={busy}
            title={title}
            onClick={() => run('start')}
          >
            Start
          </Button>
        )}

        <Button
          size={size}
          icon={<RotateCcw />}
          disabled={disabled || (!running && desiredState !== 'running')}
          title={title}
          onClick={() => setConfirm('restart')}
        >
          Restart
        </Button>

        <Button
          size={size}
          icon={<LifeBuoy />}
          disabled={disabled}
          title={title}
          onClick={() => setConfirm('safe-start')}
        >
          Safe start
        </Button>

        {state === 'crash-looping' || state === 'failed' || inhibited ? (
          <Button
            size={size}
            icon={<Wrench />}
            disabled={disabled}
            title={title}
            onClick={() => run('reset-failed')}
          >
            Reset failed
          </Button>
        ) : null}
      </div>

      <ConfirmDialog
        open={confirm === 'stop'}
        onOpenChange={(open) => setConfirm(open ? 'stop' : null)}
        title="Stop this instance?"
        description="In-flight requests are given 30 seconds to finish before the unit is stopped."
        consequences={
          autostart
            ? 'Autostart is on, so this instance will start again at the next boot. Stopping it now does not change that.'
            : 'The gateway will answer 503 for this instance until it is started again.'
        }
        confirmLabel="Stop"
        tone="danger"
        onConfirm={() => run('stop', 30)}
      />

      <ConfirmDialog
        open={confirm === 'restart'}
        onOpenChange={(open) => setConfirm(open ? 'restart' : null)}
        title="Restart this instance?"
        description="The model is unloaded and loaded again, which takes as long as the first load did."
        consequences="Requests in flight are dropped when the process exits."
        confirmLabel="Restart"
        tone="primary"
        onConfirm={() => run('restart')}
      />

      <ConfirmDialog
        open={confirm === 'safe-start'}
        onOpenChange={(open) => setConfirm(open ? 'safe-start' : null)}
        title="Start once in safe mode?"
        description="One run with -ngl 0 and a 2048-token context, to tell a GPU problem from a model problem."
        consequences={
          <>
            The override is never saved: your configuration is untouched, and the next start — from
            any trigger — uses it again. While the safe run is live this instance shows “restart
            required”, because what is running is not what is saved.
          </>
        }
        confirmLabel="Safe start"
        tone="primary"
        onConfirm={() => run('safe-start')}
      />
    </>
  );
}
