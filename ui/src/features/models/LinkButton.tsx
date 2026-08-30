/**
 * A link that looks like a button.
 *
 * The kit's `<Button>` renders a `<button>`, and an `<a>` may not contain interactive content — so
 * `<Link><Button/></Link>`, the obvious spelling, is invalid HTML and gives a screen reader two
 * nested controls where the page has one. Everywhere the models area offers "go here" as its
 * primary affordance (browse the Hub, request access, create an instance) the control really is a
 * link: it must middle-click into a new tab, and it must show its target in the status bar.
 *
 * So the element stays a link and only the styling is borrowed. This is exported as a **class
 * builder** rather than as a wrapper component on purpose: TanStack Router's `<Link>` infers its
 * `to`, `params` and `search` types from the route tree, and any wrapper that re-declares its props
 * erases that inference — `search={{ group: 'huggingface' }}` would stop being checked against the
 * route it is aimed at. A className keeps every call site fully typed.
 *
 * The class lists mirror `components/Button.tsx`; if a second area needs this, it belongs in the kit
 * beside that file rather than duplicated again.
 */

import type { ReactNode } from 'react';

import { cn } from '../../components/cn';

export type LinkButtonVariant = 'primary' | 'secondary' | 'ghost';

const VARIANTS: Record<LinkButtonVariant, string> = {
  primary:
    'bg-[var(--lm-accent)] text-[var(--lm-accent-contrast)] hover:bg-[var(--lm-accent-hover)] active:bg-[var(--lm-accent-active)] border border-transparent',
  secondary:
    'bg-[var(--lm-surface-raised)] text-[var(--lm-text)] border border-[var(--lm-border)] hover:border-[var(--lm-border-strong)] hover:bg-[var(--lm-surface)]',
  ghost:
    'bg-transparent text-[var(--lm-text-muted)] border border-transparent hover:bg-[var(--lm-neutral-soft)] hover:text-[var(--lm-text)]',
};

const SIZES = { sm: 'h-7 px-2.5 text-xs gap-1.5', md: 'h-8 px-3 text-sm gap-2' } as const;

/** Spread onto a `<Link className={…}>` or an `<a>`. */
export function linkButtonClass(
  variant: LinkButtonVariant = 'secondary',
  size: keyof typeof SIZES = 'md',
  className?: string,
): string {
  return cn(
    'inline-flex items-center rounded-[var(--lm-radius)] font-medium whitespace-nowrap',
    'transition-colors duration-[var(--lm-duration-fast)]',
    VARIANTS[variant],
    SIZES[size],
    className,
  );
}

/** The icon slot, matching `<Button icon={…}>`. */
export function LinkIcon({ children }: { children: ReactNode }) {
  return (
    <span aria-hidden className="shrink-0 [&>svg]:size-4">
      {children}
    </span>
  );
}

/**
 * An external URL — the Hub, a gated repository's access page — rendered as a button. No route
 * inference is involved here, so this one can be a component.
 */
export function ExternalLinkButton({
  href,
  variant = 'secondary',
  size = 'md',
  icon,
  className,
  children,
}: {
  href: string;
  variant?: LinkButtonVariant;
  size?: keyof typeof SIZES;
  icon?: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer noopener"
      className={linkButtonClass(variant, size, className)}
    >
      {icon ? <LinkIcon>{icon}</LinkIcon> : null}
      {children}
    </a>
  );
}
