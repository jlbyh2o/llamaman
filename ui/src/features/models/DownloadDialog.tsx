/**
 * Launching a download.
 *
 * The request is `{repo_id, revision, files, include_mmproj, mmproj_file, priority}` and the
 * response is a receipt: `202 {job_id, subject, model_id, download_id, bytes_total}` written in one
 * transaction with the `downloads`, `models` and `model_files` rows (section 3.8, section 2.7). So
 * the dialog closes into the queue rather than waiting for anything, and there is no spinner here
 * that could outlive the click.
 *
 * The projector is its own decision, because it is its own `models` row (section 7.3): reusable
 * across every quantization of the repository, which is exactly why the daemon does not fold it
 * into the quant the user picked.
 *
 * Three refusals get a real answer rather than a toast:
 *
 *  - `409 download_exists` — the transfer is already running; the dialog links to it.
 *  - `409 insufficient_disk` — the numbers are in the envelope, and the numbers are the message.
 *  - `403 hf_gated` — access grants are browser-only on the Hub's side, so the only useful control
 *    is a link out to the repository page.
 */

import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { Download, ExternalLink } from 'lucide-react';

import { Button, Dialog, DialogContent, Select, Switch, toast } from '../../components';
import { cn } from '../../components/cn';
import type { HFTreeGroup } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { downloadConflict, useCreateDownload } from './downloads';
import { gatedFrom, hubUrl } from './hf';
import { primaryFileOf } from './QuantTable';

export interface DownloadDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  repoId: string;
  revision?: string | undefined;
  /** The quantization the user picked, or null while the dialog is closed. */
  group: HFTreeGroup | null;
  /** Every projector the repository offers (`tree.mmproj`). */
  mmprojGroups: readonly HFTreeGroup[];
}

