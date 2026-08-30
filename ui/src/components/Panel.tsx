import type { HTMLAttributes, ReactNode } from 'react';
import { cn } from './cn';

export interface PanelProps extends HTMLAttributes<HTMLDivElement> {
  /** Removes the inner padding, for a panel whose body is a table or a log viewer. */
  flush?: boolean;
}

/** The card every screen is built from: one surface, one border, one radius. */
export function Panel({ flush = false, className, children, ...props }: PanelProps) {
  return (
    <div
      {...props}
      className={cn(
        'rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)]',
        flush ? 'overflow-hidden' : 'p-4',
        className,
      )}
    >
      {children}
    </div>
  );
}

export interface PanelHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  /** Buttons, a filter, a menu — right-aligned on the same baseline as the title. */
  actions?: ReactNode;
  /**
   * The heading level this title occupies in the document outline. `2` (the default) is a panel
   * inside a screen; `1` is the screen's own title, which every route owes exactly one of — a
   * page whose outline starts at h2 leaves a screen-reader user with nothing above it. Level 1
   * also takes the page-title type scale the rest of the app uses, so a screen headed by a
   * `PanelHeader` reads the same as one headed by a literal `<h1>`.
   */
  level?: 1 | 2;
  className?: string;
}

export function PanelHeader({
  title,
  description,
  actions,
  level = 2,
  className,
}: PanelHeaderProps) {
  const Heading = level === 1 ? 'h1' : 'h2';
  return (
    <div className={cn('flex items-start justify-between gap-4', className)}>
      <div className="min-w-0">
        <Heading
          className={cn(
            'truncate font-semibold text-[var(--lm-text)]',
            level === 1 ? 'text-lg tracking-tight' : 'text-sm',
          )}
        >
          {title}
        </Heading>
        {description ? (
          <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">{description}</p>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  );
}

/** A labeled value in a definition grid — the shape every detail header uses. */
export function Field({
  label,
  children,
  mono = false,
  className,
}: {
  label: ReactNode;
  children: ReactNode;
  /** Technical values get JetBrains Mono: ports, hashes, paths, argv, flags. */
  mono?: boolean;
  className?: string;
}) {
  return (
    <div className={cn('min-w-0', className)}>
      <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">{label}</dt>
      <dd
        className={cn(
          'mt-0.5 truncate text-sm text-[var(--lm-text)]',
          mono && 'lm-numeric text-[13px]',
        )}
      >
        {children}
      </dd>
    </div>
  );
}

/** Inline monospace for one technical value inside prose or a table cell. */
export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn('lm-numeric text-[13px]', className)}>{children}</span>;
}
