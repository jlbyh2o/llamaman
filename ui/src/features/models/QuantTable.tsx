/**
 * The quantization table — DESIGN section 4, screen 9.
 *
 * "A quant table with true sizes and a fit verdict per quant per GPU, shard groups collapsed,
 * mmproj auto-paired, and the download button." Each row is one *downloadable unit* as section 7.3
 * defines it: a single GGUF or an entire shard set, sized from `lfs.size` rather than from the
 * ~130-byte LFS pointer the Hub's file listing reports by default.
 *
 * A row expands rather than links, because the thing a person wants after seeing "won't run" is the
 * per-device breakdown that explains it — and that is the same report the row's badge came from,
 * already fetched. Expanding is free; navigating would not be.
 */

import type { ReactNode } from 'react';
import { ChevronRight, Download, HardDrive, Layers } from 'lucide-react';
import { Link } from '@tanstack/react-router';

import { Badge, Button, EmptyState, Skeleton } from '../../components';
import { cn } from '../../components/cn';
import type { FitReport, HFTreeGroup } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { FitReportDetail, FitVerdict } from './FitVerdict';

/** The file that carries a group's geometry: shard 1, or the only file. */
export function primaryFileOf(group: HFTreeGroup): string {
  const paths = group.files.map((file) => file.path).sort();
  return paths[0] ?? '';
}

export interface QuantTableProps {
  groups: readonly HFTreeGroup[];
  /** Reports keyed by the group's primary file, as the batch endpoint returns them. */
  reports: ReadonlyMap<string, FitReport>;
  /** `recommended_file` — the largest quantization that still fits (section 3.9). */
  recommendedFile: string;
  /** The row whose report is expanded; mirrored into the URL by the screen. */
  openFile?: string | undefined;
  onOpenChange: (file: string | undefined) => void;
  onDownload: (group: HFTreeGroup) => void;
  fitLoading?: boolean;
  fitError?: string | undefined;
  className?: string;
}

export function QuantTable({
  groups,
  reports,
  recommendedFile,
  openFile,
  onOpenChange,
  onDownload,
  fitLoading = false,
  fitError,
  className,
}: QuantTableProps) {
  if (groups.length === 0) {
    return (
      <EmptyState
        dense
        icon={<Layers />}
        title="No GGUF quantizations here"
        description="This repository advertises GGUF but its file tree holds none at this revision."
      />
    );
  }

  return (
    <div className={cn('w-full overflow-x-auto', className)}>
      <table className="w-full border-collapse text-sm">
        <caption className="sr-only">
          Quantizations, with true file sizes and a fit verdict for each
        </caption>
        <thead>
          <tr className="border-b border-[var(--lm-border)]">
            <th scope="col" className="w-8" />
            <Th>Quantization</Th>
            <Th align="right">Size</Th>
            <Th align="right" secondary>
              Files
            </Th>
            <Th>Fit</Th>
            <th scope="col" className="w-px px-3" />
          </tr>
        </thead>
        {groups.map((group) => (
          <QuantRow
            key={group.key}
            group={group}
            report={reports.get(primaryFileOf(group))}
            recommended={primaryFileOf(group) === recommendedFile && recommendedFile !== ''}
            open={openFile === primaryFileOf(group)}
            onToggle={() =>
              onOpenChange(openFile === primaryFileOf(group) ? undefined : primaryFileOf(group))
            }
            onDownload={() => onDownload(group)}
            fitLoading={fitLoading}
            {...(fitError === undefined ? {} : { fitError })}
          />
        ))}
      </table>
    </div>
  );
}

function Th({
  children,
  align = 'left',
  secondary = false,
}: {
  children?: ReactNode;
  align?: 'left' | 'right';
  secondary?: boolean;
}) {
  return (
    <th
      scope="col"
      className={cn(
        'px-3 py-2 text-[11px] font-medium tracking-wide text-[var(--lm-text-faint)] uppercase',
        align === 'right' ? 'text-right' : 'text-left',
        secondary && 'hidden md:table-cell',
      )}
    >
      {children}
    </th>
  );
}

interface QuantRowProps {
  group: HFTreeGroup;
  report: FitReport | undefined;
  recommended: boolean;
  open: boolean;
  onToggle: () => void;
  onDownload: () => void;
  fitLoading: boolean;
  fitError?: string;
}

