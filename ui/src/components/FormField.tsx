import { useId } from 'react';
import type { ReactNode } from 'react';
import { AlertCircle } from 'lucide-react';
import { cn } from './cn';

/**
 * The field wrapper.
 *
 * The instance form has "~40 interdependent optional fields" (DESIGN section 4), so the label, the
 * hint, the error and the ARIA wiring that ties them together are decided once, here, and every
 * control is handed the ids it needs through a render prop:
 *
 * ```tsx
 * <FormField label="Context size" hint="Tokens per slot × parallel slots." error={errors.ctx_size?.message}>
 *   {(field) => <Input {...field} mono {...register('ctx_size', { valueAsNumber: true })} />}
 * </FormField>
 * ```
 *
 * `field` carries `id`, `aria-describedby` and `aria-invalid`, so a screen reader reads the hint
 * and the error with the control rather than after it. Nothing here knows about react-hook-form —
 * `error` is a string, which is what a zod resolver produces — so the same wrapper serves an
 * uncontrolled `register` input, a `Controller`-driven Select and a plain useState field.
 */

export interface FieldRenderProps {
  id: string;
  'aria-describedby': string | undefined;
  'aria-invalid': boolean | undefined;
}

export interface FormFieldProps {
  label: ReactNode;
  children: (field: FieldRenderProps) => ReactNode;
  /** Explanation shown under the control, always visible — never a tooltip. */
  hint?: ReactNode;
  /** The resolver's message for this field. */
  error?: string | undefined;
  required?: boolean;
  /** The upstream flag this maps to, shown in monospace beside the label: `--ctx-size`. */
  flag?: string;
  /** Lay the label out to the left instead of above, for dense settings groups. */
  inline?: boolean;
  className?: string;
}

export function FormField({
  label,
  children,
  hint,
  error,
  required = false,
  flag,
  inline = false,
  className,
}: FormFieldProps) {
  const base = useId();
  const id = `${base}-control`;
  const hintId = hint ? `${base}-hint` : undefined;
  const errorId = error ? `${base}-error` : undefined;
  const describedBy = [errorId, hintId].filter(Boolean).join(' ') || undefined;

  const labelNode = (
    <label
      htmlFor={id}
      className="flex items-baseline gap-2 text-sm font-medium text-[var(--lm-text)]"
    >
      <span>
        {label}
        {required ? (
          <span aria-hidden className="ml-0.5 text-[var(--lm-danger)]">
            *
          </span>
        ) : null}
      </span>
      {flag ? (
        <span className="lm-numeric text-[11px] text-[var(--lm-text-faint)]">{flag}</span>
      ) : null}
    </label>
  );

  const body = (
    <>
      {children({ id, 'aria-describedby': describedBy, 'aria-invalid': error ? true : undefined })}
      {error ? (
        <p
          id={errorId}
          className="mt-1 flex items-center gap-1.5 text-xs text-[var(--lm-danger)]"
          role="alert"
        >
          <AlertCircle aria-hidden className="size-3.5 shrink-0" />
          {error}
        </p>
      ) : null}
      {hint ? (
        <p id={hintId} className="mt-1 text-xs text-[var(--lm-text-muted)]">
          {hint}
        </p>
      ) : null}
    </>
  );

  if (inline) {
    return (
      <div
        className={cn(
          'grid grid-cols-[minmax(0,14rem)_minmax(0,1fr)] items-start gap-4',
          className,
        )}
      >
        <div className="pt-1.5">{labelNode}</div>
        <div className="min-w-0">{body}</div>
      </div>
    );
  }

  return (
    <div className={cn('min-w-0', className)}>
      {labelNode}
      <div className="mt-1.5">{body}</div>
    </div>
  );
}

/** A titled group of fields — the three panes of the instance form, the nine settings groups. */
export function FieldGroup({
  title,
  description,
  children,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={cn('space-y-4', className)}>
      <div>
        <h3 className="text-sm font-semibold text-[var(--lm-text)]">{title}</h3>
        {description ? (
          <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">{description}</p>
        ) : null}
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  );
}
