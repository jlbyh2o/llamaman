/**
 * The release feed (DESIGN section 4 screen 15 "Updates" group, section 3.14 `GET /update/releases`).
 *
 * Changelog HTML arrives rendered and sanitized server-side (D35, DESIGN section 4: "no client-side
 * markdown renderer... dropping react-markdown + rehype-sanitize and moving attacker-controlled
 * content behind one audited sanitizer"), so it is the one place in this screen that uses
 * `dangerouslySetInnerHTML` — deliberately, and only on this field.
 */

import { ArrowDownCircle, ArrowUpCircle, CheckCircle2 } from 'lucide-react';
import { Badge, Button } from '../../components';
import type { UpdateRelease } from '../../api/types';
import { formatTimestamp } from '../../format';

export interface ReleaseFeedProps {
  releases: readonly UpdateRelease[];
  onApply: (tag: string) => void;
  applyingTag: string | null;
  disabled: boolean;
}

export function ReleaseFeed({ releases, onApply, applyingTag, disabled }: ReleaseFeedProps) {
  return (
    <ul className="divide-y divide-[var(--lm-border)]">
      {releases.map((release) => (
        <li key={release.tag} className="py-3 first:pt-0 last:pb-0">
          <details>
            <summary className="flex cursor-pointer list-none items-center justify-between gap-3">
              <div className="flex min-w-0 items-center gap-2">
                <span className="lm-numeric truncate text-sm font-medium text-[var(--lm-text)]">
                  {release.name || release.tag}
                </span>
                {release.same ? (
                  <Badge tone="ok" icon={<CheckCircle2 />}>
                    Running
                  </Badge>
                ) : release.newer ? (
                  <Badge tone="accent" icon={<ArrowUpCircle />}>
                    Newer
                  </Badge>
                ) : release.older ? (
                  <Badge tone="neutral" icon={<ArrowDownCircle />}>
                    Older
                  </Badge>
                ) : null}
                {release.published_at ? (
                  <span className="hidden shrink-0 text-xs text-[var(--lm-text-faint)] sm:inline">
                    {formatTimestamp(release.published_at)}
                  </span>
                ) : null}
              </div>
              <Button
                size="sm"
                variant={release.newer ? 'primary' : 'secondary'}
                disabled={disabled || release.same || !release.has_asset}
                loading={applyingTag === release.tag}
                onClick={(event) => {
                  event.preventDefault();
                  onApply(release.tag);
                }}
              >
                {release.same ? 'Current' : release.older ? 'Downgrade' : 'Update'}
              </Button>
            </summary>
            <div
              className="prose prose-sm mt-2 max-w-none text-[var(--lm-text-muted)]"
              // eslint-disable-next-line react/no-danger -- server-sanitized (D35), see module docstring
              dangerouslySetInnerHTML={{ __html: release.body_html }}
            />
            {!release.has_asset ? (
              <p className="mt-2 text-xs text-[var(--lm-text-faint)]">
                No release asset for this platform.
              </p>
            ) : null}
          </details>
        </li>
      ))}
    </ul>
  );
}
