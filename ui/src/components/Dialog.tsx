import * as DialogPrimitive from '@radix-ui/react-dialog';
import { X } from 'lucide-react';
import type { ReactNode } from 'react';
import { cn } from './cn';

/**
 * Modal dialog over Radix.
 *
 * Radix owns focus trapping, scroll locking, the escape key and the ARIA wiring; we own every
 * pixel (DESIGN section 4: "accessible headless primitives; we own all styling"). A dialog always
 * has a title — Radix warns without one, and so does a screen reader.
 */

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;

export interface DialogContentProps {
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  /** The button row along the bottom. */
  footer?: ReactNode;
  /** `md` is the default; `lg` is for the delete previews and the create-token flow. */
  size?: 'sm' | 'md' | 'lg';
  className?: string;
  /** Hide the corner close button for a dialog that must be answered. */
  dismissible?: boolean;
}

const SIZES = { sm: 'max-w-sm', md: 'max-w-lg', lg: 'max-w-2xl' } as const;

export function DialogContent({
  title,
  description,
  children,
  footer,
  size = 'md',
  className,
  dismissible = true,
}: DialogContentProps) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay
        className="fixed inset-0 bg-[var(--lm-overlay)] backdrop-blur-[1px]"
        style={{ zIndex: 'var(--lm-z-overlay)' }}
      />
      <DialogPrimitive.Content
        style={{ zIndex: 'var(--lm-z-dialog)' }}
        className={cn(
          'fixed top-1/2 left-1/2 w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2',
          'rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)]',
          'shadow-[var(--lm-shadow)] focus:outline-none',
          SIZES[size],
          className,
        )}
      >
        <div className="flex items-start justify-between gap-4 border-b border-[var(--lm-border)] px-4 py-3">
          <div className="min-w-0">
            <DialogPrimitive.Title className="text-sm font-semibold text-[var(--lm-text)]">
              {title}
            </DialogPrimitive.Title>
            {description ? (
              <DialogPrimitive.Description className="mt-1 text-xs text-[var(--lm-text-muted)]">
                {description}
              </DialogPrimitive.Description>
            ) : null}
          </div>
          {dismissible ? (
            <DialogPrimitive.Close
              aria-label="Close"
              className="-mt-1 -mr-1 rounded-[var(--lm-radius)] p-1 text-[var(--lm-text-faint)] hover:bg-[var(--lm-neutral-soft)] hover:text-[var(--lm-text)]"
            >
              <X aria-hidden className="size-4" />
            </DialogPrimitive.Close>
          ) : null}
        </div>

        {children ? (
          <div className="max-h-[70vh] overflow-y-auto px-4 py-4 text-sm text-[var(--lm-text)]">
            {children}
          </div>
        ) : null}

        {footer ? (
          <div className="flex items-center justify-end gap-2 border-t border-[var(--lm-border)] px-4 py-3">
            {footer}
          </div>
        ) : null}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}
