/**
 * `/bench/$id` — one run: progress while it executes, results once it has them, export always
 * (DESIGN section 4, screen 13).
 *
 * `useBenchRun` and `useBenchRunResults` both live in the `bench` SSE family, so a running sweep's
 * `progress` and a just-finished point's row both arrive as cache patches rather than a poll.
 */

import { useEffect, useState } from 'react';
import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import {
  AlertTriangle,
  Ban,
  Download,
  FileJson,
  FileSpreadsheet,
  FileText,
  Trash2,
} from 'lucide-react';
import type { Column } from '../../components';
import {
  Button,
  ConfirmDialog,
  DataTable,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
  Field,
  LoadingPanel,
  Mono,
  Panel,
  PanelHeader,
  Progress,
  StatusBadge,
  Textarea,
  describeError,
  toast,
} from '../../components';
import {
  formatCount,
  formatDuration,
  formatRelative,
  formatTimestamp,
  formatWithStddev,
} from '../../format';
import type { BenchExportFormat } from '../../features/bench/export';
import { downloadBenchExport } from '../../features/bench/export';
import {
  useBenchRun,
  useBenchRunResults,
  useCancelBenchRun,
  useDeleteBenchRun,
  usePatchBenchRun,
} from '../../features/bench/hooks';
import type { BenchResultRow } from '../../api/types';

const LIVE_STATES = new Set(['queued', 'preflight', 'running']);

