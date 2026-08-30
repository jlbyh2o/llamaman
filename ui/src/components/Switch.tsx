import * as SwitchPrimitive from '@radix-ui/react-switch';
import type { ComponentPropsWithoutRef } from 'react';
import { cn } from './cn';

/**
 * Toggle.
 *
 * The cheap toggles — token enable/disable, autostart — are the app's only optimistic mutations
 * (DESIGN section 4), so this renders a `pending` state that dims without moving: the thumb sits
 * where the optimistic value put it, and a rejected mutation snaps it back.
 */

export interface SwitchProps extends Omit<
  ComponentPropsWithoutRef<typeof SwitchPrimitive.Root>,
  'className'
> {
  className?: string;
  pending?: boolean;
}

export function Switch({ className, pending = false, ...props }: SwitchProps) {
  return (
    <SwitchPrimitive.Root
      {...props}
      aria-busy={pending || undefined}
      className={cn(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-[var(--lm-radius-full)]',
        'border border-transparent transition-colors duration-[var(--lm-duration-fast)]',
        'bg-[var(--lm-border-strong)] data-[state=checked]:bg-[var(--lm-accent)]',
        'disabled:cursor-not-allowed disabled:opacity-50',
        pending && 'opacity-70',
        className,
      )}
    >
      <SwitchPrimitive.Thumb
        className={cn(
          'block size-4 translate-x-0.5 rounded-full bg-[var(--lm-surface)] shadow-sm',
          'transition-transform duration-[var(--lm-duration-fast)] data-[state=checked]:translate-x-[1.125rem]',
        )}
      />
    </SwitchPrimitive.Root>
  );
}
