/**
 * The autostart toggle, and the two sentences section 2.8 requires it to say.
 *
 * `instances.autostart` is the unit's *enabled* state — a statement about **host boots** — while
 * `desired_state` is a statement about **now**. They are joined at exactly one point, the first
 * supervisor pass after a boot (D53), and never anywhere else: `PUT /instances/{id}/autostart` runs
 * `EnableUnitFiles`/`DisableUnitFiles` "**only**; never starts or stops".
 *
 * Which makes the two cross-axis cases the ones worth warning about, exactly as section 4 says:
 * turning it *off* on a running instance (it is serving now and will not come back after a reboot),
 * and turning it *on* on a stopped one (it will come back at the next boot, but it is not running
 * now — the API's own `"hint":"start_now"`).
 */

import { useState } from 'react';
import { ConfirmDialog, Switch } from '../../../components';

const RUNNING = new Set(['starting', 'loading', 'ready', 'degraded']);

export interface AutostartToggleProps {
  enabled: boolean;
  state: string;
  pending?: boolean;
  disabled?: boolean;
  onChange: (enabled: boolean) => void;
  'aria-label'?: string;
}

export function AutostartToggle({
  enabled,
  state,
  pending = false,
  disabled = false,
  onChange,
  ...aria
}: AutostartToggleProps) {
  const [asking, setAsking] = useState<'enable' | 'disable' | null>(null);
  const running = RUNNING.has(state);

  const request = (next: boolean) => {
    if (next && !running) return setAsking('enable');
    if (!next && running) return setAsking('disable');
    onChange(next);
  };

  return (
    <>
      <span
        className="inline-flex"
        title={
          enabled
            ? 'The unit is enabled: this instance starts at boot.'
            : 'The unit is disabled: this instance does not start at boot.'
        }
      >
        <Switch
          checked={enabled}
          pending={pending}
          disabled={disabled}
          onCheckedChange={request}
          aria-label={aria['aria-label'] ?? 'Start at boot'}
        />
      </span>

      <ConfirmDialog
        open={asking === 'enable'}
        onOpenChange={(open) => setAsking(open ? 'enable' : null)}
        title="Start this instance at boot?"
        description="Enabling the unit does not start anything now."
        consequences="This instance is stopped. It will come back at the next boot — but not before then."
        confirmLabel="Enable for boot"
        tone="primary"
        onConfirm={() => {
          setAsking(null);
          onChange(true);
        }}
      />

      <ConfirmDialog
        open={asking === 'disable'}
        onOpenChange={(open) => setAsking(open ? 'disable' : null)}
        title="Leave this instance out of the next boot?"
        description="Disabling the unit does not stop anything now."
        consequences="This instance is running and keeps running. After a reboot it will not come back until it is started by hand."
        confirmLabel="Disable at boot"
        tone="danger"
        onConfirm={() => {
          setAsking(null);
          onChange(false);
        }}
      />
    </>
  );
}
