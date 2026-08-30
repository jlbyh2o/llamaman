/**
 * Turning `bench/series` and `bench/compare` rows into `UplotChart` input.
 *
 * Both endpoints answer with one flat row per (series, x) pair rather than an aligned matrix — which
 * is right for the wire (it says nothing about points that do not exist) but wrong for a chart, which
 * needs one shared x domain and a same-length value array per series, with a hole wherever that
 * series has no point. These two functions are that alignment, one for each axis shape the two
 * endpoints use: `bench/series` is always a timestamp; `bench/compare`'s `x` is whatever sweep axis
 * was picked and is usually categorical, so its domain is sorted numerically when every label happens
 * to parse as a number (n_gpu_layers, n_batch, …) and left in first-seen order otherwise.
 */

export interface TimePoint {
  at: string;
  group: string;
  value: number;
}

export interface AlignedSeries {
  x: number[];
  series: { label: string; values: (number | null)[] }[];
}

/** `bench/series`: x is always time. One series per distinct `group` value. */
export function alignTimeSeries(points: readonly TimePoint[]): AlignedSeries {
  const xs = [...new Set(points.map((p) => Math.floor(new Date(p.at).getTime() / 1000)))].sort(
    (a, b) => a - b,
  );
  const groups = [...new Set(points.map((p) => p.group))];
  const byGroup = new Map<string, Map<number, number>>(groups.map((g) => [g, new Map()]));
  for (const p of points) {
    byGroup.get(p.group)?.set(Math.floor(new Date(p.at).getTime() / 1000), p.value);
  }
  return {
    x: xs,
    series: groups.map((label) => ({
      label,
      values: xs.map((x) => byGroup.get(label)?.get(x) ?? null),
    })),
  };
}

export interface CategoryPoint {
  x: string;
  series: string;
  value: number;
}

export interface AlignedCategorySeries {
  /** Index positions 0..n-1 — what `UplotChart` plots against. */
  x: number[];
  xLabels: string[];
  series: { label: string; values: (number | null)[] }[];
}

/** `bench/compare`: x is a sweep axis, usually categorical — sorted numerically when it parses as one. */
export function alignCategorySeries(points: readonly CategoryPoint[]): AlignedCategorySeries {
  const labels = [...new Set(points.map((p) => p.x))];
  const numeric = labels.length > 0 && labels.every((v) => v !== '' && Number.isFinite(Number(v)));
  const xLabels = numeric ? [...labels].sort((a, b) => Number(a) - Number(b)) : labels;

  const seriesNames = [...new Set(points.map((p) => p.series))];
  const byName = new Map<string, Map<string, number>>(seriesNames.map((s) => [s, new Map()]));
  for (const p of points) byName.get(p.series)?.set(p.x, p.value);

  return {
    x: xLabels.map((_, i) => i),
    xLabels,
    series: seriesNames.map((label) => ({
      label,
      values: xLabels.map((x) => byName.get(label)?.get(x) ?? null),
    })),
  };
}
