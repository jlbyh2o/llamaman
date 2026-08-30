/**
 * One badge for every state enum in the system.
 *
 * The maps below are the visual half of DESIGN section 2's `CHECK (state IN (…))` constraints: each
 * enum value gets a tone, a label and — for the transitional values — a pulsing dot, so `loading`
 * and `ready` never look alike at a glance. Nothing else in the app decides what color a state is.
 *
 * The four derived instance flags of section 2.8 are `<FlagBadge>`, deliberately separate: they are
 * badges *beside* the state, never a state, because an instance that is serving traffic can carry
 * one and still be `ready`.
 */

import type { ReactNode } from 'react';
import { Badge } from './Badge';
import type { Tone } from './Badge';
import { cn } from './cn';

interface StateStyle {
  tone: Tone;
  label: string;
  /** In motion: the dot pulses and the label reads as a verb. */
  pulse?: boolean;
}

const INSTANCE: Record<string, StateStyle> = {
  unknown: { tone: 'neutral', label: 'Unknown' },
  stopped: { tone: 'neutral', label: 'Stopped' },
  starting: { tone: 'info', label: 'Starting', pulse: true },
  loading: { tone: 'info', label: 'Loading model', pulse: true },
  ready: { tone: 'ok', label: 'Ready' },
  degraded: { tone: 'warn', label: 'Degraded' },
  stopping: { tone: 'neutral', label: 'Stopping', pulse: true },
  failed: { tone: 'danger', label: 'Failed' },
  'crash-looping': { tone: 'danger', label: 'Crash looping', pulse: true },
};

const JOB: Record<string, StateStyle> = {
  queued: { tone: 'neutral', label: 'Queued' },
  leased: { tone: 'info', label: 'Leased', pulse: true },
  running: { tone: 'info', label: 'Running', pulse: true },
  paused: { tone: 'warn', label: 'Paused' },
  interrupted: { tone: 'warn', label: 'Interrupted' },
  succeeded: { tone: 'ok', label: 'Succeeded' },
  failed: { tone: 'danger', label: 'Failed' },
  canceled: { tone: 'neutral', label: 'Canceled' },
};

const DOWNLOAD: Record<string, StateStyle> = {
  queued: { tone: 'neutral', label: 'Queued' },
  resolving: { tone: 'info', label: 'Resolving', pulse: true },
  running: { tone: 'info', label: 'Downloading', pulse: true },
  paused: { tone: 'warn', label: 'Paused' },
  verifying: { tone: 'info', label: 'Verifying', pulse: true },
  succeeded: { tone: 'ok', label: 'Complete' },
  failed: { tone: 'danger', label: 'Failed' },
  canceled: { tone: 'neutral', label: 'Canceled' },
};

const MODEL: Record<string, StateStyle> = {
  planned: { tone: 'neutral', label: 'Planned' },
  downloading: { tone: 'info', label: 'Downloading', pulse: true },
  incomplete: { tone: 'warn', label: 'Incomplete' },
  verifying: { tone: 'info', label: 'Verifying', pulse: true },
  ready: { tone: 'ok', label: 'Ready' },
  corrupt: { tone: 'danger', label: 'Corrupt' },
  missing: { tone: 'danger', label: 'Missing' },
  deleting: { tone: 'neutral', label: 'Deleting', pulse: true },
  deleted: { tone: 'neutral', label: 'Deleted' },
};

const LLAMACPP: Record<string, StateStyle> = {
  pending: { tone: 'neutral', label: 'Pending' },
  resolving: { tone: 'info', label: 'Resolving', pulse: true },
  fetching: { tone: 'info', label: 'Fetching', pulse: true },
  building: { tone: 'info', label: 'Building', pulse: true },
  verifying: { tone: 'info', label: 'Verifying', pulse: true },
  ready: { tone: 'ok', label: 'Ready' },
  failed: { tone: 'danger', label: 'Failed' },
  failed_verification: { tone: 'danger', label: 'Verification failed' },
  canceled: { tone: 'neutral', label: 'Canceled' },
  deleting: { tone: 'neutral', label: 'Deleting', pulse: true },
  deleted: { tone: 'neutral', label: 'Deleted' },
};

const BENCH: Record<string, StateStyle> = {
  draft: { tone: 'neutral', label: 'Draft' },
  queued: { tone: 'neutral', label: 'Queued' },
  preflight: { tone: 'info', label: 'Preflight', pulse: true },
  running: { tone: 'info', label: 'Running', pulse: true },
  succeeded: { tone: 'ok', label: 'Complete' },
  partial: { tone: 'warn', label: 'Partial' },
  failed: { tone: 'danger', label: 'Failed' },
  canceled: { tone: 'neutral', label: 'Canceled' },
};

