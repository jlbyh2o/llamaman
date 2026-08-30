/**
 * Byte counts.
 *
 * The wire form is "plain JSON numbers of bytes" (DESIGN section 3). Binary units are the default
 * because everything this app displays sizes for — GGUF files, VRAM, the fit calculator's terms —
 * is measured against them upstream, and a "7.2 GB" that is really 6.7 GiB is exactly the confusion
 * a fit verdict cannot afford.
 */

const IEC = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const;
const SI = ['B', 'kB', 'MB', 'GB', 'TB', 'PB'] as const;

export interface ByteFormatOptions {
  /** IEC (1024) by default; SI (1000) for anything quoted from a vendor spec sheet. */
  si?: boolean;
  /** Significant decimals. Default: 0 below MiB, 1 at MiB, 2 from GiB up. */
  precision?: number;
  /** Render "—" rather than "0 B" for null/undefined. */
  placeholder?: string;
}

export function formatBytes(
  bytes: number | null | undefined,
  options: ByteFormatOptions = {},
): string {
  const placeholder = options.placeholder ?? '—';
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return placeholder;

  const base = options.si ? 1000 : 1024;
  const units = options.si ? SI : IEC;
  const negative = bytes < 0;
  let value = Math.abs(bytes);
  let unit = 0;
  while (value >= base && unit < units.length - 1) {
    value /= base;
    unit += 1;
  }

  const precision = options.precision ?? (unit <= 1 ? 0 : unit === 2 ? 1 : 2);
  const rendered = value.toFixed(precision);
  return `${negative ? '-' : ''}${rendered} ${units[unit]}`;
}

/** Transfer rate, for the download queue's speed column. */
export function formatBytesPerSecond(
  bytesPerSecond: number | null | undefined,
  options: ByteFormatOptions = {},
): string {
  if (bytesPerSecond === null || bytesPerSecond === undefined || !Number.isFinite(bytesPerSecond)) {
    return options.placeholder ?? '—';
  }
  return `${formatBytes(bytesPerSecond, options)}/s`;
}

/**
 * "4.2 GiB of 7.1 GiB". Both sides are rendered in the unit of the larger, so a progress line does
 * not jump from MiB to GiB as it fills.
 */
export function formatByteProgress(
  done: number,
  total: number | null | undefined,
  options: ByteFormatOptions = {},
): string {
  if (total === null || total === undefined || total <= 0) return formatBytes(done, options);
  const base = options.si ? 1000 : 1024;
  const units = options.si ? SI : IEC;
  let unit = 0;
  let scale = 1;
  while (total / scale >= base && unit < units.length - 1) {
    scale *= base;
    unit += 1;
  }
  const precision = options.precision ?? (unit <= 1 ? 0 : unit === 2 ? 1 : 2);
  return `${(done / scale).toFixed(precision)} of ${(total / scale).toFixed(precision)} ${units[unit]}`;
}
