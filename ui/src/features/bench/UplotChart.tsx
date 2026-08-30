/**
 * The one chart component bench screens share.
 *
 * uPlot draws to a `<canvas>`, so nothing here can be styled by CSS the way the rest of the kit is —
 * strokes, grid lines and text are literal colors, read from the `--lm-chart-*` and surface tokens at
 * *build* time (DESIGN section 4, D44: "uPlot … Canvas rendering handles the thousands of points a
 * 512-point sweep produces"). That is what "uPlot integration must handle theme switching" means in
 * practice: the chart is torn down and rebuilt whenever the resolved theme changes, which is cheap —
 * uPlot's own construction cost is what the library is chosen for — and correct, because a canvas
 * never receives a `prefers-color-scheme` change on its own.
 *
 * Series are colored from `--lm-chart-1..6` in a **fixed order** (first series always gets chart-1,
 * never reassigned as other series appear or disappear), which is what keeps a color meaning the same
 * thing across a filter change.
 */

import { useEffect, useRef, useState } from 'react';
// uplot's typings are CommonJS (`export = uPlot`); without `esModuleInterop` a namespace import is
// what TypeScript accepts for the *types*, but it erases the constructor's call signature, so the
// class itself is recovered with one local cast rather than by changing a tsconfig this area does
// not own.
import * as uPlot from 'uplot';
import './chart.css';
import { resolveTheme, useThemeStore } from '../../theme/useTheme';

type UplotInstance = { destroy(): void; setSize(size: { width: number; height: number }): void };
type UplotCtor = new (
  opts: uPlot.Options,
  data: uPlot.AlignedData,
  target: HTMLElement,
) => UplotInstance;
const Uplot = uPlot as unknown as UplotCtor;

/** Re-renders when the *resolved* theme changes, including a system-preference flip mid-session. */
function useResolvedTheme(): 'dark' | 'light' {
  const preference = useThemeStore((state) => state.preference);
  const [resolved, setResolved] = useState(() => resolveTheme(preference));

  useEffect(() => {
    setResolved(resolveTheme(preference));
    if (preference !== 'system' || typeof window === 'undefined' || !window.matchMedia) return;
    const media = window.matchMedia('(prefers-color-scheme: light)');
    const onChange = () => setResolved(resolveTheme(preference));
    media.addEventListener('change', onChange);
    return () => media.removeEventListener('change', onChange);
  }, [preference]);

  return resolved;
}

function readTokens() {
  const styles = getComputedStyle(document.documentElement);
  const read = (name: string) => styles.getPropertyValue(name).trim();
  return {
    chart: [1, 2, 3, 4, 5, 6].map((n) => read(`--lm-chart-${n}`)),
    border: read('--lm-border'),
    textFaint: read('--lm-text-faint'),
    textMuted: read('--lm-text-muted'),
  };
}

export interface ChartSeriesData {
  label: string;
  values: (number | null)[];
}

export interface UplotChartProps {
  /** The shared x axis: unix seconds when `xIsTime`, otherwise an arbitrary numeric position. */
  x: number[];
  series: ChartSeriesData[];
  xIsTime?: boolean;
  /** Tick labels for a categorical x axis — same length as `x`, index-matched. Ignored when `xIsTime`. */
  xLabels?: string[];
  yFormatter?: (value: number) => string;
  height?: number;
  className?: string;
  /** Shown centered over the plot area in place of a chart, for "no data yet". */
  empty?: string;
}

export function UplotChart({
  x,
  series,
  xIsTime = false,
  xLabels,
  yFormatter,
  height = 260,
  className,
  empty,
}: UplotChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const theme = useResolvedTheme();

  const seriesKey = series.map((s) => s.label).join('|');
  const dataKey = `${x.join(',')}::${series.map((s) => s.values.join(',')).join('|')}`;

  useEffect(() => {
    const el = containerRef.current;
    if (!el || x.length === 0 || series.length === 0) return undefined;

    const tokens = readTokens();

    const uSeries: uPlot.Series[] = [
      { label: xIsTime ? 'Time' : 'x' },
      ...series.map((s, i) => ({
        label: s.label,
        stroke: tokens.chart[i % tokens.chart.length],
        width: 2,
        points: { show: x.length <= 60, size: 5 },
      })),
    ];

    const xAxis: uPlot.Axis = {
      stroke: tokens.textFaint,
      grid: { stroke: tokens.border, width: 1 },
      ticks: { stroke: tokens.border },
      ...(xIsTime
        ? {}
        : xLabels
          ? {
              values: (_u: uPlot, splits: number[]) =>
                splits.map((v) => xLabels[Math.round(v)] ?? ''),
            }
          : {}),
    };

    const yAxis: uPlot.Axis = {
      stroke: tokens.textFaint,
      grid: { stroke: tokens.border, width: 1 },
      ticks: { stroke: tokens.border },
      ...(yFormatter
        ? {
            values: (_u: uPlot, splits: number[]) => splits.map((v) => yFormatter(v)),
          }
        : {}),
    };

    const opts: uPlot.Options = {
      width: el.clientWidth || 600,
      height,
      series: uSeries,
      legend: { show: series.length > 0 },
      cursor: { points: { size: 6 } },
      scales: { x: { time: xIsTime } },
      axes: [xAxis, yAxis],
    };

    const data = [x, ...series.map((s) => s.values)] as uPlot.AlignedData;
    const plot = new Uplot(opts, data, el);

    const resize = new ResizeObserver(() => {
      const width = el.clientWidth;
      if (width > 0) plot.setSize({ width, height });
    });
    resize.observe(el);

    return () => {
      resize.disconnect();
      plot.destroy();
    };
    // `dataKey`/`seriesKey` stand in for `x`/`series`, which are new array identities on every
    // render; rebuilding on the theme is the whole point of this effect (see the module doc).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dataKey, seriesKey, xIsTime, height, theme]);

  if (x.length === 0 || series.length === 0) {
    return (
      <div
        className="flex items-center justify-center rounded-[var(--lm-radius)] border border-dashed border-[var(--lm-border)] text-xs text-[var(--lm-text-faint)]"
        style={{ height }}
      >
        {empty ?? 'No data to chart yet.'}
      </div>
    );
  }

  return <div ref={containerRef} className={`lm-chart w-full ${className ?? ''}`} />;
}
