/**
 * The per-instance usage sparkline.
 *
 * Hand-rolled SVG rather than uPlot, on the same reasoning D44 gives for the comparison bars: uPlot
 * earns its place where a sweep puts thousands of points on a canvas, and a fourteen-point daily
 * series is a shape, not a chart. Keeping it as markup also means it renders in a server-rendered
 * test and colors itself from the token palette like everything else.
 *
 * The series is `instance_usage_daily` — the gateway's own ledger, which counts **every** proxied
 * request including `auth_mode='none'` (D56). That is why a spike here can exist with no token in
 * sight, and why the empty state says "no traffic yet" rather than "no data".
 */

import { useId } from 'react';
import { formatCount } from '../../../format';

export interface SparklinePoint {
  label: string;
  value: number;
}

export interface UsageSparklineProps {
  points: readonly SparklinePoint[];
  /** What the values are: "requests", "errors". Used in the accessible summary. */
  unit?: string;
  height?: number;
  className?: string;
  tone?: 'accent' | 'danger';
}

/** The polyline for a series, scaled into a 100 × `height` box. Exported so a test can read it. */
export function sparklinePath(values: readonly number[], height: number): string {
  if (values.length === 0) return '';
  const max = Math.max(...values, 1);
  const step = values.length === 1 ? 0 : 100 / (values.length - 1);
  return values
    .map((value, index) => {
      const x = values.length === 1 ? 50 : index * step;
      const y = height - (value / max) * height;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(' ');
}

export function UsageSparkline({
  points,
  unit = 'requests',
  height = 40,
  className,
  tone = 'accent',
}: UsageSparklineProps) {
  const titleId = useId();
  const values = points.map((point) => point.value);
  const total = values.reduce((sum, value) => sum + value, 0);
  const stroke = tone === 'danger' ? 'var(--lm-danger)' : 'var(--lm-accent)';

  if (points.length === 0 || total === 0) {
    return (
      <p className={className}>
        <span className="text-xs text-[var(--lm-text-faint)]">No traffic yet.</span>
      </p>
    );
  }

  const path = sparklinePath(values, height);
  const last = points[points.length - 1];
  // One string: React refuses an array of children inside <title>, and a screen reader would read
  // the fragments as separate nodes anyway.
  const summary =
    `${formatCount(total)} ${unit} over ${points.length} days, ` +
    `ending ${formatCount(last?.value ?? 0)} on ${last?.label ?? ''}`;

  return (
    <figure className={className}>
      <svg
        viewBox={`0 0 100 ${height}`}
        preserveAspectRatio="none"
        role="img"
        aria-labelledby={titleId}
        className="h-10 w-full"
      >
        <title id={titleId}>{summary}</title>
        <polyline
          points={path}
          fill="none"
          stroke={stroke}
          strokeWidth={1.5}
          vectorEffect="non-scaling-stroke"
          strokeLinejoin="round"
          strokeLinecap="round"
        />
      </svg>
      <figcaption className="lm-numeric mt-1 flex items-baseline justify-between text-[11px] text-[var(--lm-text-faint)]">
        <span>{points[0]?.label}</span>
        <span className="text-[var(--lm-text-muted)]">
          {formatCount(total)} {unit}
        </span>
        <span>{last?.label}</span>
      </figcaption>
    </figure>
  );
}
