/**
 * Timestamps.
 *
 * The wire form is "RFC 3339 UTC strings" (DESIGN section 3). They are displayed in the viewer's
 * local zone, because the person reading a log line is on the same LAN as the host and thinks in
 * local time — and every rendering is paired with `absoluteTimestamp()` in a `title`, so the exact
 * instant is one hover away and nothing is lost.
 */

const RELATIVE = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

const DATE_TIME = new Intl.DateTimeFormat(undefined, {
  dateStyle: 'medium',
  timeStyle: 'medium',
});

const TIME_ONLY = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
});

const DATE_ONLY = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' });

export function parseTimestamp(value: string | null | undefined): Date | null {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

/** Full local date and time: "30 Aug 2026, 14:21:07". */
export function formatTimestamp(value: string | null | undefined, placeholder = '—'): string {
  const date = parseTimestamp(value);
  return date ? DATE_TIME.format(date) : placeholder;
}

/** "14:21:07" — for log lines and anything already grouped under a date. */
export function formatTime(value: string | null | undefined, placeholder = '—'): string {
  const date = parseTimestamp(value);
  return date ? TIME_ONLY.format(date) : placeholder;
}

/** "30 Aug 2026" — for daily accounting rows. */
export function formatDate(value: string | null | undefined, placeholder = '—'): string {
  const date = parseTimestamp(value);
  return date ? DATE_ONLY.format(date) : placeholder;
}

/** The unambiguous form, for `title` attributes: the RFC 3339 UTC string the daemon sent. */
export function absoluteTimestamp(value: string | null | undefined): string | undefined {
  const date = parseTimestamp(value);
  return date ? date.toISOString() : undefined;
}

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 60 * 60 * 1000],
  ['month', 30 * 24 * 60 * 60 * 1000],
  ['day', 24 * 60 * 60 * 1000],
  ['hour', 60 * 60 * 1000],
  ['minute', 60 * 1000],
  ['second', 1000],
];

/**
 * "3 minutes ago", "in 2 hours". `now` is injectable so a test is not a race against the clock.
 */
export function formatRelative(
  value: string | null | undefined,
  now: Date = new Date(),
  placeholder = '—',
): string {
  const date = parseTimestamp(value);
  if (!date) return placeholder;
  const delta = date.getTime() - now.getTime();
  const magnitude = Math.abs(delta);
  if (magnitude < 5000) return 'just now';
  for (const [unit, ms] of UNITS) {
    if (magnitude >= ms) return RELATIVE.format(Math.round(delta / ms), unit);
  }
  return RELATIVE.format(Math.round(delta / 1000), 'second');
}

/** Milliseconds between two RFC 3339 strings — a start row's duration, say. Null if either is absent. */
export function durationBetween(
  from: string | null | undefined,
  to: string | null | undefined,
): number | null {
  const a = parseTimestamp(from);
  const b = parseTimestamp(to);
  if (!a || !b) return null;
  return b.getTime() - a.getTime();
}