const TOKEN: Record<string, StateStyle> = {
  active: { tone: 'ok', label: 'Active' },
  disabled: { tone: 'warn', label: 'Disabled' },
  revoked: { tone: 'danger', label: 'Revoked' },
};

const SELF_UPDATE: Record<string, StateStyle> = {
  planned: { tone: 'neutral', label: 'Planned' },
  downloading: { tone: 'info', label: 'Downloading', pulse: true },
  verifying: { tone: 'info', label: 'Verifying', pulse: true },
  staged: { tone: 'info', label: 'Staged' },
  swapping: { tone: 'warn', label: 'Swapping', pulse: true },
  succeeded: { tone: 'ok', label: 'Succeeded' },
  failed: { tone: 'danger', label: 'Failed' },
  canceled: { tone: 'neutral', label: 'Canceled' },
};

const WIZARD: Record<string, StateStyle> = {
  pending: { tone: 'neutral', label: 'Pending' },
  active: { tone: 'accent', label: 'In progress', pulse: true },
  skipped: { tone: 'neutral', label: 'Skipped' },
  complete: { tone: 'ok', label: 'Complete' },
};

const LEVEL: Record<string, StateStyle> = {
  debug: { tone: 'neutral', label: 'Debug' },
  info: { tone: 'info', label: 'Info' },
  warn: { tone: 'warn', label: 'Warning' },
  error: { tone: 'danger', label: 'Error' },
};

const FIT: Record<string, StateStyle> = {
  fits: { tone: 'ok', label: 'Fits in VRAM' },
  partial: { tone: 'warn', label: 'Partial offload' },
  wont_run: { tone: 'danger', label: "Won't run" },
};

export const STATE_MAPS = {
  instance: INSTANCE,
  job: JOB,
  download: DOWNLOAD,
  model: MODEL,
  llamacpp: LLAMACPP,
  bench: BENCH,
  token: TOKEN,
  selfUpdate: SELF_UPDATE,
  wizard: WIZARD,
  level: LEVEL,
  fit: FIT,
} as const;

export type StateKind = keyof typeof STATE_MAPS;

/** The style for one state, or a neutral fallback that shows the raw value rather than hiding it. */
export function stateStyle(kind: StateKind, state: string): StateStyle {
  return STATE_MAPS[kind][state] ?? { tone: 'neutral', label: state };
}

export interface StatusBadgeProps {
  kind: StateKind;
  state: string;
  /** Override the mapped label — "Ready · 3/8 slots", say. */
  label?: ReactNode;
  className?: string;
}

export function StatusBadge({ kind, state, label, className }: StatusBadgeProps) {
  const style = stateStyle(kind, state);
  return (
    <Badge
      tone={style.tone}
      dot
      pulse={style.pulse ?? false}
      className={className}
      title={state}
      data-state={state}
    >
      {label ?? style.label}
    </Badge>
  );
}

/** The four derived flags of section 2.8, plus the ones the update and unit checks raise. */
const FLAGS: Record<string, { tone: Tone; label: string; hint: string }> = {
  restart_required: {
    tone: 'warn',
    label: 'Restart required',
    hint: 'The saved configuration differs from what is running. Restart to apply it.',
  },
  stale_version: {
    tone: 'warn',
    label: 'Stale build',
    hint: 'This process is running an older llama.cpp than the active version.',
  },
  inhibited: {
    tone: 'warn',
    label: 'Inhibited',
    hint: 'Something is holding this instance back from starting.',
  },
  draft_unverified: {
    tone: 'warn',
    label: 'Draft unverified',
    hint: 'The draft model’s tokenizer has not been checked against the main model yet.',
  },
  draft_mismatch: {
    tone: 'danger',
    label: 'Draft mismatch',
    hint: 'The draft model’s vocabulary does not match. This instance will refuse to start.',
  },
  safe_mode: {
    tone: 'info',
    label: 'Safe mode',
    hint: 'Running with -ngl 0 and a small context. Restart to apply the saved configuration.',
  },
};

export function FlagBadge({
  flag,
  reason,
  className,
}: {
  flag: keyof typeof FLAGS | string;
  /** `inhibit_reason` and friends, appended to the tooltip. */
  reason?: string | null;
  className?: string;
}) {
  const style = FLAGS[flag] ?? { tone: 'warn' as Tone, label: flag, hint: flag };
  return (
    <Badge
      tone={style.tone}
      className={cn('cursor-default', className)}
      title={reason ? `${style.hint} (${reason})` : style.hint}
    >
      {style.label}
    </Badge>
  );
}
