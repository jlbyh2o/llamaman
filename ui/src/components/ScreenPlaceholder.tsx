import type { ReactNode } from 'react';
import { Construction } from 'lucide-react';
import { Panel } from './Panel';
import { cn } from './cn';

/**
 * A screen that has not been built yet.
 *
 * Every leaf in DESIGN section 4's screen list has a route and a file from day one, so the router
 * is complete and the areas can be built in parallel without touching each other. Each of those
 * files renders this until its own agent replaces the body — and it names what belongs there, so
 * the placeholder is also the brief.
 *
 * `design` quotes the screen's line from section 4 verbatim; `endpoints` lists the section 3 routes
 * it will read.
 */

export interface ScreenPlaceholderProps {
  title: string;
  /** The screen's own line from DESIGN section 4. */
  design: ReactNode;
  /** API paths this screen is built on. */
  endpoints?: readonly string[];
  /** SSE topics that keep it fresh. */
  topics?: readonly string[];
  className?: string;
}

export function ScreenPlaceholder({
  title,
  design,
  endpoints,
  topics,
  className,
}: ScreenPlaceholderProps) {
  return (
    <div className={cn('space-y-4', className)}>
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">{title}</h1>
      </div>

      <Panel>
        <div className="flex items-start gap-3">
          <span
            aria-hidden
            className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-[var(--lm-radius)] bg-[var(--lm-neutral-soft)] text-[var(--lm-text-faint)]"
          >
            <Construction className="size-4" />
          </span>
          <div className="min-w-0 space-y-3">
            <p className="text-sm text-[var(--lm-text-muted)]">{design}</p>

            {endpoints?.length ? (
              <div>
                <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Endpoints
                </p>
                <ul className="mt-1 flex flex-wrap gap-1.5">
                  {endpoints.map((endpoint) => (
                    <li
                      key={endpoint}
                      className="lm-numeric rounded-[var(--lm-radius-sm)] bg-[var(--lm-surface-sunken)] px-1.5 py-0.5 text-[12px] text-[var(--lm-text-muted)]"
                    >
                      {endpoint}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}

            {topics?.length ? (
              <div>
                <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Live topics
                </p>
                <ul className="mt-1 flex flex-wrap gap-1.5">
                  {topics.map((topic) => (
                    <li
                      key={topic}
                      className="lm-numeric rounded-[var(--lm-radius-sm)] bg-[var(--lm-accent-soft)] px-1.5 py-0.5 text-[12px] text-[var(--lm-accent)]"
                    >
                      {topic}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        </div>
      </Panel>
    </div>
  );
}
