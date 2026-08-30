import { forwardRef } from 'react';
import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { Loader2 } from 'lucide-react';
import { cn } from './cn';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'link';
export type ButtonSize = 'sm' | 'md' | 'lg' | 'icon';

const VARIANTS: Record<ButtonVariant, string> = {
  primary:
    'bg-[var(--lm-accent)] text-[var(--lm-accent-contrast)] hover:bg-[var(--lm-accent-hover)] active:bg-[var(--lm-accent-active)] border border-transparent',
  secondary:
    'bg-[var(--lm-surface-raised)] text-[var(--lm-text)] border border-[var(--lm-border)] hover:border-[var(--lm-border-strong)] hover:bg-[var(--lm-surface)]',
  ghost:
    'bg-transparent text-[var(--lm-text-muted)] border border-transparent hover:bg-[var(--lm-neutral-soft)] hover:text-[var(--lm-text)]',
  danger:
    'bg-[var(--lm-danger-soft)] text-[var(--lm-danger)] border border-[var(--lm-danger)]/40 hover:border-[var(--lm-danger)]',
  link: 'bg-transparent border border-transparent text-[var(--lm-accent)] underline-offset-4 hover:underline px-0',
};

const SIZES: Record<ButtonSize, string> = {
  sm: 'h-7 px-2.5 text-xs gap-1.5',
  md: 'h-8 px-3 text-sm gap-2',
  lg: 'h-10 px-4 text-sm gap-2',
  icon: 'h-8 w-8 p-0 justify-center',
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Shows a spinner and disables the button; the label stays, so the width does not jump. */
  loading?: boolean;
  /** Rendered before the label. Omitted while `loading`, whose spinner takes the same slot. */
  icon?: ReactNode;
}

/**
 * The one button.
 *
 * Everything clickable that is not a router link uses it, so focus rings, disabled treatment and
 * the loading affordance are decided once. `disabled` and `loading` both block the click; a
 * job-backed action shows the job's progress instead (DESIGN section 4) and does not spin here.
 */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'md', loading = false, icon, className, children, ...props },
  ref,
) {
  return (
    <button
      ref={ref}
      type={props.type ?? 'button'}
      {...props}
      disabled={props.disabled || loading}
      aria-busy={loading || undefined}
      className={cn(
        'inline-flex items-center rounded-[var(--lm-radius)] font-medium whitespace-nowrap',
        'transition-colors duration-[var(--lm-duration-fast)]',
        'disabled:cursor-not-allowed disabled:opacity-50',
        VARIANTS[variant],
        SIZES[size],
        className,
      )}
    >
      {loading ? (
        <Loader2 aria-hidden className="size-4 shrink-0 animate-spin" />
      ) : icon ? (
        <span aria-hidden className="shrink-0 [&>svg]:size-4">
          {icon}
        </span>
      ) : null}
      {children}
    </button>
  );
});
