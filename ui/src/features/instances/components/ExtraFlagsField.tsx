/**
 * The escape hatch (SPEC section 3.3, DESIGN section 5.7).
 *
 * "Anything not modeled here goes in `extra_flags`, so no upstream flag is ever unreachable and no
 * upstream flag addition is a migration." The field therefore has to do three things at once: take
 * anything, refuse exactly five things, and never look like a shell prompt — because it is not one.
 * The words are split with POSIX rules and handed to `execve`; `$(…)`, backticks, globs and `;` are
 * ordinary text here, and showing the parsed words back is the clearest way to say so.
 */

import { AlertTriangle, Terminal } from 'lucide-react';
import { FormField, Textarea } from '../../../components';
import { parseExtraFlags } from '../extraFlags';
import type { Form } from './fields';

export interface ExtraFlagsFieldProps {
  form: Form;
  /** Flags the active build's `--help` does not advertise (section 5.7's churn guard). */
  unknownFlags?: readonly string[];
}

export function ExtraFlagsField({ form, unknownFlags = [] }: ExtraFlagsFieldProps) {
  const value = form.watch('extra_flags');
  const error = form.formState.errors.extra_flags?.message;
  const parsed = parseExtraFlags(value);

  return (
    <FormField
      label="Extra flags"
      flag="appended last"
      error={typeof error === 'string' ? error : undefined}
      hint={
        'Split with shell word rules — quotes group, a backslash escapes — but never run through a ' +
        'shell: no globbing, no $ expansion, no command substitution. --host, --port, -m/--model and ' +
        '--api-key are rendered by Llama Man and cannot be overridden here.'
      }
    >
      {(field) => (
        <div className="space-y-2">
          <Textarea
            {...field}
            {...form.register('extra_flags')}
            rows={2}
            mono
            spellCheck={false}
            placeholder="--cache-type-k q8_0 --lora /path/to/adapter.gguf"
          />

          {parsed.words.length > 0 ? (
            <ul className="flex flex-wrap items-center gap-1">
              <li aria-hidden className="text-[var(--lm-text-faint)]">
                <Terminal className="size-3.5" />
              </li>
              {parsed.words.map((word, index) => (
                <li
                  key={`${word}-${index}`}
                  className="lm-numeric rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-sunken)] px-1.5 py-0.5 text-[12px] text-[var(--lm-text-muted)]"
                >
                  {word}
                </li>
              ))}
            </ul>
          ) : null}

          {unknownFlags.length > 0 ? (
            <p className="flex items-start gap-1.5 text-xs text-[var(--lm-warn)]">
              <AlertTriangle aria-hidden className="mt-0.5 size-3.5 shrink-0" />
              The active build does not advertise {unknownFlags.join(', ')}. llama.cpp ships
              nightlies daily, so this is a warning rather than a refusal.
            </p>
          ) : null}
        </div>
      )}
    </FormField>
  );
}
