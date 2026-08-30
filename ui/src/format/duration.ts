/**
 * Durations.
 *
 * The wire form is "integers with a `_ms` suffix" (DESIGN section 3), so every input here is
 * milliseconds unless the name says otherwise. Two renderings, because two things are being said:
 * `formatDuration` is a measurement ("4.21 s"), `formatElapsed` is a clock ("2h 04m").
 */

const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

export interface DurationOptions {
  placeholder?: string;
}

/**
 * A measured span, with the precision the magnitude deserves: sub-second in ms, seconds with two
 * decimals, then the coarse clock form. Used for build steps, job durations and bench timings.
 */
export function formatDuration(
  ms: number | null | undefined,
  options: DurationOptions = {},
): string {
  const placeholder = options.placeholder ?? '—';
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return placeholder;
  const value = Math.abs(ms);
  const sign = ms < 0 ? '-' : '';

  if (value < SECOND) return `${sign}${Math.round(value)} ms`;
  if (value < 10 * SECOND) return `${sign}${(value / SECOND).toFixed(2)} s`;
  if (value < MINUTE) return `${sign}${(value / SECOND).toFixed(1)} s`;
  return sign + formatElapsed(value);
}

/**
 * A clock-shaped span: `45s`, `2m 05s`, `1h 12m`, `3d 4h`. Two units at most — the third never
 * changes a decision.
 */
export function formatElapsed(
  ms: number | null | undefined,
  options: DurationOptions = {},
): string {
  const placeholder = options.placeholder ?? '—';
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return placeholder;
  const value = Math.max(0, Math.round(ms));

  if (value >= DAY) {
    const days = Math.floor(value / DAY);
    const hours = Math.floor((value % DAY) / HOUR);
    return hours ? `${days}d ${hours}h` : `${days}d`;
  }
  if (value >= HOUR) {
    const hours = Math.floor(value / HOUR);
    const minutes = Math.floor((value % HOUR) / MINUTE);
    return minutes ? `${hours}h ${String(minutes).padStart(2, '0')}m` : `${hours}h`;
  }
  if (value >= MINUTE) {
    const minutes = Math.floor(value / MINUTE);
    const seconds = Math.floor((value % MINUTE) / SECOND);
    return seconds ? `${minutes}m ${String(seconds).padStart(2, '0')}s` : `${minutes}m`;
  }
  return `${Math.floor(value / SECOND)}s`;
}

/** Seconds in, clock form out — for the `retry_after_sec` and `eta_sec` fields. */
export function formatSeconds(
  seconds: number | null | undefined,
  options: DurationOptions = {},
): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return options.placeholder ?? '—';
  }
  return formatElapsed(seconds * SECOND, options);
}

/** "about 9 minutes" — the build and sweep estimates, which are honest about being estimates. */
export function formatEstimate(minutes: number | null | undefined): string {
  if (minutes === null || minutes === undefined || !Number.isFinite(minutes)) return 'unknown';
  if (minutes < 1) return 'under a minute';
  if (minutes < 90)
    return `about ${Math.round(minutes)} minute${Math.round(minutes) === 1 ? '' : 's'}`;
  const hours = minutes / 60;
  return `about ${hours.toFixed(1)} hours`;
}
