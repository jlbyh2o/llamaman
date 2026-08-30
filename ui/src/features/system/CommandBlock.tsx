/**
 * A remediation command, or a numbered procedure of them.
 *
 * Every degraded-mode answer in section 3.3/3.4/3.14 carries an exact shell command rather than a
 * description of one — `sudo systemctl restart llamaman.service`, `sudo llamaman install-units
 * --repair-polkit`, the five-command downgrade of section 12.4 — because a host in one of these
 * states is being operated by someone at a terminal, not clicking through a wizard. This is the one
 * place that command gets rendered, so copy-to-clipboard and the monospace treatment are decided
 * once for the System, Settings and Update screens alike.
 */

import { useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { Button } from '../../components/Button';
import { cn } from '../../components/cn';

export function CommandLine({ command, className }: { command: string; className?: string }) {
  const [copied, setCopied] = useState(false);

  const copy = () => {
    void navigator.clipboard?.writeText(command).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <div
      className={cn(
        'flex items-center gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-border)]',
        'bg-[var(--lm-surface-sunken)] py-1.5 pr-1.5 pl-3',
        className,
      )}
    >
      <code className="lm-numeric flex-1 overflow-x-auto text-[12.5px] whitespace-pre text-[var(--lm-text)]">
        {command}
      </code>
      <Button
        size="sm"
        variant="ghost"
        icon={copied ? <Check /> : <Copy />}
        onClick={copy}
        aria-label="Copy command"
        className={copied ? 'text-[var(--lm-ok)]' : undefined}
      >
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  );
}

/** A numbered procedure — section 12.4's five-command downgrade, in order, never a shortcut. */
export function CommandProcedure({
  commands,
  className,
}: {
  commands: readonly string[];
  className?: string;
}) {
  return (
    <ol className={cn('space-y-2', className)}>
      {commands.map((command, index) => (
        <li key={index} className="flex items-start gap-2">
          <span
            aria-hidden
            className="mt-1.5 flex size-4 shrink-0 items-center justify-center rounded-full bg-[var(--lm-neutral-soft)] text-[10px] font-medium text-[var(--lm-text-muted)]"
          >
            {index + 1}
          </span>
          <CommandLine command={command} className="flex-1" />
        </li>
      ))}
    </ol>
  );
}
