import * as ProgressPrimitive from '@radix-ui/react-progress';
import type { ReactNode } from 'react';
import { cn } from './cn';
import type { Tone } from './Badge';

/**
 * Determinate and indeterminate progress.
 *
 * "Anything job-backed renders the job's progress rather than a spinner" (DESIGN section 4), so
 * this is the shape a download, a build, a scan and a bench sweep all take. `value === null` is the
 * honest indeterminate case — work is happening and the daemon has not yet said how much.
 */

const FILL: Record<Tone, string> = {
  neutral: 'bg-[var(--lm-text-faint)]',
  accent: 'bg-[var(--lm-accent)]',
  ok: 'bg-[var(--lm-ok)]',
  warn: 'bg-[var(--lm-warn)]',
  danger: 'bg-[var(--lm-danger)]',
  info: 'bg-[var(--lm-info)]',
};

export interface ProgressProps {
  /** 0–100, or null for indeterminate. */
  value: number | null;
  tone?: Tone;
  /** The line above the bar: what is happening. */
  label?: ReactNode;
  /** The right-hand line above the bar: how much, how fast, how long left. */
  detail?: ReactNode;
  size?: 'sm' | 'md';
  className?: string;
  'aria-label'?: string;
}

export function Progress({
  value,
  tone = 'accent',
  label,
  detail,
  size = 'md',
  className,
  ...aria
}: ProgressProps) {
  const clamped = value === null ? null : Math.max(0, Math.min(100, value));

  return (
    <div className={cn('w-full', className)}>
      {label || detail ? (
        <div className="mb-1 flex items-baseline justify-between gap-3 text-xs">
          <span className="truncate text-[var(--lm-text-muted)]">{label}</span>
          {detail ? (
            <span className="lm-numeric shrink-0 text-[var(--lm-text-faint)]">{detail}</span>
          ) : null}
        </div>
      ) : null}

      <ProgressPrimitive.Root
        {...aria}
        value={clamped}
        className={cn(
          'relative w-full overflow-hidden rounded-[var(--lm-radius-full)] bg-[var(--lm-surface-sunken)]',
          size === 'sm' ? 'h-1' : 'h-1.5',
        )}
      >
        {clamped === null ? (
          <div
            className={cn('h-full w-1/3 animate-pulse rounded-[var(--lm-radius-full)]', FILL[tone])}
          />
        ) : (
          <ProgressPrimitive.Indicator
            className={cn(
              'h-full rounded-[var(--lm-radius-full)] transition-[width] duration-[var(--lm-duration)]',
              FILL[tone],
            )}
            style={{ width: `${clamped}%` }}
          />
        )}
      </ProgressPrimitive.Root>
    </div>
  );
}

/**
 * The horizontal meter the dashboard uses for VRAM and disk: a filled bar whose tone reacts to how
 * full it is, with the numbers beside it rather than inside.
 */
export function Meter({
  used,
  total,
  label,
  detail,
  className,
}: {
  used: number;
  total: number;
  label: ReactNode;
  detail?: ReactNode;
  className?: string;
}) {
  const pct = total > 0 ? (used / total) * 100 : 0;
  const tone: Tone = pct >= 92 ? 'danger' : pct >= 78 ? 'warn' : 'accent';
  return (
    <Progress
      value={pct}
      tone={tone}
      label={label}
      {...(detail === undefined ? {} : { detail })}
      {...(className === undefined ? {} : { className })}
    />
  );
}
