import * as TooltipPrimitive from '@radix-ui/react-tooltip';
import type { ReactNode } from 'react';
import { cn } from './cn';

/**
 * Tooltips.
 *
 * `<TooltipProvider>` wraps the app once, in the shell. A tooltip is for elaboration only — never
 * for information a user must have, which goes in visible copy, and never as the only label on an
 * icon button, which gets `aria-label` as well.
 */

export const TooltipProvider = TooltipPrimitive.Provider;

export interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  side?: 'top' | 'right' | 'bottom' | 'left';
  className?: string;
  /** Match the trigger's width for a long explanation. */
  wide?: boolean;
}

export function Tooltip({
  content,
  children,
  side = 'top',
  className,
  wide = false,
}: TooltipProps) {
  return (
    <TooltipPrimitive.Root>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content
          side={side}
          sideOffset={6}
          collisionPadding={8}
          style={{ zIndex: 'var(--lm-z-tooltip)' }}
          className={cn(
            'rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-raised)]',
            'px-2 py-1 text-xs text-[var(--lm-text)] shadow-[var(--lm-shadow)]',
            wide ? 'max-w-xs' : 'max-w-56',
            className,
          )}
        >
          {content}
          <TooltipPrimitive.Arrow className="fill-[var(--lm-surface-raised)]" />
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