function QuantRow({
  group,
  report,
  recommended,
  open,
  onToggle,
  onDownload,
  fitLoading,
  fitError,
}: QuantRowProps) {
  const local = group.local_model_id ?? null;
  const sharded = group.shard_total > 1;

  return (
    <tbody className="border-b border-[var(--lm-border)] last:border-b-0">
      <tr
        className={cn(
          'align-middle',
          recommended && 'bg-[var(--lm-accent-soft)]',
          !open && 'hover:bg-[var(--lm-neutral-soft)]',
        )}
      >
        <td className="pl-2">
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={open}
            aria-label={open ? `Collapse ${group.quant_label}` : `Expand ${group.quant_label}`}
            className="rounded-[var(--lm-radius-sm)] p-1 text-[var(--lm-text-faint)] hover:text-[var(--lm-text)]"
          >
            <ChevronRight
              aria-hidden
              className={cn(
                'size-4 transition-transform duration-[var(--lm-duration-fast)]',
                open && 'rotate-90',
              )}
            />
          </button>
        </td>

        <td className="px-3 py-2.5">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="lm-numeric text-[13px] font-medium text-[var(--lm-text)]">
              {group.quant_label || group.key}
            </span>
            {sharded ? <Badge tone="neutral">{formatCount(group.shard_total)} shards</Badge> : null}
            {!group.complete ? (
              <Badge tone="warn" title="This repository holds only part of the shard set.">
                Incomplete set
              </Badge>
            ) : null}
            {local ? (
              <Badge tone="ok" icon={<HardDrive />}>
                On disk
              </Badge>
            ) : null}
          </div>
        </td>

        <td className="lm-numeric px-3 py-2.5 text-right text-[13px] text-[var(--lm-text)]">
          {formatBytes(group.total_bytes)}
        </td>

        <td className="lm-numeric hidden px-3 py-2.5 text-right text-[13px] text-[var(--lm-text-muted)] md:table-cell">
          {formatCount(group.files.length)}
        </td>

        <td className="px-3 py-2.5">
          {report ? (
            <FitVerdict report={report} recommended={recommended} />
          ) : fitError ? (
            <span className="text-xs text-[var(--lm-text-muted)]">{fitError}</span>
          ) : fitLoading ? (
            <Skeleton className="w-28" />
          ) : (
            <span className="text-xs text-[var(--lm-text-faint)]">—</span>
          )}
        </td>

        <td className="px-3 py-2.5 text-right whitespace-nowrap">
          {local ? (
            <Link
              to="/models/$id"
              params={{ id: local }}
              className="text-sm text-[var(--lm-accent)] underline-offset-4 hover:underline"
            >
              Open
            </Link>
          ) : (
            <Button
              size="sm"
              variant={recommended ? 'primary' : 'secondary'}
              icon={<Download />}
              onClick={onDownload}
              disabled={!group.complete}
              title={
                group.complete
                  ? undefined
                  : 'The repository is missing shards this set names; the transfer could never finish.'
              }
            >
              Download
            </Button>
          )}
        </td>
      </tr>

      {open ? (
        <tr>
          <td colSpan={6} className="px-3 pb-4">
            <div className="space-y-4 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3">
              {report ? (
                <FitReportDetail report={report} />
              ) : (
                <p className="text-xs text-[var(--lm-text-muted)]">
                  {fitError ?? 'No estimate for this quantization yet.'}
                </p>
              )}

              <div>
                <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Files
                </p>
                <ul className="mt-1 space-y-0.5">
                  {[...group.files]
                    .sort((a, b) => a.path.localeCompare(b.path))
                    .map((file) => (
                      <li
                        key={file.path}
                        className="lm-numeric flex items-baseline justify-between gap-4 text-[12px]"
                      >
                        <span className="truncate text-[var(--lm-text-muted)]">{file.path}</span>
                        <span className="shrink-0 text-[var(--lm-text-faint)]">
                          {formatBytes(file.size_bytes)}
                        </span>
                      </li>
                    ))}
                </ul>
              </div>
            </div>
          </td>
        </tr>
      ) : null}
    </tbody>
  );
}
