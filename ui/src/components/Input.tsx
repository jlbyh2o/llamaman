import { forwardRef } from 'react';
import type { InputHTMLAttributes, ReactNode, TextareaHTMLAttributes } from 'react';
import { cn } from './cn';

/**
 * The class every field here shares.
 *
 * The focus treatment is two things at once and both are deliberate. `focus:border-…` tints the
 * border on ANY focus, including a click, which is the affordance that says "you are typing here".
 * `focus-visible:outline-…` draws the real ring, and only when the browser judges the focus to have
 * come from the keyboard — which is exactly the rule `theme.css`'s global `:focus-visible` states.
 *
 * What is NOT here is `focus:outline-none`. It used to be, and it suppressed the ring on KEYBOARD
 * focus specifically (`focus:`, not `focus-visible:`), leaving a 1px border tint as the only
 * indication — a treatment that reads as hover, on every text input, textarea and select trigger in
 * the app, including the type-the-name field that guards a purge. Section 4's baseline names
 * focus-visible rings as a requirement and the token was already defined; the component was simply
 * opting out of it.
 */
const BASE = [
  'w-full rounded-[var(--lm-radius)] border border-[var(--lm-border)]',
  'bg-[var(--lm-surface-sunken)] px-2.5 text-sm text-[var(--lm-text)]',
  'placeholder:text-[var(--lm-text-faint)]',
  'hover:border-[var(--lm-border-strong)]',
  'focus:border-[var(--lm-accent)]',
  'focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--lm-focus)]',
  'disabled:cursor-not-allowed disabled:opacity-50',
  'aria-[invalid=true]:border-[var(--lm-danger)]',
].join(' ');

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  /** Ports, sizes, hashes and paths are technical values and get JetBrains Mono. */
  mono?: boolean;
  /** A unit or a hint pinned inside the right edge: "MiB", "tokens", ":5526". */
  suffix?: ReactNode;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { mono = false, suffix, className, ...props },
  ref,
) {
  const input = (
    <input
      ref={ref}
      {...props}
      className={cn(BASE, 'h-8', mono && 'lm-numeric text-[13px]', suffix && 'pr-12', className)}
    />
  );
  if (!suffix) return input;
  return (
    <div className="relative">
      {input}
      <span className="pointer-events-none absolute inset-y-0 right-2.5 flex items-center text-xs text-[var(--lm-text-faint)]">
        {suffix}
      </span>
    </div>
  );
});

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  mono?: boolean;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea(
  { mono = false, className, ...props },
  ref,
) {
  return (
    <textarea
      ref={ref}
      rows={props.rows ?? 3}
      {...props}
      className={cn(BASE, 'py-1.5 leading-relaxed', mono && 'lm-numeric text-[13px]', className)}
    />
  );
});
