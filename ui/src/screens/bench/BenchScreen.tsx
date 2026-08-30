/**
 * `/bench` — history table and charts (DESIGN section 4, screen 13).
 *
 * Filters, sort and the selection carried into "Compare" all live in the URL (`benchSearchSchema`),
 * which is what makes a filtered view of the benchmark history a link someone can send. The table
 * reads `GET /bench/runs`; the chart below it is a second, independent query against `GET
 * /bench/series` — a run only ever contributes points to a chart once it has results, so the two
 * views disagree in exactly the way section 3.13 intends: one is "what have I run", the other is
 * "how has it changed".
 */

import { useMemo, useState } from 'react';
import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { GitCompare, Plus } from 'lucide-react';
import type { Column } from '../../components';
import {
  Button,
  DataTable,
  EmptyState,
  Input,
  Panel,
  PanelHeader,
  QueryError,
  Select,
  StatusBadge,
} from '../../components';
import { formatCount, formatRelative, formatTimestamp, formatTokensPerSecond } from '../../format';
import { alignTimeSeries } from '../../features/bench/chartData';
import { UplotChart } from '../../features/bench/UplotChart';
import type { BenchSeriesGroup } from '../../features/bench/hooks';
import { useBenchRuns, useBenchSeries, useReadyModels } from '../../features/bench/hooks';
import type { BenchRun, BenchState } from '../../api/types';

const GROUP_OPTIONS: { value: BenchSeriesGroup; label: string }[] = [
  { value: 'llamacpp_tag', label: 'llama.cpp version' },
  { value: 'quant_label', label: 'Quantization' },
  { value: 'n_gpu_layers', label: 'GPU layers' },
  { value: 'run_name', label: 'Run' },
];

