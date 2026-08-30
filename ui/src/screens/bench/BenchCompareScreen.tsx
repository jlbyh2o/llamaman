/**
 * `/bench/compare` — side-by-side comparison, driven entirely by URL params (DESIGN section 4,
 * screen 13: "the selection lives entirely in the URL, because a comparison is a thing people send
 * each other").
 *
 * `POST /bench/compare` is a read dressed as a mutation (it computes a grouped aggregate over
 * `bench_points ⋈ bench_results`, writes nothing), so `useBenchCompare` wraps it as a query keyed by
 * its own inputs rather than firing it as an action.
 */

import { useMemo } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { X } from 'lucide-react';
import type { Column } from '../../components';
import { Badge, DataTable, EmptyState, Panel, PanelHeader, Select } from '../../components';
import { formatCount } from '../../format';
import { alignCategorySeries } from '../../features/bench/chartData';
import { UplotChart } from '../../features/bench/UplotChart';
import type { BenchSeriesGroup } from '../../features/bench/hooks';
import { useBenchCompare, useBenchRuns } from '../../features/bench/hooks';
import type { BenchCompare } from '../../api/types';

type ComparePoint = BenchCompare['points'][number];

const AXIS_OPTIONS: { value: BenchSeriesGroup; label: string }[] = [
  { value: 'n_gpu_layers', label: 'GPU layers' },
  { value: 'n_batch', label: 'Batch size' },
  { value: 'n_ubatch', label: 'Micro-batch size' },
  { value: 'n_threads', label: 'Threads' },
  { value: 'flash_attn', label: 'Flash attention' },
  { value: 'type_k', label: 'Cache type (K)' },
  { value: 'type_v', label: 'Cache type (V)' },
  { value: 'split_mode', label: 'Split mode' },
  { value: 'tensor_split', label: 'Tensor split' },
  { value: 'test_kind', label: 'Test kind' },
  { value: 'llamacpp_tag', label: 'llama.cpp version' },
  { value: 'quant_label', label: 'Quantization' },
  { value: 'run_name', label: 'Run' },
];

const METRIC_OPTIONS = [
  { value: 'avg_ts', label: 'Tokens/sec (avg)' },
  { value: 'stddev_ts', label: 'Tokens/sec (stddev)' },
  { value: 'avg_ns', label: 'Time per test (avg ns)' },
  { value: 'stddev_ns', label: 'Time per test (stddev ns)' },
];

export function BenchCompareScreen() {
  const search = useSearch({ from: '/app/bench/compare' });
  const navigate = useNavigate({ from: '/bench/compare' });

  const runIds = search.runs ?? [];
  const x = search.x ?? 'n_gpu_layers';
  const y = search.y ?? 'avg_ts';
  const series = search.series ?? 'run_name';

  const allRuns = useBenchRuns({ limit: 100 });
  const selectedRuns = useMemo(
    () => (allRuns.data?.items ?? []).filter((r) => runIds.includes(r.id)),
    [allRuns.data, runIds],
  );

  const compare = useBenchCompare(runIds.length > 0 ? { runIds, x, y, series } : undefined);

  const chart = useMemo(() => alignCategorySeries(compare.data?.points ?? []), [compare.data]);

  function removeRun(id: string) {
    void navigate({
      search: (prev) => ({ ...prev, runs: runIds.filter((r) => r !== id) }),
    });
  }

  const columns: Column<ComparePoint>[] = [
    {
      id: 'x',
      header: AXIS_OPTIONS.find((a) => a.value === x)?.label ?? x,
      cell: (r) => r.x,
      mono: true,
    },
    { id: 'series', header: 'Series', cell: (r) => r.series },
    { id: 'value', header: 'Value', align: 'right', mono: true, cell: (r) => r.value.toFixed(2) },
    { id: 'samples', header: 'n', align: 'right', mono: true, cell: (r) => formatCount(r.samples) },
  ];

  return (
    <div className="space-y-6 p-6">
      <PanelHeader
        level={1}
        title="Compare benchmarks"
        description="Pick two or more runs and an axis to plot them against."
      />

      <Panel className="space-y-3">
        <h2 className="text-sm font-semibold text-[var(--lm-text)]">Runs</h2>
        {selectedRuns.length === 0 ? (
          <p className="text-xs text-[var(--lm-text-muted)]">
            No runs selected — pick some from the benchmark history table and use "Compare".
          </p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {selectedRuns.map((run) => (
              <Badge key={run.id} tone="accent" className="pr-1">
                {run.name}
                <button
                  type="button"
                  onClick={() => removeRun(run.id)}
                  aria-label={`Remove ${run.name} from comparison`}
                  className="ml-1 rounded-full p-0.5 hover:bg-[var(--lm-neutral-soft)]"
                >
                  <X aria-hidden className="size-3" />
                </button>
              </Badge>
            ))}
          </div>
        )}

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          <label className="space-y-1 text-xs text-[var(--lm-text-muted)]">
            <span>X axis</span>
            <Select
              value={x}
              onValueChange={(v) => void navigate({ search: (prev) => ({ ...prev, x: v }) })}
              options={AXIS_OPTIONS}
            />
          </label>
          <label className="space-y-1 text-xs text-[var(--lm-text-muted)]">
            <span>Y axis (metric)</span>
            <Select
              value={y}
              onValueChange={(v) => void navigate({ search: (prev) => ({ ...prev, y: v }) })}
              options={METRIC_OPTIONS}
            />
          </label>
          <label className="space-y-1 text-xs text-[var(--lm-text-muted)]">
            <span>Series</span>
            <Select
              value={series}
              onValueChange={(v) => void navigate({ search: (prev) => ({ ...prev, series: v }) })}
              options={AXIS_OPTIONS}
            />
          </label>
        </div>
      </Panel>

      <Panel>
        {runIds.length === 0 ? (
          <EmptyState
            title="Select runs to compare"
            description="Head back to Benchmarks, check a few runs, and choose Compare."
            dense
          />
        ) : (
          <UplotChart
            x={chart.x}
            series={chart.series}
            xLabels={chart.xLabels}
            yFormatter={(v) => v.toFixed(y.includes('ns') ? 0 : 1)}
          />
        )}
      </Panel>

      {compare.data && compare.data.points.length > 0 ? (
        <Panel flush>
          <DataTable
            columns={columns}
            rows={compare.data.points}
            rowKey={(row) => `${row.x}:${row.series}`}
          />
        </Panel>
      ) : null}
    </div>
  );
}
