import * as TabsPrimitive from '@radix-ui/react-tabs';
import type { ComponentPropsWithoutRef } from 'react';
import { cn } from './cn';

/**
 * Tabs.
 *
 * Where the selection is a filter or a view the user might link to — the settings groups, the
 * instance form's three panes — the screen drives `value`/`onValueChange` from a router search
 * param, because filters and comparisons belong in the URL (DESIGN section 4).
 */

export const Tabs = TabsPrimitive.Root;

export function TabsList({
  className,
  ...props
}: ComponentPropsWithoutRef<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      className={cn(
        'inline-flex items-center gap-1 rounded-[var(--lm-radius)] border border-[var(--lm-border)]',
        'bg-[var(--lm-surface-sunken)] p-1',
        className,
      )}
      {...props}
    />
  );
}

export function TabsTrigger({
  className,
  ...props
}: ComponentPropsWithoutRef<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      className={cn(
        'rounded-[var(--lm-radius-sm)] px-3 py-1 text-sm font-medium whitespace-nowrap',
        'text-[var(--lm-text-muted)] transition-colors duration-[var(--lm-duration-fast)]',
        'hover:text-[var(--lm-text)]',
        'data-[state=active]:bg-[var(--lm-surface-raised)] data-[state=active]:text-[var(--lm-text)]',
        'data-[state=active]:shadow-[var(--lm-shadow)]',
        'disabled:cursor-not-allowed disabled:opacity-50',
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({
  className,
  ...props
}: ComponentPropsWithoutRef<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content className={cn('mt-4 focus:outline-none', className)} {...props} />;
}