export function BenchScreen() {
  // `useSearch`'s `from` is the route's internal id — the pathless `app` shell layout route
  // contributes an `/app` prefix there even though it contributes no path segment to the URL;
  // `useNavigate`'s `from` is the plain URL path, same as `Link to=`.
  const search = useSearch({ from: '/app/bench' });
  const navigate = useNavigate({ from: '/bench' });
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const runs = useBenchRuns({
    ...(search.model_id ? { model_id: search.model_id } : {}),
    ...(search.state ? { state: search.state as BenchState } : {}),
  });

  const models = useReadyModels();
  const [test, setTest] = useState<'pp' | 'tg' | 'pp+tg'>('tg');
  const [group, setGroup] = useState<BenchSeriesGroup>('llamacpp_tag');

  const series = useBenchSeries({
    ...(search.model_id ? { model_id: search.model_id } : {}),
    test,
    metric: 'avg_ts',
    group,
  });

  const chart = useMemo(() => alignTimeSeries(series.data?.points ?? []), [series.data]);

  const rows = useMemo(() => {
    const items = runs.data?.items ?? [];
    const q = search.q?.toLowerCase();
    return q ? items.filter((r) => r.name.toLowerCase().includes(q)) : items;
  }, [runs.data, search.q]);

  function toggle(id: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  const columns: Column<BenchRun>[] = [
    {
      id: 'select',
      header: '',
      width: '2rem',
      cell: (row) => (
        <input
          type="checkbox"
          checked={selected.has(row.id)}
          onClick={(e) => e.stopPropagation()}
          onChange={() => toggle(row.id)}
          aria-label={`Select ${row.name} for comparison`}
        />
      ),
    },
    {
      id: 'name',
      header: 'Name',
      sortValue: (row) => row.name,
      cell: (row) => (
        <Link
          to="/bench/$id"
          params={{ id: row.id }}
          className="font-medium text-[var(--lm-text)] hover:text-[var(--lm-accent)]"
        >
          {row.name}
        </Link>
      ),
    },
    {
      id: 'model',
      header: 'Model',
      secondary: true,
      cell: (row) => (
        <span className="text-[var(--lm-text-muted)]">
          {row.model_label}
          {row.quant_label ? ` · ${row.quant_label}` : ''}
        </span>
      ),
    },
    {
      id: 'version',
      header: 'llama.cpp',
      secondary: true,
      mono: true,
      cell: (row) => row.llamacpp_tag,
    },
    {
      id: 'state',
      header: 'State',
      sortValue: (row) => row.state,
      cell: (row) => <StatusBadge kind="bench" state={row.state} />,
    },
    {
      id: 'points',
      header: 'Points',
      align: 'right',
      mono: true,
      cell: (row) => (
        <span>
          {formatCount(row.points_done)}/{formatCount(row.points_total)}
          {row.points_failed > 0 ? (
            <span className="ml-1 text-[var(--lm-danger)]">({row.points_failed} failed)</span>
          ) : null}
        </span>
      ),
    },
    {
      id: 'created',
      header: 'Created',
      sortValue: (row) => row.created_at,
      cell: (row) => (
        <span title={formatTimestamp(row.created_at)}>{formatRelative(row.created_at)}</span>
      ),
    },
  ];

  return (
    <div className="space-y-6 p-6">
      <PanelHeader
        level={1}
        title="Benchmarks"
        description="Sweep results are never auto-deleted — every run stays in history."
        actions={
          <>
            <Button
              variant="secondary"
              icon={<GitCompare />}
              disabled={selected.size < 1}
              onClick={() =>
                void navigate({
                  to: '/bench/compare',
                  search: { runs: [...selected], x: 'n_gpu_layers', y: 'avg_ts' },
                })
              }
            >
              Compare {selected.size > 0 ? `(${selected.size})` : ''}
            </Button>
            <Link to="/bench/new">
              <Button variant="primary" icon={<Plus />}>
                New benchmark
              </Button>
            </Link>
          </>
        }
      />

      <Panel className="flex flex-wrap items-center gap-3">
        <Input
          className="min-w-48 flex-1"
          placeholder="Search runs by name…"
          value={search.q ?? ''}
          onChange={(event) =>
            void navigate({ search: (prev) => ({ ...prev, q: event.target.value || undefined }) })
          }
        />
        <Select
          value={search.model_id}
          onValueChange={(value) =>
            void navigate({ search: (prev) => ({ ...prev, model_id: value || undefined }) })
          }
          placeholder="All models"
          className="w-56"
          options={(models.data?.items ?? []).map((m) => ({
            value: m.id,
            label: `${m.repo_id}${m.quant_label ? ` · ${m.quant_label}` : ''}`,
          }))}
        />
        <Select
          value={search.state}
          onValueChange={(value) =>
            void navigate({ search: (prev) => ({ ...prev, state: value || undefined }) })
          }
          placeholder="All states"
          className="w-40"
          options={[
            { value: 'draft', label: 'Draft' },
            { value: 'queued', label: 'Queued' },
            { value: 'running', label: 'Running' },
            { value: 'succeeded', label: 'Succeeded' },
            { value: 'partial', label: 'Partial' },
            { value: 'failed', label: 'Failed' },
            { value: 'canceled', label: 'Canceled' },
          ]}
        />
        {search.model_id || search.state || search.q ? (
          <Button variant="ghost" size="sm" onClick={() => void navigate({ search: {} })}>
            Clear filters
          </Button>
        ) : null}
      </Panel>

      <Panel flush>
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.id}
          loading={runs.isPending}
          empty={
            runs.isError ? (
              // A dropped read must never render "No benchmarks yet" over a real history.
              <QueryError
                title="The benchmark runs could not be read"
                error={runs.error}
                onRetry={() => void runs.refetch()}
                dense
              />
            ) : (
              <EmptyState
                title="No benchmarks yet"
                description="Run a sweep against a model to start building history."
                action={
                  <Link to="/bench/new">
                    <Button variant="primary" icon={<Plus />}>
                      New benchmark
                    </Button>
                  </Link>
                }
              />
            )
          }
        />
      </Panel>

      <Panel className="space-y-3">
        <PanelHeader
          title="History"
          description="Tokens per second over time, across llama.cpp versions."
          actions={
            <>
              <Select
                value={test}
                onValueChange={(v) => setTest(v as 'pp' | 'tg' | 'pp+tg')}
                className="w-32"
                options={[
                  { value: 'tg', label: 'Generation (tg)' },
                  { value: 'pp', label: 'Prompt (pp)' },
                  { value: 'pp+tg', label: 'Combined' },
                ]}
              />
              <Select
                value={group}
                onValueChange={(v) => setGroup(v as BenchSeriesGroup)}
                className="w-44"
                options={GROUP_OPTIONS}
              />
            </>
          }
        />
        <UplotChart
          x={chart.x}
          series={chart.series}
          xIsTime
          yFormatter={(v) => formatTokensPerSecond(v)}
          empty={
            search.model_id
              ? 'No results for this model and test yet.'
              : 'Pick a model above to see its history.'
          }
        />
      </Panel>
    </div>
  );
}
