/**
 * The instance form's field primitives.
 *
 * DESIGN section 4 sizes this form at "~40 interdependent optional fields" and it is closer to
 * sixty once the draft tuning and the endpoint toggles are counted. Three wrappers keep that
 * readable: a text field, a select, and the tri-state that every nullable boolean in `model.FlagSet`
 * needs — because "not set", "on" and "off" are three different statements and a checkbox can only
 * make two of them.
 *
 * Each wrapper takes the whole `useForm` return rather than a `control` plus a `register` plus an
 * `errors`, so a field is one line at the call site and the ARIA wiring is decided once in
 * `<FormField>`.
 */

import type { ReactNode } from 'react';
import { Controller } from 'react-hook-form';
import type { UseFormReturn } from 'react-hook-form';
import { FormField, Input, Select, Textarea } from '../../../components';
import type { FieldRenderProps, SelectOption } from '../../../components';
import type { InstanceFormValues, TriState } from '../values';

/** Field names whose value is a string — everything but `autostart` and `device_uuids`. */
export type StringFieldName = {
  [K in keyof InstanceFormValues]: InstanceFormValues[K] extends string ? K : never;
}[keyof InstanceFormValues];

export type Form = UseFormReturn<InstanceFormValues>;

/**
 * `<FormField>`'s render props, minus the keys that are `undefined`.
 *
 * `exactOptionalPropertyTypes` is on, and `SelectProps` declares `'aria-describedby'?: string` — so
 * spreading a render-prop object that carries an explicit `undefined` is a type error. This drops
 * those keys instead of widening every consumer's props.
 */
export function aria(field: FieldRenderProps): {
  id: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean;
} {
  const out: { id: string; 'aria-describedby'?: string; 'aria-invalid'?: boolean } = {
    id: field.id,
  };
  const describedBy = field['aria-describedby'];
  if (describedBy !== undefined) out['aria-describedby'] = describedBy;
  const invalid = field['aria-invalid'];
  if (invalid !== undefined) out['aria-invalid'] = invalid;
  return out;
}

function messageOf(form: Form, name: StringFieldName): string | undefined {
  const error = form.formState.errors[name];
  return typeof error?.message === 'string' ? error.message : undefined;
}

interface CommonProps {
  form: Form;
  name: StringFieldName;
  label: ReactNode;
  /** The upstream flag, shown in monospace beside the label: `-c`, `--jinja`. */
  flag?: string;
  hint?: ReactNode;
  required?: boolean;
  disabled?: boolean;
  className?: string;
}

export interface TextFieldProps extends CommonProps {
  placeholder?: string;
  mono?: boolean;
  suffix?: ReactNode;
  /** `number` gets the numeric keyboard and the spinner; validation is still zod's. */
  numeric?: boolean;
  inline?: boolean;
}

export function TextField({
  form,
  name,
  label,
  flag,
  hint,
  required = false,
  disabled = false,
  placeholder,
  mono = false,
  suffix,
  numeric = false,
  inline = false,
  className,
}: TextFieldProps) {
  return (
    <FormField
      label={label}
      required={required}
      inline={inline}
      error={messageOf(form, name)}
      {...(flag === undefined ? {} : { flag })}
      {...(hint === undefined ? {} : { hint })}
      {...(className === undefined ? {} : { className })}
    >
      {(field) => (
        <Input
          {...field}
          {...form.register(name)}
          disabled={disabled}
          mono={mono}
          inputMode={numeric ? 'numeric' : undefined}
          autoComplete="off"
          spellCheck={false}
          {...(placeholder === undefined ? {} : { placeholder })}
          {...(suffix === undefined ? {} : { suffix })}
        />
      )}
    </FormField>
  );
}

export interface TextAreaFieldProps extends CommonProps {
  placeholder?: string;
  mono?: boolean;
  rows?: number;
}

export function TextAreaField({
  form,
  name,
  label,
  flag,
  hint,
  required = false,
  disabled = false,
  placeholder,
  mono = false,
  rows = 3,
  className,
}: TextAreaFieldProps) {
  return (
    <FormField
      label={label}
      required={required}
      error={messageOf(form, name)}
      {...(flag === undefined ? {} : { flag })}
      {...(hint === undefined ? {} : { hint })}
      {...(className === undefined ? {} : { className })}
    >
      {(field) => (
        <Textarea
          {...field}
          {...form.register(name)}
          rows={rows}
          disabled={disabled}
          mono={mono}
          spellCheck={false}
          {...(placeholder === undefined ? {} : { placeholder })}
        />
      )}
    </FormField>
  );
}

export interface SelectFieldProps extends CommonProps {
  options: readonly SelectOption[];
  placeholder?: string;
  mono?: boolean;
  inline?: boolean;
}

export function SelectField({
  form,
  name,
  label,
  flag,
  hint,
  required = false,
  disabled = false,
  options,
  placeholder,
  mono = false,
  inline = false,
  className,
}: SelectFieldProps) {
  return (
    <FormField
      label={label}
      required={required}
      inline={inline}
      error={messageOf(form, name)}
      {...(flag === undefined ? {} : { flag })}
      {...(hint === undefined ? {} : { hint })}
      {...(className === undefined ? {} : { className })}
    >
      {(field) => (
        <Controller
          control={form.control}
          name={name}
          render={({ field: controlled }) => (
            <Select
              {...aria(field)}
              value={String(controlled.value)}
              onValueChange={controlled.onChange}
              options={options}
              disabled={disabled}
              mono={mono}
              {...(placeholder === undefined ? {} : { placeholder })}
            />
          )}
        />
      )}
    </FormField>
  );
}

/**
 * The nullable boolean, as three explicit choices.
 *
 * `null` in `flags_json` means "do not pass the flag", which is not the same as passing `false` —
 * `--mlock` off is a decision llama-server acts on, and "unset" leaves its own default in place.
 * The labels say which is which rather than making the reader remember.
 */
export interface TriFieldProps extends CommonProps {
  /** What each state means for this flag, e.g. `{ on: 'Locked in RAM', off: 'Pageable' }`. */
  labels?: { unset?: string; on?: string; off?: string };
  inline?: boolean;
}

export function TriField({ labels, ...props }: TriFieldProps) {
  const options: SelectOption<TriState>[] = [
    { value: '', label: labels?.unset ?? 'Leave to llama.cpp' },
    { value: 'on', label: labels?.on ?? 'On' },
    { value: 'off', label: labels?.off ?? 'Off' },
  ];
  return <SelectField {...props} options={options} />;
}
