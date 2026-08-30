import { describe, expect, it } from 'vitest';
import { formatByteProgress, formatBytes, formatBytesPerSecond } from './bytes';
import { durationBetween, formatRelative, formatTimestamp, parseTimestamp } from './datetime';
import { formatDuration, formatElapsed, formatEstimate, formatSeconds } from './duration';
import { elide, formatPercent, formatTokensPerSecond, formatWithStddev, shortHash } from './number';

describe('bytes', () => {
  it('uses binary units, because that is what GGUF and VRAM are measured in', () => {
    expect(formatBytes(1024)).toBe('1 KiB');
    expect(formatBytes(1024 ** 3 * 4.92)).toBe('4.92 GiB');
    expect(formatBytes(0)).toBe('0 B');
  });

  it('offers SI for numbers quoted from a spec sheet', () => {
    expect(formatBytes(1000, { si: true })).toBe('1 kB');
  });

  it('renders a placeholder rather than a fake zero', () => {
    expect(formatBytes(null)).toBe('—');
    expect(formatBytes(undefined)).toBe('—');
    expect(formatBytes(Number.NaN)).toBe('—');
  });

  it('keeps both sides of a progress pair in the same unit', () => {
    // 512 MiB of 8 GiB must not read "512 MiB of 8 GiB" with a jumping unit as it fills.
    expect(formatByteProgress(512 * 1024 ** 2, 8 * 1024 ** 3)).toBe('0.50 of 8.00 GiB');
  });

  it('renders a rate', () => {
    expect(formatBytesPerSecond(1024 ** 2 * 12)).toBe('12.0 MiB/s');
  });
});

describe('durations', () => {
  it('picks the precision the magnitude deserves', () => {
    expect(formatDuration(420)).toBe('420 ms');
    expect(formatDuration(4210)).toBe('4.21 s');
    expect(formatDuration(42_100)).toBe('42.1 s');
    expect(formatDuration(125_000)).toBe('2m 05s');
  });

  it('renders a clock span with at most two units', () => {
    expect(formatElapsed(45_000)).toBe('45s');
    expect(formatElapsed(3_600_000)).toBe('1h');
    expect(formatElapsed(4_320_000)).toBe('1h 12m');
    expect(formatElapsed(280_800_000)).toBe('3d 6h');
  });

  it('takes seconds where the API sends seconds', () => {
    expect(formatSeconds(90)).toBe('1m 30s');
  });

  it('hedges an estimate rather than pretending to precision', () => {
    expect(formatEstimate(9)).toBe('about 9 minutes');
    expect(formatEstimate(1)).toBe('about 1 minute');
    expect(formatEstimate(0.4)).toBe('under a minute');
    expect(formatEstimate(null)).toBe('unknown');
  });
});

describe('timestamps', () => {
  const now = new Date('2026-08-30T12:00:00Z');

  it('parses the RFC 3339 wire form', () => {
    expect(parseTimestamp('2026-08-30T11:59:00Z')?.toISOString()).toBe('2026-08-30T11:59:00.000Z');
    expect(parseTimestamp('not a date')).toBeNull();
    expect(parseTimestamp(null)).toBeNull();
  });

  it('reports relative time in both directions', () => {
    expect(formatRelative('2026-08-30T11:57:00Z', now)).toContain('3 minutes');
    expect(formatRelative('2026-08-30T14:00:00Z', now)).toContain('2 hours');
    expect(formatRelative('2026-08-30T12:00:01Z', now)).toBe('just now');
  });

  it('falls back to a placeholder rather than "Invalid Date"', () => {
    expect(formatTimestamp(null)).toBe('—');
    expect(formatRelative(undefined, now)).toBe('—');
  });

  it('measures a span between two wire timestamps', () => {
    expect(durationBetween('2026-08-30T12:00:00Z', '2026-08-30T12:01:30Z')).toBe(90_000);
    expect(durationBetween('2026-08-30T12:00:00Z', null)).toBeNull();
  });
});

describe('numbers', () => {
  it('scales throughput precision to the magnitude', () => {
    expect(formatTokensPerSecond(4.128)).toBe('4.13 tok/s');
    expect(formatTokensPerSecond(41.28)).toBe('41.3 tok/s');
    expect(formatTokensPerSecond(412.8)).toBe('413 tok/s');
  });

  it('renders a bench point with its stddev', () => {
    expect(formatWithStddev(1234.5, 12.25)).toBe('1234.50 ± 12.25');
    expect(formatWithStddev(1234.5, null)).toBe('1234.50');
  });

  it('formats percentages from a fraction or a percent', () => {
    expect(formatPercent(0.684)).toBe('68%');
    expect(formatPercent(68.4, { alreadyPercent: true, decimals: 1 })).toBe('68.4%');
  });

  it('shortens identifiers without hiding that it did', () => {
    expect(shortHash('0123456789abcdef0123')).toBe('0123456789ab');
    expect(elide('01J8ZQ4K7T9WX2', 4, 3)).toBe('01J8…WX2');
    expect(elide('short')).toBe('short');
    expect(elide(null)).toBe('—');
  });
});
