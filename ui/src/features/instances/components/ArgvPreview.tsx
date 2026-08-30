/**
 * The rendered command line.
 *
 * "`GET /instances/{id}/command` returns this argv and env verbatim, so what the UI shows is what
 * runs" (DESIGN section 5.7). Under the form it is the dry run of `POST /instances/validate`; on the
 * detail screen it is the command endpoint. Either way the rule is the same: show the daemon's
 * rendering, never a client-side reconstruction of it — `RenderArgv` is one pure function in one
 * package for exactly that reason (D49), and a second implementation in TypeScript would be a
 * second source of truth that could disagree with what actually launches.
 */

import { useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { Badge, Button, Panel, PanelHeader } from '../../../components';

export interface ArgvPreviewProps {
  argv?: readonly string[] | undefined;
  env?: Record<string, string> | undefined;
  unit?: string | undefined;
  unknownFlags?: readonly string[];
  loading?: boolean;
  /** Why there is nothing to show: no model resolved yet, or the route is not served. */
  unavailable?: string | undefined;
  title?: string;
}

/** One flag and its value per line, which is how a long command line stays readable. */
export function formatArgv(argv: readonly string[]): string {
  const lines: string[] = [];
  for (let i = 0; i < argv.length; i += 1) {
    const word = argv[i] as string;
    if (i === 0) {
      lines.push(word);
      continue;
    }
    const next = argv[i + 1];
    if (word.startsWith('-') && next !== undefined && !next.startsWith('-')) {
      lines.push(`${word} ${next}`);
      i += 1;
    } else {
      lines.push(word);
    }
  }
  return lines.join(' \\\n  ');
}

export function ArgvPreview({
  argv,
  env,
  unit,
  unknownFlags = [],
  loading = false,
  unavailable,
  title = 'Command line',
}: ArgvPreviewProps) {
  const [copied, setCopied] = useState(false);
  const text = argv && argv.length > 0 ? formatArgv(argv) : '';

  const copy = () => {
    void navigator.clipboard?.writeText(argv ? argv.join(' ') : '');
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Panel flush>
      <div className="px-4 pt-3">
        <PanelHeader
          title={title}
          {...(unit === undefined ? {} : { description: unit })}
          actions={
            text ? (
              <Button
                size="sm"
                variant="ghost"
                icon={copied ? <Check /> : <Copy />}
                onClick={copy}
                aria-label="Copy the command line"
              >
                {copied ? 'Copied' : 'Copy'}
              </Button>
            ) : null
          }
        />
      </div>

      {unknownFlags.length > 0 ? (
        <div className="flex flex-wrap gap-1 px-4 pt-2">
          {unknownFlags.map((flag) => (
            <Badge key={flag} tone="warn" title="This build's --help does not advertise this flag">
              {flag}
            </Badge>
          ))}
        </div>
      ) : null}

      <pre className="lm-numeric mt-3 max-h-72 overflow-auto border-t border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] px-4 py-3 text-[12px] leading-relaxed text-[var(--lm-text-muted)]">
        {text ||
          (loading
            ? 'Rendering…'
            : (unavailable ??
              'No command line yet — the daemon renders it once a model and an active llama.cpp build are resolved.'))}
      </pre>

      {env && Object.keys(env).length > 0 ? (
        <dl className="grid gap-x-4 gap-y-1 border-t border-[var(--lm-border)] px-4 py-3 sm:grid-cols-[auto_1fr]">
          {Object.entries(env).map(([key, value]) => (
            <div key={key} className="contents">
              <dt className="lm-numeric text-[12px] text-[var(--lm-text-faint)]">{key}</dt>
              <dd className="lm-numeric truncate text-[12px] text-[var(--lm-text-muted)]">
                {value}
              </dd>
            </div>
          ))}
        </dl>
      ) : null}
    </Panel>
  );
}