export function BenchRunScreen() {
  const { id } = useParams({ from: '/app/bench/$id' });
  useSearch({ from: '/app/bench/$id' });
  const navigate = useNavigate({ from: '/bench/$id' });

  const run = useBenchRun(id);
  const results = useBenchRunResults(id);
  const cancel = useCancelBenchRun();
  const del = useDeleteBenchRun();
  const patch = usePatchBenchRun(id);

  const [notes, setNotes] = useState('');
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [cancelOpen, setCancelOpen] = useState(false);
  const [exporting, setExporting] = useState<BenchExportFormat | null>(null);

  useEffect(() => {
    setNotes(run.data?.run.notes ?? '');
  }, [run.data?.run.notes]);

  if (run.isLoading) return <LoadingPanel />;
  if (run.isError || !run.data) {
    return (
      <div className="p-6">
        <Panel className="text-sm text-[var(--lm-danger)]">
          {run.error ? describeError(run.error).title : 'This run could not be found.'}
        </Panel>
      </div>
    );
  }

  const { run: summary, points, progress } = run.data;
  const live = LIVE_STATES.has(summary.state);
  const cancelable = live && run.data.job_id;
  const deletable = summary.state !== 'running' && summary.state !== 'preflight';

  async function handleExport(format: BenchExportFormat) {
    setExporting(format);
    try {
      await downloadBenchExport(id, format, summary.name);
    } catch (err) {
      const { title, description } = describeError(err);
      toast.error(title, { description });
    } finally {
      setExporting(null);
    }
  }

  const columns: Column<BenchResultRow>[] = [
    { id: 'ordinal', header: '#', width: '3rem', mono: true, cell: (r) => r.ordinal },
    { id: 'test', header: 'Test', cell: (r) => r.test_kind },
    {
      id: 'ngl',
      header: 'ngl',
      mono: true,
      align: 'right',
      cell: (r) => (r.n_gpu_layers === null || r.n_gpu_layers === undefined ? '—' : r.n_gpu_layers),
    },
    { id: 'batch', header: 'batch', mono: true, align: 'right', cell: (r) => r.n_batch ?? '—' },
    {
      id: 'ubatch',
      header: 'ubatch',
      mono: true,
      align: 'right',
      cell: (r) => r.n_ubatch ?? '—',
      secondary: true,
    },
    {
      id: 'fa',
      header: 'fa',
      mono: true,
      align: 'right',
      secondary: true,
      cell: (r) =>
        r.flash_attn === null || r.flash_attn === undefined ? '—' : r.flash_attn ? '1' : '0',
    },
    { id: 'ctk', header: 'ctk', mono: true, cell: (r) => r.type_k ?? '—', secondary: true },
    { id: 'ctv', header: 'ctv', mono: true, cell: (r) => r.type_v ?? '—', secondary: true },
    {
      id: 'prompt',
      header: 'pp / tg / depth',
      mono: true,
      align: 'right',
      cell: (r) => `${r.n_prompt}/${r.n_gen}${r.n_depth ? `/${r.n_depth}` : ''}`,
    },
    {
      id: 'ts',
      header: 't/s',
      mono: true,
      align: 'right',
      sortValue: (r) => r.avg_ts,
      cell: (r) => formatWithStddev(r.avg_ts, r.stddev_ts),
    },
    {
      id: 'samples',
      header: 'n',
      mono: true,
      align: 'right',
      secondary: true,
      cell: (r) => formatCount(r.samples),
    },
  ];

  return (
    <div className="space-y-6 p-6">
      <PanelHeader
        level={1}
        title={
          <span className="flex items-center gap-2">
            {summary.name}
            <StatusBadge kind="bench" state={summary.state} />
          </span>
        }
        description={`${summary.model_label}${summary.quant_label ? ` · ${summary.quant_label}` : ''} — ${summary.llamacpp_tag}${summary.llamacpp_commit ? ` (${summary.llamacpp_commit.slice(0, 8)})` : ''}`}
        actions={
          <>
            {cancelable ? (
              <Button variant="secondary" icon={<Ban />} onClick={() => setCancelOpen(true)}>
                Cancel
              </Button>
            ) : null}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="secondary" icon={<Download />} loading={exporting !== null}>
                  Export
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent>
                <DropdownMenuItem onSelect={() => void handleExport('json')}>
                  <FileJson /> JSON
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void handleExport('csv')}>
                  <FileSpreadsheet /> CSV
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void handleExport('md')}>
                  <FileText /> Markdown
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <Button
              variant="danger"
              icon={<Trash2 />}
              disabled={!deletable}
              title={deletable ? undefined : 'Cannot delete a run that is still executing'}
              onClick={() => setDeleteOpen(true)}
            >
              Delete
            </Button>
          </>
        }
      />

      {live ? (
        <Panel>
          <Progress
            value={
              progress && progress.points_total > 0
                ? (progress.points_done / progress.points_total) * 100
                : null
            }
            label={progress?.current ?? 'Waiting for the bench lease…'}
            detail={progress ? `${progress.points_done} / ${progress.points_total}` : undefined}
          />
        </Panel>
      ) : null}

      {summary.error_message ? (
        <Panel className="flex items-start gap-2 border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)]">
          <AlertTriangle aria-hidden className="mt-0.5 size-4 shrink-0 text-[var(--lm-danger)]" />
          <p className="text-sm text-[var(--lm-text)]">{summary.error_message}</p>
        </Panel>
      ) : null}

      <Panel className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <Field label="Points">
          {formatCount(summary.points_done)} / {formatCount(summary.points_total)}
          {summary.points_failed > 0 ? ` (${summary.points_failed} failed)` : ''}
        </Field>
        <Field label="Repetitions">{summary.repetitions}</Field>
        <Field label="Created" mono>
          <span title={formatTimestamp(summary.created_at)}>
            {formatRelative(summary.created_at)}
          </span>
        </Field>
        <Field label="Duration">
          {summary.started_at && summary.finished_at
            ? formatDuration(
                new Date(summary.finished_at).getTime() - new Date(summary.started_at).getTime(),
              )
            : '—'}
        </Field>
        {summary.stopped_instances.length > 0 ? (
          <Field label="Stopped instances" className="col-span-2 sm:col-span-4">
            {summary.stopped_instances.join(', ')}
            {summary.restore_done ? ' — restored' : ' — not yet restored'}
          </Field>
        ) : null}
      </Panel>

      <Panel flush>
        <div className="border-b border-[var(--lm-border)] px-4 py-3">
          <h2 className="text-sm font-semibold text-[var(--lm-text)]">Results</h2>
        </div>
        <DataTable
          columns={columns}
          rows={results.data?.items ?? []}
          rowKey={(row) => row.point_id}
          loading={results.isLoading}
          caption="Benchmark results"
        />
      </Panel>

      {points.some((p) => p.state === 'failed') ? (
        <Panel className="space-y-2">
          <h2 className="text-sm font-semibold text-[var(--lm-text)]">Failed points</h2>
          <ul className="space-y-1 text-xs text-[var(--lm-text-muted)]">
            {points
              .filter((p) => p.state === 'failed')
              .map((p) => (
                <li key={p.id}>
                  <Mono>#{p.ordinal}</Mono> {p.label} — {p.error_message ?? 'no error message'}
                </li>
              ))}
          </ul>
        </Panel>
      ) : null}

      <Panel className="space-y-2">
        <h2 className="text-sm font-semibold text-[var(--lm-text)]">Notes</h2>
        <Textarea
          value={notes}
          onChange={(event) => setNotes(event.target.value)}
          onBlur={() => {
            if (notes !== (run.data?.run.notes ?? '')) {
              patch.mutate({ notes: notes || null });
            }
          }}
          placeholder="What was this run for?"
        />
      </Panel>

      <ConfirmDialog
        open={cancelOpen}
        onOpenChange={setCancelOpen}
        title="Cancel this benchmark?"
        description="The current point finishes, the rest are marked skipped, and any instances this run stopped are restarted."
        confirmLabel="Cancel run"
        busy={cancel.isPending}
        onConfirm={() => {
          cancel.mutate(id, {
            onSuccess: () => setCancelOpen(false),
            onError: (err) => toast.error(err),
          });
        }}
      />

      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title="Delete this benchmark run?"
        description="Its points and results are removed. This cannot be undone."
        confirmLabel="Delete"
        busy={del.isPending}
        onConfirm={() => {
          del.mutate(id, {
            onSuccess: () => void navigate({ to: '/bench' }),
            onError: (err) => toast.error(err),
          });
        }}
      />
    </div>
  );
}
