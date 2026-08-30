import * as SelectPrimitive from '@radix-ui/react-select';
import { Check, ChevronDown } from 'lucide-react';
import type { ReactNode } from 'react';
import { cn } from './cn';

/**
 * Single-choice select over Radix.
 *
 * Used for every closed enum in the app — cache types, split modes, restart policies, channels —
 * which is why `options` takes an optional `description`: a flag whose meaning is not obvious from
 * its name gets a line of explanation in the list rather than a tooltip nobody opens.
 */

export interface SelectOption<T extends string = string> {
  value: T;
  label: ReactNode;
  description?: ReactNode;
  disabled?: boolean;
}

export interface SelectProps<T extends string = string> {
  value: T | undefined;
  onValueChange: (value: T) => void;
  options: readonly SelectOption<T>[];
  placeholder?: string;
  disabled?: boolean;
  /** Rendered on the trigger for values that are ports, sizes or tags. */
  mono?: boolean;
  id?: string;
  className?: string;
  'aria-label'?: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean;
}

export function Select<T extends string = string>({
  value,
  onValueChange,
  options,
  placeholder = 'Select…',
  disabled = false,
  mono = false,
  id,
  className,
  ...aria
}: SelectProps<T>) {
  return (
    <SelectPrimitive.Root
      value={value ?? ''}
      onValueChange={(next) => onValueChange(next as T)}
      disabled={disabled}
    >
      <SelectPrimitive.Trigger
        id={id}
        {...aria}
        className={cn(
          'inline-flex h-8 w-full items-center justify-between gap-2 rounded-[var(--lm-radius)]',
          'border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] px-2.5 text-sm',
          'text-[var(--lm-text)] data-[placeholder]:text-[var(--lm-text-faint)]',
          // The same focus treatment `Input` uses, and for the same reason: the border tint says
          // "active", and the keyboard ring is what a keyboard user navigates by. See Input.tsx.
          'hover:border-[var(--lm-border-strong)] focus:border-[var(--lm-accent)]',
          'focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--lm-focus)]',
          'disabled:cursor-not-allowed disabled:opacity-50',
          'aria-invalid:border-[var(--lm-danger)]',
          mono && 'lm-numeric text-[13px]',
          className,
        )}
      >
        <SelectPrimitive.Value placeholder={placeholder} />
        <SelectPrimitive.Icon>
          <ChevronDown aria-hidden className="size-4 text-[var(--lm-text-faint)]" />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>

      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          position="popper"
          sideOffset={4}
          style={{ zIndex: 'var(--lm-z-dropdown)' }}
          className={cn(
            'max-h-72 min-w-[var(--radix-select-trigger-width)] overflow-hidden',
            'rounded-[var(--lm-radius)] border border-[var(--lm-border)]',
            'bg-[var(--lm-surface-raised)] shadow-[var(--lm-shadow)]',
          )}
        >
          <SelectPrimitive.Viewport className="p-1">
            {options.map((option) => (
              <SelectPrimitive.Item
                key={option.value}
                value={option.value}
                disabled={option.disabled ?? false}
                className={cn(
                  'relative flex cursor-default flex-col rounded-[var(--lm-radius-sm)] py-1.5 pr-7 pl-2 outline-none',
                  'data-[highlighted]:bg-[var(--lm-neutral-soft)]',
                  'data-[disabled]:pointer-events-none data-[disabled]:opacity-50',
                )}
              >
                <SelectPrimitive.ItemText>
                  <span className={cn('text-sm text-[var(--lm-text)]', mono && 'lm-numeric')}>
                    {option.label}
                  </span>
                </SelectPrimitive.ItemText>
                {option.description ? (
                  <span className="mt-0.5 text-xs text-[var(--lm-text-muted)]">
                    {option.description}
                  </span>
                ) : null}
                <SelectPrimitive.ItemIndicator className="absolute top-1.5 right-2">
                  <Check aria-hidden className="size-4 text-[var(--lm-accent)]" />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}