export function DownloadDialog({
  open,
  onOpenChange,
  repoId,
  revision,
  group,
  mmprojGroups,
}: DownloadDialogProps) {
  const create = useCreateDownload();
  const [includeMmproj, setIncludeMmproj] = useState(true);
  const [mmprojFile, setMmprojFile] = useState<string>('');
  const [priority, setPriority] = useState('100');

  const hasProjectors = mmprojGroups.length > 0;

  // Reopening the dialog — for a different quant, or after a refusal — starts from a clean answer.
  // The dependency list is deliberately the *identity of the thing being downloaded* rather than
  // everything the body reads: `create` is a stable mutation object whose `error` changing is
  // exactly what this must not react to, or a failed attempt would erase its own message.
  useEffect(() => {
    if (!open) return;
    create.reset();
    setIncludeMmproj(hasProjectors);
    setMmprojFile(mmprojGroups.length === 1 ? primaryFileOf(mmprojGroups[0]!) : '');
    setPriority('100');
  }, [open, repoId, group?.key]);

  if (!group) return null;

  const files = [...group.files].map((file) => file.path).sort();
  const projector = mmprojGroups.find((candidate) => primaryFileOf(candidate) === mmprojFile);
  const projectorBytes = includeMmproj ? (projector?.total_bytes ?? 0) : 0;
  const conflict = downloadConflict(create.error);
  const gated = gatedFrom(create.error);
  const ambiguous = includeMmproj && mmprojGroups.length > 1 && mmprojFile === '';

  const submit = () => {
    create.mutate(
      {
        repo_id: repoId,
        files,
        include_mmproj: includeMmproj,
        ...(revision === undefined ? {} : { revision }),
        ...(includeMmproj && mmprojFile !== '' ? { mmproj_file: mmprojFile } : {}),
        priority: Number(priority),
      },
      {
        onSuccess: () => {
          toast.success(`Queued ${group.quant_label || group.key}`, {
            description: `${formatBytes(group.total_bytes + projectorBytes)} from ${repoId}`,
          });
          onOpenChange(false);
        },
      },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="md"
        title={`Download ${group.quant_label || group.key}`}
        description={repoId}
        footer={
          <>
            <Button variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              icon={<Download />}
              loading={create.isPending}
              disabled={ambiguous}
              onClick={submit}
            >
              Start download
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <dl className="grid grid-cols-2 gap-3">
            <div>
              <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                Files
              </dt>
              <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                {formatCount(files.length)}
                {group.shard_total > 1 ? ` (${formatCount(group.shard_total)} shards)` : ''}
              </dd>
            </div>
            <div>
              <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                Total size
              </dt>
              <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                {formatBytes(group.total_bytes + projectorBytes)}
              </dd>
            </div>
          </dl>

          <ul className="lm-numeric max-h-32 space-y-0.5 overflow-y-auto rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-2 text-[12px] text-[var(--lm-text-muted)]">
            {files.map((file) => (
              <li key={file} className="truncate">
                {file}
              </li>
            ))}
          </ul>

          {hasProjectors ? (
            <div className="space-y-2 rounded-[var(--lm-radius)] border border-[var(--lm-border)] p-3">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <p className="text-sm font-medium text-[var(--lm-text)]">
                    Include the multimodal projector
                  </p>
                  <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">
                    Downloaded as its own model, so every quantization of this repository can share
                    it.
                  </p>
                </div>
                <Switch
                  checked={includeMmproj}
                  onCheckedChange={setIncludeMmproj}
                  aria-label="Include the multimodal projector"
                />
              </div>

              {includeMmproj && mmprojGroups.length > 1 ? (
                <Select
                  mono
                  value={mmprojFile === '' ? undefined : mmprojFile}
                  onValueChange={setMmprojFile}
                  placeholder="Choose a projector…"
                  aria-label="Projector"
                  options={mmprojGroups.map((candidate) => ({
                    value: primaryFileOf(candidate),
                    label: candidate.quant_label || primaryFileOf(candidate),
                    description: formatBytes(candidate.total_bytes),
                  }))}
                />
              ) : null}

              {ambiguous ? (
                <p className="text-xs text-[var(--lm-warn)]">
                  This repository offers several projectors, so one has to be chosen.
                </p>
              ) : null}
            </div>
          ) : null}

          <label className="block">
            <span className="text-xs font-medium text-[var(--lm-text)]">Queue priority</span>
            <Select
              className="mt-1"
              value={priority}
              onValueChange={setPriority}
              aria-label="Queue priority"
              options={[
                { value: '10', label: 'High', description: 'Ahead of everything already queued' },
                { value: '100', label: 'Normal', description: 'The default' },
                { value: '900', label: 'Low', description: 'After everything else' },
              ]}
            />
          </label>

          {gated ? (
            <Callout tone="warn">
              <p>
                This repository is gated. Access is granted on the Hub itself, in a browser — this
                daemon cannot request it for you.
              </p>
              <a
                href={gated.requestUrl || hubUrl(repoId)}
                target="_blank"
                rel="noreferrer noopener"
                className="mt-1 inline-flex items-center gap-1.5 text-[var(--lm-accent)] underline-offset-4 hover:underline"
              >
                Request access on Hugging Face
                <ExternalLink aria-hidden className="size-3.5" />
              </a>
            </Callout>
          ) : conflict?.kind === 'exists' ? (
            <Callout tone="warn">
              <p>This model is already in the queue.</p>
              <Link
                to="/downloads"
                onClick={() => onOpenChange(false)}
                className="mt-1 inline-block text-[var(--lm-accent)] underline-offset-4 hover:underline"
              >
                Open the download queue
              </Link>
            </Callout>
          ) : conflict?.kind === 'disk' ? (
            <Callout tone="danger">
              <p>
                Not enough space on the cache filesystem: {formatBytes(conflict.needed)} needed,{' '}
                {formatBytes(conflict.free)} free.
              </p>
            </Callout>
          ) : create.error ? (
            <Callout tone="danger">
              <p>{(create.error as Error).message}</p>
            </Callout>
          ) : null}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function Callout({ tone, children }: { tone: 'warn' | 'danger'; children: ReactNode }) {
  return (
    <div
      role="alert"
      className={cn(
        'rounded-[var(--lm-radius)] border px-3 py-2 text-xs text-[var(--lm-text)]',
        tone === 'warn'
          ? 'border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)]'
          : 'border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)]',
      )}
    >
      {children}
    </div>
  );
}
