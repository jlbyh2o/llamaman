import * as Menu from '@radix-ui/react-dropdown-menu';
import type { ComponentPropsWithoutRef } from 'react';
import { cn } from './cn';

/** Row actions and the account menu. Radix owns keyboard navigation and typeahead. */

export const DropdownMenu = Menu.Root;
export const DropdownMenuTrigger = Menu.Trigger;

export function DropdownMenuContent({
  className,
  align = 'end',
  sideOffset = 4,
  ...props
}: ComponentPropsWithoutRef<typeof Menu.Content>) {
  return (
    <Menu.Portal>
      <Menu.Content
        align={align}
        sideOffset={sideOffset}
        style={{ zIndex: 'var(--lm-z-dropdown)' }}
        className={cn(
          'min-w-44 overflow-hidden rounded-[var(--lm-radius)] border border-[var(--lm-border)]',
          'bg-[var(--lm-surface-raised)] p-1 shadow-[var(--lm-shadow)]',
          className,
        )}
        {...props}
      />
    </Menu.Portal>
  );
}

export function DropdownMenuItem({
  className,
  danger = false,
  ...props
}: ComponentPropsWithoutRef<typeof Menu.Item> & { danger?: boolean }) {
  return (
    <Menu.Item
      className={cn(
        'flex cursor-default items-center gap-2 rounded-[var(--lm-radius-sm)] px-2 py-1.5 text-sm outline-none',
        'data-[highlighted]:bg-[var(--lm-neutral-soft)]',
        'data-[disabled]:pointer-events-none data-[disabled]:opacity-50',
        '[&>svg]:size-4 [&>svg]:shrink-0',
        danger
          ? 'text-[var(--lm-danger)] data-[highlighted]:bg-[var(--lm-danger-soft)]'
          : 'text-[var(--lm-text)]',
        className,
      )}
      {...props}
    />
  );
}

export function DropdownMenuLabel({
  className,
  ...props
}: ComponentPropsWithoutRef<typeof Menu.Label>) {
  return (
    <Menu.Label
      className={cn(
        'px-2 py-1 text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase',
        className,
      )}
      {...props}
    />
  );
}

export function DropdownMenuSeparator({
  className,
  ...props
}: ComponentPropsWithoutRef<typeof Menu.Separator>) {
  return <Menu.Separator className={cn('my-1 h-px bg-[var(--lm-border)]', className)} {...props} />;
}
