import { useState } from 'react';
import type { ReactNode } from 'react';
import { Button } from './Button';
import { Dialog, DialogContent } from './Dialog';
import { cn } from './cn';

/**
 * The confirmation gate.
 *
 * Two levels, because this app has two kinds of destructive action:
 *
 *  - **Ordinary**: stop an instance, cancel a build, delete a token. One button.
 *  - **Typed**: the hard deletes, where section 3.10c requires a second confirmation naming "the row
 *    counts and byte totals about to be discarded, because that history is the one thing in this
 *    system that cannot be recomputed". Set `confirmPhrase` and the button stays disabled until the
 *    phrase is typed exactly.
 *
 * `consequences` is the slot for that accounting — the delete-preview numbers, the instances that
 * will stop, the free space that will come back.
 */

export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  /** What will be lost, in the caller's own words: counts, byte totals, affected instances. */
  consequences?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Danger by default; a non-destructive confirmation can use `primary`. */
  tone?: 'danger' | 'primary';
  /** When set, the confirm button unlocks only once this exact string is typed. */
  confirmPhrase?: string;
  busy?: boolean;
  onConfirm: () => void;
}

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  consequences,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  tone = 'danger',
  confirmPhrase,
  busy = false,
  onConfirm,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState('');
  const locked = confirmPhrase !== undefined && typed !== confirmPhrase;

  const close = (next: boolean) => {
    if (!next) setTyped('');
    onOpenChange(next);
  };

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent
        size="sm"
        title={title}
        {...(description === undefined ? {} : { description })}
        footer={
          <>
            <Button variant="ghost" onClick={() => close(false)} disabled={busy}>
              {cancelLabel}
            </Button>
            <Button
              variant={tone === 'danger' ? 'danger' : 'primary'}
              onClick={onConfirm}
              disabled={locked}
              loading={busy}
            >
              {confirmLabel}
            </Button>
          </>
        }
      >
        {consequences ? (
          <div
            className={cn(
              'rounded-[var(--lm-radius)] border p-3 text-xs',
              tone === 'danger'
                ? 'border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)] text-[var(--lm-text)]'
                : 'border-[var(--lm-border)] bg-[var(--lm-surface-raised)] text-[var(--lm-text-muted)]',
            )}
          >
            {consequences}
          </div>
        ) : null}

        {confirmPhrase !== undefined ? (
          <label className="mt-3 block">
            <span className="text-xs text-[var(--lm-text-muted)]">
              Type <span className="lm-numeric text-[var(--lm-text)]">{confirmPhrase}</span> to
              confirm
            </span>
            <input
              value={typed}
              onChange={(event) => setTyped(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              className={cn(
                'lm-numeric mt-1 w-full rounded-[var(--lm-radius)] border px-2 py-1.5 text-sm',
                'border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] text-[var(--lm-text)]',
                // Of every field in the app this is the one that must never lose its ring: it is
                // the confirmation that guards a purge, and a keyboard user who cannot see where
                // focus is cannot tell this field from the button beside it. See Input.tsx.
                'focus:border-[var(--lm-accent)]',
                'focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--lm-focus)]',
              )}
            />
          </label>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
