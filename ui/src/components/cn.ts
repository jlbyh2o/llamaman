import { clsx } from 'clsx';
import type { ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

/**
 * Conditional class composition with conflict resolution.
 *
 * `twMerge` is what makes a `className` prop on a kit component actually override the component's
 * own utilities instead of racing them in source order — `cn('px-3', props.className)` with
 * `className="px-6"` yields `px-6`, not both.
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
