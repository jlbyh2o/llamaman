import type { HTMLAttributes, ReactNode } from 'react';
import { cn } from './cn';

export type Tone = 'neutral' | 'accent' | 'ok' | 'warn' | 'danger' | 'info';

const TONES: Record<Tone, string> = {
  neutral: 'bg-[var(--lm-neutral-soft)] text-[var(--lm-text-muted)] ring-[var(--lm-border-strong)]',
  accent: 'bg-[var(--lm-accent-soft)] text-[var(--lm-accent)] ring-[var(--lm-accent)]/35',
  ok: 'bg-[var(--lm-ok-soft)] text-[var(--lm-ok)] ring-[var(--lm-ok)]/35',
  warn: 'bg-[var(--lm-warn-soft)] text-[var(--lm-warn)] ring-[var(--lm-warn)]/35',
  danger: 'bg-[var(--lm-danger-soft)] text-[var(--lm-danger)] ring-[var(--lm-danger)]/35',
  info: 'bg-[var(--lm-info-soft)] text-[var(--lm-info)] ring-[var(--lm-info)]/35',
};

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  tone?: Tone;
  /** A leading dot, which is what makes a column of states scannable without reading them. */
  dot?: boolean;
  /** Pulses the dot for a state that is going somewhere: starting, loading, downloading, building. */
  pulse?: boolean;
  icon?: ReactNode;
}

export function Badge({
  tone = 'neutral',
  dot = false,
  pulse = false,
  icon,
  className,
  children,
  ...props
}: BadgeProps) {
  return (
    <span
      {...props}
      className={cn(
        'inline-flex items-center gap-1.5 rounded-[var(--lm-radius-full)] px-2 py-0.5',
        'text-xs font-medium whitespace-nowrap ring-1 ring-inset',
        TONES[tone],
        className,
      )}
    >
      {dot ? (
        <span
          aria-hidden
          className={cn('size-1.5 shrink-0 rounded-full bg-current', pulse && 'animate-pulse')}
        />
      ) : null}
      {icon ? (
        <span aria-hidden className="shrink-0 [&>svg]:size-3">
          {icon}
        </span>
      ) : null}
      {children}
    </span>
  );
}
