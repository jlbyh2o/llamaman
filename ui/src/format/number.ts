/**
 * Numbers that are not bytes and not durations: counts, percentages, throughput, and the
 * hash/port/flag strings that get monospace treatment.
 */

const DECIMAL = new Intl.NumberFormat(undefined);

export function formatCount(value: number | null | undefined, placeholder = '—'): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return placeholder;
  return DECIMAL.format(value);
}

/** "68%" — or "68.4%" when a decimal is asked for. Input is a fraction unless `alreadyPercent`. */
export function formatPercent(
  value: number | null | undefined,
  options: { decimals?: number; alreadyPercent?: boolean; placeholder?: string } = {},
): string {
  const placeholder = options.placeholder ?? '—';
  if (value === null || value === undefined || !Number.isFinite(value)) return placeholder;
  const percent = options.alreadyPercent ? value : value * 100;
  return `${percent.toFixed(options.decimals ?? 0)}%`;
}

/** Tokens per second, the metric every bench chart and instance card reports. */
export function formatTokensPerSecond(value: number | null | undefined, placeholder = '—'): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return placeholder;
  const decimals = value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(decimals)} tok/s`;
}

/** "1234 ± 12" — a bench point with its standard deviation. */
export function formatWithStddev(
  value: number | null | undefined,
  stddev: number | null | undefined,
  decimals = 2,
): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—';
  if (stddev === null || stddev === undefined || !Number.isFinite(stddev)) {
    return value.toFixed(decimals);
  }
  return `${value.toFixed(decimals)} ± ${stddev.toFixed(decimals)}`;
}

/** First twelve characters of a hash, which is enough to compare two by eye. */
export function shortHash(value: string | null | undefined, length = 12): string {
  if (!value) return '—';
  return value.length <= length ? value : value.slice(0, length);
}

/** `01J8Z…` ULIDs and config hashes are long; this is the middle-elided form for a narrow column. */
export function elide(value: string | null | undefined, head = 8, tail = 4): string {
  if (!value) return '—';
  if (value.length <= head + tail + 1) return value;
  return `${value.slice(0, head)}…${value.slice(-tail)}`;
}
