/**
 * A port field with the availability hint section 2.8's rules can honestly give.
 *
 * Three of those rules need no syscall — the 1024–65535 range, the management port and the internal
 * pool, and another instance's claim — so they are answered as you type. The fourth, "can this
 * process bind it right now", is the daemon's live probe and is deliberately not simulated: the
 * hint says what is known and the save says the rest. That is also why F6 exists as a runtime
 * fallback rather than a promise made here.
 */

import { Wand2 } from 'lucide-react';
import { Button, FormField, Input } from '../../../components';
import { portHint } from '../ports';
import type { FormContext, PortKind } from '../schema';
import type { Form, StringFieldName } from './fields';

export interface PortFieldProps {
  form: Form;
  name: Extract<StringFieldName, 'public_port' | 'internal_port'>;
  kind: PortKind;
  label: string;
  ctx: FormContext;
  /** `GET /ports/suggest`'s answer, or this side's walk when that route is not served. */
  suggestion?: number | undefined;
  hint?: string;
}

const TONE_CLASS = {
  ok: 'text-[var(--lm-text-muted)]',
  warn: 'text-[var(--lm-warn)]',
  danger: 'text-[var(--lm-danger)]',
} as const;

export function PortField({ form, name, kind, label, ctx, suggestion, hint }: PortFieldProps) {
  const value = form.watch(name);
  const error = form.formState.errors[name]?.message;
  const availability = portHint(kind, value, ctx);

  return (
    <FormField
      label={label}
      {...(kind === 'internal' ? { flag: '--port' } : {})}
      error={typeof error === 'string' ? error : undefined}
      hint={
        <span className="flex flex-wrap items-center gap-x-2">
          {hint ? <span>{hint}</span> : null}
          {availability ? (
            <span className={TONE_CLASS[availability.tone]}>{availability.message}</span>
          ) : null}
        </span>
      }
    >
      {(field) => (
        <div className="flex items-center gap-2">
          <Input
            {...field}
            {...form.register(name)}
            mono
            inputMode="numeric"
            autoComplete="off"
            placeholder={kind === 'public' ? '8081' : String(ctx.internalPortMin + 1)}
            className="w-40"
          />
          {suggestion !== undefined && String(suggestion) !== value.trim() ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<Wand2 />}
              onClick={() =>
                form.setValue(name, String(suggestion), {
                  shouldValidate: true,
                  shouldDirty: true,
                })
              }
            >
              Use {suggestion}
            </Button>
          ) : null}
        </div>
      )}
    </FormField>
  );
}
