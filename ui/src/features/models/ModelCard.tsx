/**
 * The model card.
 *
 * D35 and DESIGN section 4 removed the client-side markdown renderer from this app on purpose:
 * "Model cards and changelogs arrive as sanitized HTML from the server, dropping `react-markdown` +
 * `rehype-sanitize` and moving attacker-controlled content behind one audited sanitizer." A model
 * card is a README written by whoever published the repository — untrusted text from the open
 * internet — and the only place it is ever sanitized is `GET /api/v1/hf/card/{repo}`.
 *
 * So `dangerouslySetInnerHTML` here is the *designed* path rather than a shortcut, and the two
 * things that make it safe are worth naming where the call is:
 *
 *  1. The HTML has already been through the daemon's sanitizer. This component must never be handed
 *     markup from anywhere else, and it takes an `HFCardDTO` rather than a string so that it cannot
 *     accidentally be.
 *  2. Everything is styled from the outside, through descendant selectors on a wrapper class. No
 *     class from the card's own markup is trusted to mean anything, and the card cannot reach the
 *     app's tokens to restyle the page around it.
 *
 * "View source" shows the raw markdown the same response carries — as text, in a `<pre>`, which is
 * the one rendering of untrusted content that is unambiguously inert.
 */

import { useState } from 'react';
import { Code2, ExternalLink, FileText } from 'lucide-react';

import { Button, EmptyState, Panel, PanelHeader } from '../../components';
import { cn } from '../../components/cn';
import type { HFCard } from '../../api/types';
import { hubUrl } from './hf';

/**
 * The card's typography, applied from outside its markup.
 *
 * Tailwind's typography plugin is not a dependency of this project (DESIGN section 14 fixes the
 * list), so the handful of element rules a README actually needs are written as descendant variants
 * — every color a token, every table and code block scrollable inside its own box so a wide card
 * cannot make the page scroll sideways.
 */
const CARD_PROSE = [
  'text-sm leading-relaxed text-[var(--lm-text-muted)]',
  '[&_h1]:mt-6 [&_h1]:mb-2 [&_h1]:text-base [&_h1]:font-semibold [&_h1]:text-[var(--lm-text)]',
  '[&_h2]:mt-6 [&_h2]:mb-2 [&_h2]:text-sm [&_h2]:font-semibold [&_h2]:text-[var(--lm-text)]',
  '[&_h3]:mt-4 [&_h3]:mb-1.5 [&_h3]:text-sm [&_h3]:font-medium [&_h3]:text-[var(--lm-text)]',
  '[&_h4]:mt-4 [&_h4]:mb-1.5 [&_h4]:text-sm [&_h4]:font-medium [&_h4]:text-[var(--lm-text)]',
  '[&>*:first-child]:mt-0',
  '[&_p]:my-2',
  '[&_a]:text-[var(--lm-accent)] [&_a]:underline-offset-4 hover:[&_a]:underline',
  '[&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-5',
  '[&_ol]:my-2 [&_ol]:list-decimal [&_ol]:pl-5',
  '[&_li]:my-0.5',
  '[&_strong]:font-semibold [&_strong]:text-[var(--lm-text)]',
  '[&_code]:lm-numeric [&_code]:rounded-[var(--lm-radius-sm)] [&_code]:bg-[var(--lm-surface-sunken)] [&_code]:px-1 [&_code]:py-0.5 [&_code]:text-[12px]',
  '[&_pre]:my-3 [&_pre]:overflow-x-auto [&_pre]:rounded-[var(--lm-radius)] [&_pre]:border [&_pre]:border-[var(--lm-border)] [&_pre]:bg-[var(--lm-surface-sunken)] [&_pre]:p-3',
  '[&_pre_code]:bg-transparent [&_pre_code]:p-0',
  '[&_blockquote]:my-3 [&_blockquote]:border-l-2 [&_blockquote]:border-[var(--lm-border-strong)] [&_blockquote]:pl-3',
  '[&_hr]:my-5 [&_hr]:border-[var(--lm-border)]',
  '[&_img]:max-w-full [&_img]:rounded-[var(--lm-radius)]',
  '[&_table]:my-3 [&_table]:block [&_table]:w-full [&_table]:overflow-x-auto [&_table]:border-collapse',
  '[&_th]:border [&_th]:border-[var(--lm-border)] [&_th]:px-2 [&_th]:py-1 [&_th]:text-left [&_th]:text-[var(--lm-text)]',
  '[&_td]:border [&_td]:border-[var(--lm-border)] [&_td]:px-2 [&_td]:py-1',
].join(' ');

export interface ModelCardProps {
  card: HFCard | undefined;
  repoId: string;
  loading?: boolean;
  /** Why the card is missing, when it is — a gated repo, or a repository with no README. */
  unavailable?: string | undefined;
  className?: string;
}

export function ModelCard({
  card,
  repoId,
  loading = false,
  unavailable,
  className,
}: ModelCardProps) {
  const [source, setSource] = useState(false);

  return (
    <Panel className={cn('space-y-3', className)}>
      <PanelHeader
        title="Model card"
        description="Rendered and sanitized by the daemon — this page runs no markdown renderer."
        actions={
          <>
            {card?.markdown ? (
              <Button
                size="sm"
                variant="ghost"
                icon={source ? <FileText /> : <Code2 />}
                onClick={() => setSource((open) => !open)}
              >
                {source ? 'Rendered' : 'Source'}
              </Button>
            ) : null}
            <a
              href={hubUrl(repoId)}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1.5 text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
            >
              On the Hub
              <ExternalLink aria-hidden className="size-3.5" />
            </a>
          </>
        }
      />

      {loading ? (
        <div className="space-y-2" aria-hidden>
          <span className="block h-3 w-2/3 animate-pulse rounded bg-[var(--lm-neutral-soft)]" />
          <span className="block h-3 w-full animate-pulse rounded bg-[var(--lm-neutral-soft)]" />
          <span className="block h-3 w-4/5 animate-pulse rounded bg-[var(--lm-neutral-soft)]" />
        </div>
      ) : unavailable ? (
        <EmptyState dense title="No model card" description={unavailable} />
      ) : !card?.html ? (
        <EmptyState
          dense
          title="No model card"
          description="This repository publishes no README at this revision."
        />
      ) : source ? (
        <pre className="lm-numeric max-h-[36rem] overflow-auto rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3 text-[12px] whitespace-pre-wrap text-[var(--lm-text-muted)]">
          {card.markdown}
        </pre>
      ) : (
        <div
          className={cn('max-h-[36rem] overflow-y-auto', CARD_PROSE)}
          // Safe by construction: this string is the daemon's sanitizer output (D35) and nothing
          // else may be passed here. See the module comment above.
          dangerouslySetInnerHTML={{ __html: card.html }}
        />
      )}
    </Panel>
  );
}
